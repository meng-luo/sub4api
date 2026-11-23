package service

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// codexUpstreamMinVersion 上游 /backend-api/codex 接受的最低 version 头：
// 若请求携带 version 且低于该值，上游直接 404（issue #3901，2026-07 实测）。
const codexUpstreamMinVersion = "0.144.0"

// codexClientVersionMaxLen 官方版本号均为短 ASCII 串，远低于此上限。
const codexClientVersionMaxLen = 64

// codexClientVersionPattern 允许 0.146.0 与 0.147.0-alpha.4 两类官方形态。
var codexClientVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.]+)?$`)

// NormalizeCodexClientVersion 校验并归一化 Codex 客户端版本号，非法值返回空串。
// 该值会被拼进出站 User-Agent 与 version 头，必须拒绝任意字节，避免管理员误填或
// 自动同步拿到异常值时把不可控内容透给上游。
func NormalizeCodexClientVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || len(version) > codexClientVersionMaxLen || !codexClientVersionPattern.MatchString(version) {
		return ""
	}
	return version
}

// codexExecOriginator 是 codex-rs 非交互执行（`codex exec`）的 originator，
// 与 turn metadata 的 turn_trigger="exec" 配套。实测该身份正常通过上游。
const codexExecOriginator = "codex_exec"

// buildCodexCLIUserAgent 按版本号拼出规范 Codex CLI User-Agent。
// UA 形态只在 codexCLIUserAgentPlatform 一处定义，尾部标识组由
// buildCodexUserAgentFor 统一生成，避免多处拼装漂移。
func buildCodexCLIUserAgent(version string) string {
	return buildCodexUserAgentFor(openai.CodexDefaultOriginator, version)
}

// buildCodexUserAgentFor 按 originator 与版本拼出规范 Codex UA：
// `{originator}/{version} ({os}; {arch}) {terminal} ({originator}; {version})`。
//
// 首段、版本段与尾部 `(originator; version)` 组必须同源自洽：上游按 #3901 校验
// originator 与 UA 首段是否配套，而真实客户端从不发出首尾不一致的身份。
// 版本非法或为空时回落到内置常量版本，本函数永不返回空串。
func buildCodexUserAgentFor(originator, version string) string {
	if originator = strings.TrimSpace(originator); originator == "" {
		originator = openai.CodexDefaultOriginator
	}
	if version = NormalizeCodexClientVersion(version); version == "" {
		version = codexCLIVersion
	}
	return openai.WithCodexUATrailer(
		originator+"/"+version+codexCLIUserAgentPlatform,
		originator,
		version,
	)
}

// codexOriginatorForTurnTrigger 由 x-codex-turn-metadata 的 turn_trigger 反查配套 originator。
//
// 真实客户端的 originator 与 turn_trigger 同源：实测逐字节一致的关系是
// `originator: codex_exec` ↔ `"turn_trigger":"exec"`。网关在强制统一身份时若不跟随，
// 就会发出「UA/originator 声称是交互式 TUI、请求元数据声称是非交互 exec」的
// 自相矛盾身份。元数据缺失、非法或非 exec 时返回空串，由调用方沿用规范 CLI 身份。
func codexOriginatorForTurnTrigger(h http.Header) string {
	if strings.EqualFold(codexTurnTriggerFromMetadata(h.Get(openAIWSTurnMetadataHeader)), "exec") {
		return codexExecOriginator
	}
	return ""
}

func codexTurnTriggerFromMetadata(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	metadata, err := decodeCodexTurnMetadata(raw)
	if err != nil || metadata == nil {
		return ""
	}
	trigger, _ := metadata["turn_trigger"].(string)
	return strings.TrimSpace(trigger)
}

// codexIdentityEnforcement 控制 enforceCodexIdentityHeaders 是否强制统一出站身份，
// 由 gateway.disable_codex_identity_enforcement 在服务构造时取反发布。
// 默认开启：上游会按客户端身份进行风控分层，命中风险身份的请求会拿到
// HTTP 200 + 流内 server_is_overloaded，本次请求即失败；强制统一出口可确保没有
// 请求带着第三方或陈旧身份出站。关闭后退回「仅按最终 UA 配对 originator」的收口语义。
var codexIdentityEnforcement = func() *atomic.Bool {
	v := &atomic.Bool{}
	v.Store(true)
	return v
}()

// SetCodexIdentityEnforcementEnabled 发布 Codex 出站身份强制统一开关。
// enforceCodexIdentityHeaders 是所有出站路径共用的纯函数收口点，无法在热路径注入配置，
// 故由持有配置的服务在构造时发布进程级快照。
func SetCodexIdentityEnforcementEnabled(enabled bool) {
	codexIdentityEnforcement.Store(enabled)
}

// codexEmitVersionHeader 控制 ChatGPT 推理面是否发送 version 头。
// 官方 Codex CLI 0.171.0 的 POST /backend-api/codex/responses 出站请求恰好 15 个头，
// 其中没有 version（实测 45 KB ~ 31.7 MB 请求体均不带该头），因此默认不发送。
// 该头只在 issue #3901 的版本门槛场景下被上游校验，由
// gateway.codex_identity.emit_version_header 在上游策略回退时重新打开。
//
// 与 codexIdentityEnforcement 同为进程级快照：ensure/enforce/project 都是纯函数收口点，
// 无法在热路径注入配置，故由持有配置的服务在构造时发布。
var codexEmitVersionHeader = func() *atomic.Bool {
	v := &atomic.Bool{}
	v.Store(false)
	return v
}()

// SetCodexEmitVersionHeader 发布「ChatGPT 推理面是否发送 version 头」的进程级开关。
func SetCodexEmitVersionHeader(enabled bool) {
	codexEmitVersionHeader.Store(enabled)
}

// applyCodexCompactVersionHeader 处理 legacy /responses/compact 的 version 头。
// 该端点不走 projectCodexOutboundHeaders 收口，因此在这里单独对齐业务面的开关：
// 关闭时连客户端透传的值一并削平，打开时按规范身份补齐。
func applyCodexCompactVersionHeader(h http.Header) {
	if !codexEmitVersionHeader.Load() {
		h.Del("version")
		return
	}
	if strings.TrimSpace(h.Get("version")) == "" {
		h.Set("version", CodexCanonicalClientVersion())
	}
}

// setCodexVersionHeader 按开关写入 version 头。关闭时这里不写、也不改写客户端
// 自报的值——ChatGPT 业务 HTTP 路径的削平统一由 projectCodexOutboundHeaders 负责。
func setCodexVersionHeader(h http.Header, version string) {
	if !codexEmitVersionHeader.Load() {
		return
	}
	h.Set("version", version)
}

// codexCanonicalUserAgentResolver 返回当前生效的规范 Codex User-Agent（后台设置 / 自动同步版本号）。
// 由 SettingService 在装配时注入；解析器内部自带 TTL 缓存，热路径不触库。
type codexCanonicalUserAgentResolver func() string

var (
	codexCanonicalUAMu       sync.RWMutex
	codexCanonicalUAResolver codexCanonicalUserAgentResolver
)

// SetCodexCanonicalUserAgentResolver 注入规范 User-Agent 解析器。
// 未注入或解析结果非法时回退到编译期常量 codexCLIUserAgent。
func SetCodexCanonicalUserAgentResolver(resolver func() string) {
	codexCanonicalUAMu.Lock()
	defer codexCanonicalUAMu.Unlock()
	codexCanonicalUAResolver = resolver
}

// CodexCanonicalUserAgent 返回当前生效的规范 Codex User-Agent。
// 取值走与推理相同的解析链：面板 UA 指纹 + 面板/自动同步版本号 + 编译期兜底。
// 供无账号句柄的出站路径（OAuth 换 Token / 刷新）使用。
func CodexCanonicalUserAgent() string {
	return resolveCodexOutboundIdentity("").userAgent
}

// CodexCanonicalAuthIdentity 返回凭据面（auth.openai.com：换 Token / 刷新 / whoami）
// 出站请求的身份对：规范 User-Agent 与配套 originator，与推理解析链同源。
// 凭据面不发 version 头——真实 Codex 客户端在该面只携带 originator 与 User-Agent
// （codex-rs login/default_client.rs 的 default_headers()），version 门槛
// （issue #3901）只存在于 /backend-api/codex 推理面。
func CodexCanonicalAuthIdentity() (userAgent, originator string) {
	identity := resolveCodexOutboundIdentity("")
	return identity.userAgent, identity.originator
}

// ApplyCodexCanonicalAuthIdentity 为凭据面出站请求写入身份对（不含 version）。
func ApplyCodexCanonicalAuthIdentity(h http.Header) {
	if h == nil {
		return
	}
	userAgent, originator := CodexCanonicalAuthIdentity()
	h.Set("user-agent", userAgent)
	h.Set("originator", originator)
}

// CodexCanonicalClientVersion 返回当前生效的 Codex 客户端版本号。
func CodexCanonicalClientVersion() string {
	return resolveCodexOutboundIdentity("").version
}

// codexCanonicalUserAgent 返回出站规范 User-Agent。
func codexCanonicalUserAgent() string {
	codexCanonicalUAMu.RLock()
	resolver := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	if resolver != nil {
		if ua := resolver(); strings.TrimSpace(ua) != "" {
			return ua
		}
	}
	return codexCLIUserAgent
}

// codexOutboundIdentity 出站身份三元组，三者必须同源自洽：
// originator 与 User-Agent 首段配套（否则上游 404，issue #3901），
// version 等于 User-Agent 的版本段且不低于上游门槛。
type codexOutboundIdentity struct {
	userAgent  string
	originator string
	version    string
}

// resolveCodexOutboundIdentity 由候选 User-Agent 推导自洽的出站身份。
// candidateUA 为空时使用规范 User-Agent；推导不出官方身份时整体回退为规范 CLI 身份。
//
// 候选 UA（面板 / 账号级的管理员显式配置）只贡献客户端名与 OS / 架构 / 终端指纹，
// 其自带的版本段一律用当前生效版本重建：一条填写于某个历史版本的 UA 否则会把出站身份
// 永久钉死在陈旧版本上，绕过版本自动同步，落回上游身份风控更严格的一侧。
// 需要固定版本请填「Codex 客户端版本号」并关闭自动同步。
func resolveCodexOutboundIdentity(candidateUA string) codexOutboundIdentity {
	return resolveCodexOutboundIdentityFor(candidateUA, "")
}

// resolveCodexOutboundIdentityFor 与 resolveCodexOutboundIdentity 相同，但允许调用方
// 指定期望 originator（由请求自身的 turn metadata 反查得出）。指定后 UA 首段与尾部
// 标识组一起切换到该 originator，保证 originator / UA / 元数据三者一致。
func resolveCodexOutboundIdentityFor(candidateUA, wantedOriginator string) codexOutboundIdentity {
	canonical := codexCanonicalUserAgent()
	if _, _, ok := openai.PairCodexClientIdentity(canonical); !ok {
		canonical = codexCLIUserAgent
	}
	ua := candidateUA
	if strings.TrimSpace(ua) == "" {
		ua = canonical
	}
	originator, pairedUA, ok := openai.PairCodexClientIdentity(ua)
	if !ok {
		if originator, pairedUA, ok = openai.PairCodexClientIdentity(canonical); !ok {
			originator, pairedUA = openai.CodexCLIOriginator, codexCLIUserAgent
		}
	}
	// 旧配置或账号级 UA 可能仍保存 codex-tui。该身份是 sub2api 默认出口曾经
	// 使用的风险指纹，必须与 UA 首段一起归一为 CLI，不能只替换 originator。
	originator, pairedUA, _ = openai.NormalizeCodexClientIdentityToCLI(originator, pairedUA)
	if wanted := strings.TrimSpace(wantedOriginator); wanted != "" {
		if swapped := openai.SwapCodexUserAgentClient(pairedUA, wanted); swapped != "" {
			originator, pairedUA = wanted, swapped
		}
	}
	// 生效版本只有一个来源：规范身份（面板版本号 → 自动同步值 → 内置常量，见
	// SettingService.GetOpenAICodexClientVersion）。UA 与 version 头由此同源派生。
	version := codexClientVersionFromUA(canonical)
	pairedUA = openai.WithCodexUATrailer(pairedUA, originator, version)
	if rebuilt := openai.SetCodexUserAgentVersion(pairedUA, version); rebuilt != "" {
		pairedUA = rebuilt
	}
	return codexOutboundIdentity{userAgent: pairedUA, originator: originator, version: version}
}

// codexClientVersionFromUA 取 UA 的版本段作为生效版本；
// 非法或低于上游门槛（低于则上游 404，issue #3901）时回退编译期常量。
func codexClientVersionFromUA(ua string) string {
	version := NormalizeCodexClientVersion(openai.CodexUserAgentVersion(ua))
	if version == "" || CompareVersions(version, codexUpstreamMinVersion) < 0 {
		return codexCLIVersion
	}
	return version
}

// ensureCodexIdentityHeaders 补齐 OAuth（ChatGPT 内部接口）出站请求所需的 Codex 身份头。
// 已有 User-Agent 与 version 保持不变，交给紧随其后的 enforceCodexIdentityHeaders 收口。
func ensureCodexIdentityHeaders(h http.Header) {
	if h == nil {
		return
	}
	identity := resolveCodexOutboundIdentity("")
	if strings.TrimSpace(h.Get("user-agent")) == "" {
		h.Set("user-agent", identity.userAgent)
	}
	if strings.TrimSpace(h.Get("originator")) == "" {
		h.Set("originator", identity.originator)
	}
	// version 默认不出站（官方业务请求不含该头）；开关打开时仍按规范身份补齐。
	if strings.TrimSpace(h.Get("version")) == "" {
		setCodexVersionHeader(h, identity.version)
	}
	h.Set("OpenAI-Beta", "responses=experimental")
}

// applyOpenAICodexProbeHeaders 为合成探测请求补齐 Codex 身份和引擎指纹。
func applyOpenAICodexProbeHeaders(h http.Header) {
	if h == nil {
		return
	}
	ensureCodexIdentityHeaders(h)
	h.Set("X-Codex-Window-ID", uuid.NewString())
}

// enforceCodexIdentityHeaders 收口 OAuth（ChatGPT 内部接口）出站请求的客户端身份头。
// 见 enforceCodexIdentityHeadersWithUA；无账号级自定义 User-Agent 时使用本函数。
func enforceCodexIdentityHeaders(h http.Header) {
	enforceCodexIdentityHeadersWithUA(h, "")
}

// enforceCodexIdentityHeadersWithUA 强制统一 OAuth 出站身份：User-Agent / originator / version
// 一律改写为网关的规范身份，客户端自报身份不参与构造。上游会按客户端身份进行风控分层，
// 命中风险身份的请求会拿到 HTTP 200 + 流内 server_is_overloaded；统一出口可确保没有请求带着
// 第三方或陈旧身份出站，也天然满足 originator 与 UA 首段配套的上游校验（issue #3901）。
//
// overrideUA 是账号级自定义 User-Agent：管理员的显式配置仍然生效，但只贡献客户端名与
// OS / 架构 / 终端指纹——版本段与 originator 都由规范身份重建，不允许出现自相矛盾或陈旧的身份。
//
// 强制统一被 gateway.disable_codex_identity_enforcement 关闭时，退回「按最终 User-Agent 配对
// originator + version 门槛校正」的收口语义，供上游策略变动时回滚。
//
// 仅对携带 originator 的请求生效：compat 桥接等非 ChatGPT 内部接口路径会显式删除 originator，
// 不应被补回。需要从缺失身份头恢复的调用方应先调用 ensureCodexIdentityHeaders。
// 必须在所有 User-Agent 改写之后调用。
func enforceCodexIdentityHeadersWithUA(h http.Header, overrideUA string) {
	if h == nil || h.Get("originator") == "" {
		return
	}
	if !codexIdentityEnforcement.Load() {
		pairCodexIdentityHeaders(h)
		return
	}
	identity := resolveCodexOutboundIdentity(overrideUA)
	h.Set("user-agent", identity.userAgent)
	h.Set("originator", identity.originator)
	// version 只在显式开关下写出：官方 Codex CLI 的业务请求没有这个头。
	// 客户端自报的旧值不在这里删——HTTP 业务路径由 projectCodexOutboundHeaders 削平，
	// 而 models 列表 / search / 探针等路径显式写入的 version 不受影响。
	setCodexVersionHeader(h, identity.version)
}

// enforceCodexIdentityHeadersWithOriginator 是 enforceCodexIdentityHeadersWithUA 的
// 显式 originator 版本，供「已确认客户端声明过 turn_trigger」的调用方在元数据合并完成后
// 重新收口身份。originator 为空时等价于强制统一为规范 CLI 身份。
func enforceCodexIdentityHeadersWithOriginator(h http.Header, overrideUA, originator string) {
	if h == nil || h.Get("originator") == "" {
		return
	}
	if !codexIdentityEnforcement.Load() {
		pairCodexIdentityHeaders(h)
		return
	}
	identity := resolveCodexOutboundIdentityFor(overrideUA, originator)
	h.Set("user-agent", identity.userAgent)
	h.Set("originator", identity.originator)
	setCodexVersionHeader(h, identity.version)
}

// codexClientDeclaredTurnTrigger 返回**客户端自己声明**的 turn_trigger，未声明时为空。
//
// 必须与「生效元数据」区分：ensureCodexMetadataShapeDefaults 会给缺省请求补
// turn_trigger=exec（常规回合），那是网关合成的形状值，不代表客户端真实身份。
// 拿它去切换 originator 会把网关自建的探针/诊断请求也变成 codex_exec，破坏已经
// 钉定 CLI 身份的诊断基线。这里只在请求头或请求体里真实携带时返回非空。
func codexClientDeclaredTurnTrigger(h http.Header, body []byte) string {
	embedded := gjson.GetBytes(body, "client_metadata."+openAIWSTurnMetadataHeader)
	if embedded.Type == gjson.String {
		return codexClientDeclaredTurnTriggerFromMetadata(h, embedded.String())
	}
	return codexClientDeclaredTurnTriggerFromMetadata(h, "")
}

func codexClientDeclaredTurnTriggerFromMetadata(h http.Header, embedded string) string {
	if trigger := codexTurnTriggerFromMetadata(embedded); trigger != "" {
		return trigger
	}
	return codexTurnTriggerFromMetadata(h.Get(openAIWSTurnMetadataHeader))
}

// pairCodexIdentityHeaders 是关闭强制统一后的兜底收口：保留客户端真实身份，
// 仅保证 originator 与最终 User-Agent 首段配套、version 不低于上游门槛（issue #3901）。
func pairCodexIdentityHeaders(h http.Header) {
	originator, pairedUA, ok := openai.PairCodexClientIdentity(h.Get("user-agent"))
	if !ok {
		identity := resolveCodexOutboundIdentity("")
		originator, pairedUA = identity.originator, identity.userAgent
		setCodexVersionHeader(h, identity.version)
	}
	h.Set("user-agent", pairedUA)
	h.Set("originator", originator)
	if v := strings.TrimSpace(h.Get("version")); v != "" && CompareVersions(v, codexUpstreamMinVersion) < 0 {
		setCodexVersionHeader(h, resolveCodexOutboundIdentity("").version)
	}
}

// codexHTTPNonOfficialHeaders 是官方 Codex 客户端在 ChatGPT 业务 HTTP 路径
// （POST /backend-api/codex/responses）上**从不发送**的请求头。逐项依据
// codex-rs HEAD f4e18a95bb 源码：
//
//   - session_id、conversation_id：官方会话头只有连字符形式
//     session-id / thread-id（codex-api/src/requests/headers.rs:5-11）；下划线
//     形式在 7c7b4861d8（fix: drop underscored id headers #22193）中已删除。
//   - x-codex-installation-id：官方仅作为 client_metadata 键发送
//     （core/src/responses_metadata.rs:325-334），从不作为 HTTP 头。
//   - OpenAI-Beta：只在 client.rs:1314-1330 build_websocket_headers 写入，
//     属 WS 握手专有；HTTP 业务头集合仅由 client.rs:2325-2343
//     build_responses_headers 产生（x-codex-beta-features / x-codex-turn-state）。
//   - accept-language：reqwest 不写该头，官方业务请求里不存在。
//
// 反例（官方确实发送，不得删）：version 来自内置 openai provider 的
// http_headers（model-provider-info/src/lib.rs:533-535，经
// built_in_model_providers:656 进入所有出站请求）；x-client-request-id 由
// codex-api/src/endpoint/responses.rs:87-89 在 HTTP 上写为 thread_id。
//
// 这些名字「缺失」对上游完全无感（官方自己就不发），但「存在」是本代理独有的
// 可判别特征，因此出站前按官方线序削平，而不是继续按代理习惯补齐。
var codexHTTPNonOfficialHeaders = [...]string{
	"OpenAI-Beta",
	"session_id",
	"conversation_id",
	"x-codex-installation-id",
	"accept-language",
}

// codexHTTPNonOfficialHeadersKeepVersion 是 version 开关打开时的收口名单；
// 关闭时额外削平 version（官方业务请求没有该头）。两份名单在编译期固化，
// 热路径只做一次切片选择，避免每请求分配。
var (
	codexHTTPNonOfficialHeadersKeepVersion = codexHTTPNonOfficialHeaders[:]
	codexHTTPNonOfficialHeadersDropVersion = append(
		append([]string(nil), codexHTTPNonOfficialHeaders[:]...), "version")
)

// codexHTTPProjectedNonOfficialHeaders 返回 HTTP 业务路径实际要削平的名单：
// version 是否在名单内取决于进程级开关（关闭时连客户端自报的 version 也一并删除）。
func codexHTTPProjectedNonOfficialHeaders() []string {
	if codexEmitVersionHeader.Load() {
		return codexHTTPNonOfficialHeadersKeepVersion
	}
	return codexHTTPNonOfficialHeadersDropVersion
}

// codexWSNonOfficialHeaders 是 WS 握手上官方不发的同名子集。WS 与 HTTP 的差异
// 必须保留：OpenAI-Beta 与 x-client-request-id 是官方 WS 握手**确实发送**的
// （client.rs:1314-1330），故不在 WS 列表内。
var codexWSNonOfficialHeaders = [...]string{
	"session_id",
	"conversation_id",
	"x-codex-installation-id",
	"accept-language",
}

// projectCodexOutboundHeaders 把出站头收口成官方线序：删除官方在该传输上不发的
// 头，并保证连字符会话头确实存在（官方总是同时发 session-id 与 thread-id）。
//
// 必须在所有其它头改写（指纹收敛、账号 namespace、client_metadata 投影、账号级
// 覆写）之后调用——它是最后一道收口，任何更早的补齐都可能被后续步骤重新引入。
//
// 与强制统一身份共用 gateway.disable_codex_identity_enforcement：该开关的语义就是
// 「上游策略变动时退回旧线序」，因此关闭后这里保持原样不动。
func projectCodexOutboundHeaders(h http.Header, websocket bool) {
	if h == nil || !codexIdentityEnforcement.Load() {
		return
	}
	// HTTP 与 WS 名单长度不同，这里按切片遍历，保留各自完整集合。
	var nonOfficial []string
	if websocket {
		nonOfficial = codexWSNonOfficialHeaders[:]
	} else {
		nonOfficial = codexHTTPProjectedNonOfficialHeaders()
	}
	for _, name := range nonOfficial {
		deleteHeaderAllForms(h, name)
	}
	// 官方 build_session_headers 在 session_id 与 thread_id 均存在时必然同时写出
	// 两个连字符头；这里只把已有的值补成对，不凭空造 ID（造 ID 是上游派生层的职责）。
	if session := strings.TrimSpace(h.Get("session-id")); session != "" && strings.TrimSpace(h.Get("thread-id")) == "" {
		h.Set("thread-id", session)
	}
}
