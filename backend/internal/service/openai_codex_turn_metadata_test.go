package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexTurnMetadataRewritePreservesHeaderSafeJSON(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"})
	account.Credentials = map[string]any{"chatgpt_account_id": "test-account"}
	ids := resolveCodexFingerprintIDsFromRequest(account, http.Header{})
	require.NotNil(t, ids)

	rewriters := map[string]func(*testing.T, string) string{
		"account_header": func(t *testing.T, raw string) string {
			h := make(http.Header)
			h.Set(openAIWSTurnMetadataHeader, raw)
			applyCodexAccountIdentityHeaders(h, account, 77)
			return h.Get(openAIWSTurnMetadataHeader)
		},
		"account_body_map": func(t *testing.T, raw string) string {
			cm := map[string]any{openAIWSTurnMetadataHeader: raw}
			require.True(t, applyCodexAccountIdentityClientMetadataMap(map[string]any{"client_metadata": cm}, account, 77, ""))
			value, ok := cm[openAIWSTurnMetadataHeader].(string)
			require.True(t, ok)
			return value
		},
		"account_body_raw": func(t *testing.T, raw string) string {
			body, err := json.Marshal(map[string]any{"client_metadata": map[string]any{openAIWSTurnMetadataHeader: raw}})
			require.NoError(t, err)
			updated, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 77, "")
			require.NoError(t, err)
			require.True(t, changed)
			return gjson.GetBytes(updated, "client_metadata."+openAIWSTurnMetadataHeader).String()
		},
		"fingerprint_header": func(t *testing.T, raw string) string {
			h := make(http.Header)
			h.Set(openAIWSTurnMetadataHeader, raw)
			applyCodexFingerprintHeaders(h, ids)
			return h.Get(openAIWSTurnMetadataHeader)
		},
		"fingerprint_body_map": func(t *testing.T, raw string) string {
			cm := map[string]any{openAIWSTurnMetadataHeader: raw}
			require.True(t, applyCodexFingerprintClientMetadata(map[string]any{"client_metadata": cm}, ids))
			value, ok := cm[openAIWSTurnMetadataHeader].(string)
			require.True(t, ok)
			return value
		},
		"fingerprint_body_raw": func(t *testing.T, raw string) string {
			body, err := json.Marshal(map[string]any{"client_metadata": map[string]any{openAIWSTurnMetadataHeader: raw}})
			require.NoError(t, err)
			updated, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
			require.NoError(t, err)
			require.True(t, changed)
			return gjson.GetBytes(updated, "client_metadata."+openAIWSTurnMetadataHeader).String()
		},
	}

	for _, raw := range []string{
		`{"installation_id":"client-install","workspaces":{"C:\\work\\中文🚀":{}}}`,
		`{"installation_id":"client-install","workspaces":{"C:\\work\\中文🚀":{"label":"café"}},"literal":"\\u4e2d","quote":"\"\\","controls":"\b\f\n\r\t\u0000\u007f","timestamp":1789302780858,"enabled":true}`,
		`{"installation_id":"client-install","workspaces":{"C:\\work\\\u4e2d\u6587\ud83d\ude80":{"label":"caf\u00e9"}},"literal":"\\u4e2d","quote":"\"\\","controls":"\b\f\n\r\t\u0000\u007f","timestamp":1789302780858,"enabled":true}`,
		`{"installation_id":"client-install","workspaces":{"C:\\work\\ascii":{}},"literal":"\\u4e2d"}`,
	} {
		var original map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &original))
		for name, rewrite := range rewriters {
			t.Run(name, func(t *testing.T) {
				updated := rewrite(t, raw)
				var decoded map[string]any
				require.NoError(t, json.Unmarshal([]byte(updated), &decoded))
				for key, value := range original {
					if key != "installation_id" {
						require.Equal(t, value, decoded[key], key)
					}
				}
				require.NotEqual(t, original["installation_id"], decoded["installation_id"])
				for _, b := range []byte(updated) {
					require.True(t, b >= 0x20 && b < 0x7f, "non-printable ASCII header byte: 0x%02x", b)
				}
			})
		}
	}
}

// 官方 turn metadata 由 serde struct 按声明顺序序列化（codex-rs
// core/src/responses_metadata.rs:544-601），未知键走 flatten(BTreeMap) 字典序。
// Go map 的 json.Marshal 恒为字母序——上游可逐字节比对，必须复刻官方顺序。
func TestCodexTurnMetadataMarshalUsesOfficialFieldOrder(t *testing.T) {
	raw, err := marshalCodexTurnMetadata(map[string]any{
		"turn_id":         "t-1",
		"window_id":       "w-1",
		"installation_id": "i-1",
		"session_id":      "s-1",
		"thread_id":       "th-1",
		"request_kind":    "turn",
		"zzz_extra":       "extra-key-sorted-last",
		"aaa_extra":       "another-extra",
	})
	require.NoError(t, err)
	// 官方顺序：installation_id, session_id, thread_id, …, turn_id, window_id, …, request_kind
	want := `{"installation_id":"i-1","session_id":"s-1","thread_id":"th-1","turn_id":"t-1","window_id":"w-1","request_kind":"turn","aaa_extra":"another-extra","zzz_extra":"extra-key-sorted-last"}`
	require.Equal(t, want, string(raw))
}

// 官方 AsciiJsonFormatter 不转义 & < >（serde_json 默认也不转）；Go json.Marshal
// 会把它们转成 & < >。带 query 的 git URL（workspaces.*.associated_remote_urls）
// 一旦经过代理重排就会与官方字节不一致，这是可判别的字节级差异。
func TestCodexTurnMetadataMarshalDoesNotEscapeHTMLEntities(t *testing.T) {
	raw, err := marshalCodexTurnMetadata(map[string]any{
		"turn_trigger": `a&b<c>d`,
		"workspaces": map[string]any{
			"/repo": map[string]any{
				"associated_remote_urls": map[string]any{
					"origin": "https://host/repo?a=1&b=2",
				},
				"latest_git_commit_hash": "abc123",
				"has_changes":            true,
			},
		},
	})
	require.NoError(t, err)
	require.NotContains(t, string(raw), `\u0026`, "不得出现 Go 的 & 转义")
	require.NotContains(t, string(raw), `\u003c`, "不得出现 Go 的 < 转义")
	require.NotContains(t, string(raw), `\u003e`, "不得出现 Go 的 > 转义")
	require.Contains(t, string(raw), `a&b<c>d`)
	require.Contains(t, string(raw), `https://host/repo?a=1&b=2`)
	// 非 ASCII 仍按官方行为转义为 \uXXXX（ASCII-only header 载体）。
	nonASCII, err := marshalCodexTurnMetadata(map[string]any{"agent_name": "中"})
	require.NoError(t, err)
	require.Contains(t, string(nonASCII), `\u4e2d`)
}

// 官方 workspaces 是 BTreeMap，其 value 为 struct TurnMetadataWorkspace
// （associated_remote_urls, latest_git_commit_hash, has_changes 声明顺序，
// codex-rs core/src/responses_metadata.rs:186-194），子对象也需按声明序。
func TestCodexTurnMetadataNestedObjectUsesDeclarationOrder(t *testing.T) {
	raw, err := marshalCodexTurnMetadata(map[string]any{
		"workspaces": map[string]any{
			"/repo": map[string]any{
				"has_changes":            true,
				"latest_git_commit_hash": "abc123",
				"associated_remote_urls": map[string]any{"origin": "https://x"},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t,
		`{"workspaces":{"/repo":{"associated_remote_urls":{"origin":"https://x"},"latest_git_commit_hash":"abc123","has_changes":true}}}`,
		string(raw))
}
