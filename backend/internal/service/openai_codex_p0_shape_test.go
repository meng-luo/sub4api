package service

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

// codexOfficialTurnMetadataKeys 是 Codex CLI 0.171.0 `codex exec` 实测的
// x-codex-turn-metadata 键集合（21 键），顺序即官方 serde 声明序。
var codexOfficialTurnMetadataKeys = []string{
	"installation_id",
	"session_id",
	"thread_id",
	"agent_name",
	"turn_id",
	"window_id",
	"window_number",
	"context_window_id",
	"request_kind",
	"history_initialization",
	"root_turn_id",
	"thread_source",
	"turn_trigger",
	"sandbox",
	"sandbox_mode",
	"auto_review_enabled",
	"node_repl_auto_review_required",
	"node_repl_disabled",
	"turn_started_at_unix_ms",
	"analytics_enabled",
	"model",
}

// newCodexMetadataShapeAccount 返回一个 macOS 身份的 OAuth 账号：默认 sandbox=auto
// 会跟随 UA 派生出 seatbelt，与官方实测值一致。
func newCodexMetadataShapeAccount() *Account {
	return &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "shape-account",
			"user_agent":         "codex_cli_rs/0.159.3 (Mac OS 15.1.0; arm64) dumb",
		},
	}
}

func decodeCodexShapeMetadata(t *testing.T, raw string) map[string]any {
	t.Helper()
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
	return metadata
}

// 空 metadata + 非 compact 请求应合成逐字对齐官方的 21 键，client_metadata 为 7 键。
func TestCodexTurnMetadataShapeSynthesizesOfficialKeys(t *testing.T) {
	SetCodexMetadataSandbox(config.CodexSandboxAuto, config.CodexSandboxModeReadOnly)
	t.Cleanup(func() { SetCodexMetadataSandbox(config.CodexSandboxAuto, config.CodexSandboxModeReadOnly) })

	account := newCodexMetadataShapeAccount()
	body := []byte(`{"model":"gpt-5.1-codex","stream":true,"input":[{"type":"message","role":"user","content":"hi"}]}`)

	next, changed, err := normalizeCodexRequestMetadata(body, http.Header{}, account, false)
	require.NoError(t, err)
	require.True(t, changed)

	raw := gjson.GetBytes(next, "client_metadata."+openAIWSTurnMetadataHeader).String()
	require.NotEmpty(t, raw)
	metadata := decodeCodexShapeMetadata(t, raw)

	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	require.ElementsMatch(t, codexOfficialTurnMetadataKeys, keys, "键集合必须与官方实测 21 键一致")

	// 与旧文档不同的两处实测值。
	require.Equal(t, false, metadata["node_repl_disabled"])
	require.Equal(t, "exec", metadata["turn_trigger"])
	require.Equal(t, "/root", metadata["agent_name"])
	require.Equal(t, "new", metadata["history_initialization"])
	require.Equal(t, "user", metadata["thread_source"])
	require.Equal(t, "seatbelt", metadata["sandbox"])
	require.Equal(t, "read-only", metadata["sandbox_mode"])
	require.Equal(t, false, metadata["auto_review_enabled"])
	require.Equal(t, true, metadata["analytics_enabled"])
	require.Equal(t, "gpt-5.1-codex", metadata["model"], "model 取请求体实际模型")
	require.Equal(t, metadata["turn_id"], metadata["root_turn_id"], "无 parent_turn_id 时 root=自身")

	require.Len(t, gjson.GetBytes(next, "client_metadata").Map(), 7, "client_metadata 应为实测 7 键")
	require.NotEmpty(t, gjson.GetBytes(next, "client_metadata.root_turn_id").String())
}

// 序列化必须按官方 serde 声明序，而不是 map 的字典序。
func TestCodexTurnMetadataShapeSerializesInOfficialOrder(t *testing.T) {
	account := newCodexMetadataShapeAccount()
	body := []byte(`{"model":"gpt-5.1-codex"}`)
	next, _, err := normalizeCodexRequestMetadata(body, http.Header{}, account, false)
	require.NoError(t, err)
	raw := gjson.GetBytes(next, "client_metadata."+openAIWSTurnMetadataHeader).String()
	require.NotEmpty(t, raw)

	last := -1
	for _, key := range codexOfficialTurnMetadataKeys {
		index := strings.Index(raw, `"`+key+`":`)
		require.GreaterOrEqual(t, index, 0, "缺少键 %s", key)
		require.Greater(t, index, last, "键序错误：%s", key)
		last = index
	}
}

// 客户端已经给出的形状字段是权威值，代理不得覆盖。
func TestCodexMetadataShapeDefaultsKeepClientValues(t *testing.T) {
	account := newCodexMetadataShapeAccount()
	metadata := map[string]any{"turn_id": "t-1", "turn_trigger": "tui", "sandbox": "none"}
	_, err := ensureCodexMetadataShapeDefaults(metadata, map[string]any{}, []byte(`{"model":"m"}`), account, false)
	require.NoError(t, err)
	require.Equal(t, "tui", metadata["turn_trigger"])
	require.Equal(t, "none", metadata["sandbox"])
}

// compact 回合的 request_kind 与 turn_trigger 必须联动。
func TestCodexMetadataShapeCompactTrigger(t *testing.T) {
	account := newCodexMetadataShapeAccount()
	body := []byte(`{"model":"gpt-5.1-codex","input":[{"type":"compaction_trigger"}]}`)
	next, _, err := normalizeCodexRequestMetadata(body, http.Header{}, account, false)
	require.NoError(t, err)
	metadata := decodeCodexShapeMetadata(t, gjson.GetBytes(next, "client_metadata."+openAIWSTurnMetadataHeader).String())
	require.Equal(t, "compaction", metadata["request_kind"])
	require.Equal(t, "compaction", metadata["turn_trigger"])
	require.NotContains(t, metadata, "model", "实测 compaction 回合不携带 model（用 compaction 对象替换）")
}

// root_turn_id：无 parent 时等于自身，有 parent 时继承。
func TestCodexMetadataShapeRootTurnID(t *testing.T) {
	account := newCodexMetadataShapeAccount()

	t.Run("no parent", func(t *testing.T) {
		metadata := map[string]any{"turn_id": "t-1"}
		_, err := ensureCodexMetadataShapeDefaults(metadata, map[string]any{}, []byte(`{}`), account, false)
		require.NoError(t, err)
		require.Equal(t, "t-1", metadata["root_turn_id"])
		require.Equal(t, "new", metadata["history_initialization"])
	})

	t.Run("with parent", func(t *testing.T) {
		metadata := map[string]any{"turn_id": "t-2", "parent_turn_id": "X"}
		_, err := ensureCodexMetadataShapeDefaults(metadata, map[string]any{}, []byte(`{}`), account, false)
		require.NoError(t, err)
		require.Equal(t, "X", metadata["root_turn_id"])
		require.Equal(t, "resume", metadata["history_initialization"])
	})
}

// sandbox=auto 时按 User-Agent 的 OS 段派生，避免「Windows UA + seatbelt」的矛盾。
func TestCodexMetadataShapeSandboxFollowsUserAgentOS(t *testing.T) {
	SetCodexMetadataSandbox(config.CodexSandboxAuto, config.CodexSandboxModeReadOnly)
	t.Cleanup(func() { SetCodexMetadataSandbox(config.CodexSandboxAuto, config.CodexSandboxModeReadOnly) })

	cases := []struct {
		name string
		ua   string
		want string
	}{
		{"macos", "codex_cli_rs/0.159.3 (Mac OS 15.1.0; arm64) dumb", config.CodexSandboxSeatbelt},
		{"windows", "codex_cli_rs/0.159.3 (Windows 10.0.22621.1848; x86_64) dumb", config.CodexSandboxNone},
		{"linux", "codex_cli_rs/0.159.3 (Ubuntu 24.04; x86_64) dumb", config.CodexSandboxNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := newCodexMetadataShapeAccount()
			account.Credentials["user_agent"] = tc.ua
			metadata := map[string]any{"turn_id": "t-auto"}
			_, err := ensureCodexMetadataShapeDefaults(metadata, map[string]any{}, []byte(`{}`), account, false)
			require.NoError(t, err)
			require.Equal(t, tc.want, metadata["sandbox"])
		})
	}
}

// 显式配置优先于 UA 派生。
func TestCodexMetadataShapeHonorsSandboxConfig(t *testing.T) {
	SetCodexMetadataSandbox(config.CodexSandboxNone, config.CodexSandboxModeDangerFullAccess)
	t.Cleanup(func() { SetCodexMetadataSandbox(config.CodexSandboxAuto, config.CodexSandboxModeReadOnly) })

	metadata := map[string]any{"turn_id": "t-1"}
	_, err := ensureCodexMetadataShapeDefaults(metadata, map[string]any{}, []byte(`{}`), newCodexMetadataShapeAccount(), false)
	require.NoError(t, err)
	require.Equal(t, config.CodexSandboxNone, metadata["sandbox"])
	require.Equal(t, config.CodexSandboxModeDangerFullAccess, metadata["sandbox_mode"])
}

// P0-2：version 头默认不出站，开关打开后按规范身份补齐。
func TestCodexVersionHeaderSwitch(t *testing.T) {
	SetCodexEmitVersionHeader(false)
	t.Cleanup(func() { SetCodexEmitVersionHeader(false) })

	t.Run("default off", func(t *testing.T) {
		h := make(http.Header)
		h.Set("version", "9.9.9")
		ensureCodexIdentityHeaders(h)
		enforceCodexIdentityHeaders(h)
		projectCodexOutboundHeaders(h, false)
		require.Empty(t, h.Get("version"))
	})

	t.Run("enabled", func(t *testing.T) {
		SetCodexEmitVersionHeader(true)
		t.Cleanup(func() { SetCodexEmitVersionHeader(false) })

		h := make(http.Header)
		ensureCodexIdentityHeaders(h)
		enforceCodexIdentityHeaders(h)
		projectCodexOutboundHeaders(h, false)
		require.Equal(t, resolveCodexOutboundIdentity("").version, h.Get("version"))
	})

	t.Run("websocket keeps client value when disabled", func(t *testing.T) {
		SetCodexEmitVersionHeader(false)
		h := make(http.Header)
		h.Set("version", "1.2.3")
		projectCodexOutboundHeaders(h, true)
		require.Equal(t, "1.2.3", h.Get("version"), "WS 名单不含 version")
	})
}

// 官方身份三元组与 version 开关互不影响：UA/originator 始终收敛。
func TestCodexIdentityStaysCanonicalWithoutVersionHeader(t *testing.T) {
	SetCodexEmitVersionHeader(false)
	t.Cleanup(func() { SetCodexEmitVersionHeader(false) })

	h := make(http.Header)
	h.Set("user-agent", "third-party/1.0.0")
	h.Set("originator", "codex_cli_rs")
	enforceCodexIdentityHeaders(h)
	projectCodexOutboundHeaders(h, false)

	require.Equal(t, openai.CodexDefaultOriginator, h.Get("originator"))
	require.Equal(t, codexCanonicalUserAgent(), h.Get("user-agent"))
	require.Empty(t, h.Get("version"))
}

// legacy compact 不走 project 收口：开关关闭时连客户端透传的 version 都要删掉，
// 打开时缺失则补规范值、已有则保留。
func TestApplyCodexCompactVersionHeader(t *testing.T) {
	t.Run("off drops client value", func(t *testing.T) {
		SetCodexEmitVersionHeader(false)
		t.Cleanup(func() { SetCodexEmitVersionHeader(false) })
		h := make(http.Header)
		h.Set("version", "1.2.3")
		applyCodexCompactVersionHeader(h)
		require.Empty(t, h.Get("version"))
	})

	t.Run("on fills missing", func(t *testing.T) {
		SetCodexEmitVersionHeader(true)
		t.Cleanup(func() { SetCodexEmitVersionHeader(false) })
		h := make(http.Header)
		applyCodexCompactVersionHeader(h)
		require.Equal(t, CodexCanonicalClientVersion(), h.Get("version"))
	})

	t.Run("on keeps existing", func(t *testing.T) {
		SetCodexEmitVersionHeader(true)
		t.Cleanup(func() { SetCodexEmitVersionHeader(false) })
		h := make(http.Header)
		h.Set("version", "0.200.1")
		applyCodexCompactVersionHeader(h)
		require.Equal(t, "0.200.1", h.Get("version"))
	})
}

// body 未声明 model 时不写该键（不做猜测性兜底），其余 20 键仍完整。
func TestCodexMetadataShapeOmitsModelWithoutBodyModel(t *testing.T) {
	account := newCodexMetadataShapeAccount()
	next, _, err := normalizeCodexRequestMetadata([]byte(`{"stream":true}`), http.Header{}, account, false)
	require.NoError(t, err)
	metadata := decodeCodexShapeMetadata(t, gjson.GetBytes(next, "client_metadata."+openAIWSTurnMetadataHeader).String())
	_, hasModel := metadata["model"]
	require.False(t, hasModel, "body 没有 model 时不应凭空补默认模型")
	require.Len(t, metadata, len(codexOfficialTurnMetadataKeys)-1)
}

// 真实客户端的原生压缩请求：metadata 带 compaction 对象、request_kind=compaction、
// 无 model，且 body 里没有 compaction_trigger item（isCompact=false）。
// 代理不得把它误判成普通回合（补 model / turn_trigger=exec）。
func TestCodexMetadataShapeDetectsClientCompactionMetadata(t *testing.T) {
	account := newCodexMetadataShapeAccount()
	clientMetadata := `{"request_kind":"compaction","compaction":{"trigger":"auto","reason":"context_limit",` +
		`"implementation":"responses","phase":"pre_turn","strategy":"memento"}}`
	payload := []byte(`{"model":"gpt-5.1-codex","input":[{"type":"message","role":"user","content":"hi"}],` +
		`"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(clientMetadata) + `}}`)

	next, _, err := normalizeCodexRequestMetadata(payload, http.Header{}, account, false)
	require.NoError(t, err)
	metadata := decodeCodexShapeMetadata(t, gjson.GetBytes(next, "client_metadata."+openAIWSTurnMetadataHeader).String())

	require.NotContains(t, metadata, "model", "压缩回合不得补 model")
	require.Equal(t, "compaction", metadata["turn_trigger"])
	require.Equal(t, "compaction", metadata["request_kind"])
	require.NotNil(t, metadata["compaction"], "客户端自带的 compaction 对象必须保留")
}
