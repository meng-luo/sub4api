# Sub4API OpenAI Transport（自研出站插件）

替代原先计划由闭源厂商包（`local.sub2api.openai-transport`）承担的传输层职责：
宿主继续负责协议、鉴权、SSE、计费；本插件只负责**用与官方 Codex 客户端一致的
TLS/HTTP2 行为把请求发出去**，并把原始响应流式回传。

```
backend/pluginimpl/openaitransport/     实现（Go）
backend/cmd/openai-transport-plugin/    进程入口
backend/pluginimpl/openaitransport/package/  打包、签名、配置 UI
```

## 1. 为什么要自研

* 仓库从未分发闭源包，宿主却内置了它的发布者公钥（固定信任根）——等于给一个不存在的产物留后门。该内置公钥已删除，信任只来自 `plugins.trusted_publishers`。
* 没有插件时，Codex OAuth 流量 100% 走 Go 的 `crypto/tls` + `x/net/http2`，与真机 Rust（rustls/hyper）指纹不同，属于可被上游一票打标的差异。

## 2. 实现要点

| 关注点 | 做法 |
|---|---|
| TLS | `utls` + 自建 `rustlsClientHelloSpec`：9 条套件（TLS1.3 三条在前）、无 GREASE、ALPN `h2,http/1.1`、曲线 `x25519/secp256r1/secp384r1` |
| HTTP/2 | 单一 `http.Transport` + `Protocols` 显式启用；`HTTP2.SendPingTimeout=0`（不主动发健康 PING，与 hyper 一致）、`MaxReadFrameSize=16384` |
| HTTP/1.1 | `http2=false` 时只启用 HTTP/1.1（对照排查用） |
| 压缩 | 默认 `DisableCompression=true` 且不写 `Content-Encoding`（官方 0.171.0 实测不压缩，226 KB 也不压） |
| 代理 | HTTP/HTTPS CONNECT、SOCKS5/SOCKS5H；按 `proxy_url` 隔离连接池 |
| 流式 | 请求体按帧写入 `io.Pipe`，响应体按 32 KiB 分帧回传，不攒包 |
| 取消 | gRPC stream ctx 透传到 `http.Request`，下游断开即取消上游 |
| 错误 | `ForwardResponseError.request_sent`：一旦调用 `client.Do` 即置 `true`，宿主据此禁止重放 |

## 3. 配置

```json
{
  "tls_profile": "rustls",
  "http2": true,
  "disable_compression": true,
  "body_compression": false,
  "insecure_skip_verify": false,
  "handshake_timeout_seconds": 15,
  "response_header_timeout_seconds": 600,
  "idle_conn_timeout_seconds": 90,
  "max_conns_per_host": 0,
  "max_idle_conns_per_host": 16
}
```

* `tls_profile`：`rustls`（默认，模拟真机）/ `go`（对照）。
* `body_compression`：默认 `false`。**不要打开**，除非确认目标客户端版本确实压缩请求体。
* 未知字段会被 `ValidateConfig` 拒绝（`DisallowUnknownFields`）。

## 4. 构建与安装

```bash
cd backend/pluginimpl/openaitransport/package
./build.sh                                   # 当前平台
./build.sh darwin-arm64 linux-amd64 linux-arm64
SIGN_KEY=~/.sub4api/plugin-signing.key ./build.sh   # 构建 + 签名
```

产物：`dist/sub4api-openai-transport-<version>.s2plugin`。

安装步骤：

1. 把 `sign.py` 打印的 `key_id: "公钥"` 写入部署配置：
   ```yaml
   plugins:
     trusted_publishers:
       publisher-xxxxxxxxxxxxxxxx: "BASE64公钥"
   ```
2. 管理页 → 插件 → 上传 `.s2plugin` → 启用（灰度 100%）。
3. 需要「未命中插件即拒绝」时打开：
   ```yaml
   plugins:
     require_openai_oauth_plugin: true
   ```

离线校验包（不需要装到宿主）：`python3 s2plugin_verify.py <包> --key <key_id>=<公钥>`。

### 4.1 从仓库根构建 / 镜像内置包

```bash
make plugin-package        # 当前平台 → backend/pluginimpl/openaitransport/package/dist/
make plugin-package-all    # darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
```

镜像（`Dockerfile`）在 backend 构建阶段顺带出包，按目标架构打 `linux-<arch>`，
产物落在镜像内 `/app/plugin-packages/`：

```bash
# 未签名包（只能用 plugins.allow_unsigned=true 安装，适合调试）
docker build -t sub2api:dev .

# 已签名包：私钥作为 build secret 传入，并且必须显式 PLUGIN_SIGN=1
# （secret 内容不参与层缓存键，不显式开启会命中未签名旧层）
docker build --build-arg PLUGIN_SIGN=1 \
  --secret id=plugin_signing_key,src="$HOME/.sub4api/plugin-signing.key" -t sub2api:prod .

# 取出镜像内的包后按上面 1~3 步上传
docker create --name sub2api-pkg sub2api:prod && \
  docker cp sub2api-pkg:/app/plugin-packages/sub4api-openai-transport-0.1.0.s2plugin . && \
  docker rm sub2api-pkg
```

`PLUGIN_VERSION` 可用 `--build-arg` 覆盖；`build.sh` 的 `OUT` 环境变量可改产物目录
（镜像里指向 `/app/plugin-dist`），脚本在 Linux 容器内用 `sha256sum`、在 macOS 用
`shasum`，两条路径都能出包。

## 5. 验收标准

```bash
# 1) 单测（含 ClientHello 线序抓取断言）
go test ./pluginimpl/openaitransport/ -v

# 2) 与真机对拍：抓包算 JA3/JA4，要求与本插件一致到字符串
tcpdump -w plugin.pcap host chatgpt.com
# 期望：ext_order = 0000 0017 ff01 000a 000b 000d 0010 0005 0012 0033 002b
#       首套件 0x1302，ALPN [h2 http/1.1]，无 GREASE，无周期性 PING
```

## 6. 已知差距（按优先级）

1. **HTTP/2 SETTINGS 参数**仍由 Go 生成（`INITIAL_WINDOW_SIZE` 等与 hyper 的默认值不同）。要完全一致需要自绘帧层；当前已对齐「不发 PING、MAX_FRAME_SIZE=16384」两项最显眼的行为。
2. **HPACK 表大小/头压缩策略**同样由 Go 决定。
3. **TLS 扩展集合**是 rustls 的近似：`ec_point_formats`、`signed_certificate_timestamp` 是否发送，取决于具体 rustls 版本，需用真机抓包逐版本校准（改 `profile.go` 一处即可）。
4. **WebSocket** 不在本插件能力范围内（宿主对命中插件的 WS 账号走 HTTP Bridge）。
