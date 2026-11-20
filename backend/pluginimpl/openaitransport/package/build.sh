#!/usr/bin/env bash
# 构建 Sub4API 自研 OpenAI Transport 插件包（.s2plugin）。
#
# 用法：
#   ./build.sh                       # 只打当前平台
#   ./build.sh darwin-arm64 linux-amd64 linux-arm64
#   SIGN_KEY=~/.sub4api/plugin-signing.key ./build.sh   # 同时签名
#
# 产物：dist/sub4api-openai-transport-<version>.s2plugin（未签名或已签名）
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKEND="$(cd "${HERE}/../../.." && pwd)"
VERSION="${VERSION:-0.1.0}"
PLUGIN_ID="local.sub4api.openai-transport"
NAME="Sub4API OpenAI Transport"
OUT="${OUT:-${HERE}/dist}"
WORK="${OUT}/stage"

targets=("$@")
if [ ${#targets[@]} -eq 0 ]; then
  case "$(uname -s)-$(uname -m)" in
    Darwin-arm64) targets=("darwin-arm64") ;;
    Darwin-x86_64) targets=("darwin-amd64") ;;
    Linux-x86_64) targets=("linux-amd64") ;;
    Linux-aarch64) targets=("linux-arm64") ;;
    *) echo "未知平台，请显式传入目标，例如 darwin-arm64" >&2; exit 1 ;;
  esac
fi

hash_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

command -v zip >/dev/null 2>&1 || { echo "缺少 zip 命令，无法打包" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "缺少 python3，无法生成清单" >&2; exit 1; }

rm -rf "${WORK}"
mkdir -p "${WORK}/runtimes" "${WORK}/ui"
cp "${HERE}/ui/index.html" "${WORK}/ui/index.html"

runtimes_json=""
files_json=""
for t in "${targets[@]}"; do
  os="${t%%-*}"; arch="${t##*-}"
  bin="plugin"
  [ "${os}" = "windows" ] && bin="plugin.exe"
  rel="runtimes/${t}/${bin}"
  mkdir -p "${WORK}/runtimes/${t}"
  echo "==> building ${t}"
  ( cd "${BACKEND}" && GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
      go build -trimpath -ldflags "-s -w" -o "${WORK}/${rel}" ./cmd/openai-transport-plugin )
  hash=$(hash_sha256 "${WORK}/${rel}")
  runtimes_json+=$([ -z "${runtimes_json}" ] && echo "" || echo ",")
  runtimes_json+="\"${t}\":{\"path\":\"${rel}\"}"
  files_json+=$([ -z "${files_json}" ] && echo "" || echo ",")
  files_json+="\"${rel}\":\"${hash}\""
done

ui_hash=$(hash_sha256 "${WORK}/ui/index.html")
files_json+=",\"ui/index.html\":\"${ui_hash}\""

manifest="${WORK}/manifest.json"
cat > "${manifest}" <<JSON
{
  "schema_version": 1,
  "id": "${PLUGIN_ID}",
  "name": "${NAME}",
  "version": "${VERSION}",
  "author": "sub4api self-hosted",
  "description": "Self-hosted OpenAI OAuth outbound transport (rustls-like TLS + h2).",
  "requires": {
    "sub2api": ">=1.0.0",
    "plugin_protocol": 1,
    "transport_api": 1,
    "ui_bridge": 1,
    "tested_sub2api_versions": ["1.0.28"],
    "recommended_sub2api_version": "1.0.28"
  },
  "capabilities": [
    { "id": "openai.oauth.outbound_transport.v1", "platform": "openai", "account_type": "oauth" }
  ],
  "runtimes": { ${runtimes_json} },
  "ui": { "entrypoint": "ui/index.html" },
  "files": { ${files_json} }
}
JSON

python3 - "${manifest}" <<'PY'
import json,sys
m=json.load(open(sys.argv[1]))
json.dump(m, open(sys.argv[1],'w'), ensure_ascii=False, indent=2)
print("manifest ok:", m["id"], m["version"], "runtimes:", list(m["runtimes"]))
PY

pkg="${OUT}/sub4api-openai-transport-${VERSION}.s2plugin"
rm -f "${pkg}"
( cd "${WORK}" && zip -qr "${pkg}" . -x '.*' )

if [ -n "${SIGN_KEY:-}" ]; then
  python3 "${HERE}/sign.py" "${pkg}" "${SIGN_KEY}"
fi

echo "==> done: ${pkg}"
echo "    安装：管理页 → 插件 → 上传；并把 sign.py 打印的发布者公钥写入 plugins.trusted_publishers"
