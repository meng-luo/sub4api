//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
)

// buildRealisticCodexLiteBody 构造一个形态接近真机 Codex 0.171.0 的
// responses-lite 透传请求体：additional_tools 前置于 history、无顶层 tools、
// 无 instructions、client_metadata 携带内嵌 turn metadata。
func buildRealisticCodexLiteBody(targetBytes int) []byte {
	tm := `{"installation_id":"a3f880d7-091f-4604-9c0f-87e4a5016e99",` +
		`"session_id":"01a2064b-8ab7-75c3-8517-bd927169a05a",` +
		`"thread_id":"01a2064b-8ab7-75c3-8517-bd927169a05a","agent_name":"/root",` +
		`"turn_id":"01a2064b-8aca-7672-8207-879f6d4e26bf",` +
		`"window_id":"01a2064b-8ab7-75c3-8517-bd927169a05a:0","window_number":0,` +
		`"context_window_id":"01a2064b-8aba-7070-a8cf-65af63980838",` +
		`"request_kind":"turn","history_initialization":"new",` +
		`"root_turn_id":"01a2064b-8aca-7672-8207-879f6d4e26bf","thread_source":"user",` +
		`"turn_trigger":"exec","sandbox":"none","sandbox_mode":"danger-full-access",` +
		`"auto_review_enabled":false,"node_repl_auto_review_required":true,` +
		`"node_repl_disabled":false,"turn_started_at_unix_ms":1795401943756,` +
		`"analytics_enabled":true,"model":"gpt-5.6-luna","reasoning_effort":"low"}`
	tmJSON, _ := json.Marshal(tm)

	tools := `[{"type":"namespace","name":"functions","description":"","tools":[` +
		`{"type":"function","name":"shell","description":"Run a shell command.","parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}},"workdir":{"type":"string"},"timeout_ms":{"type":"number"}},"required":["command"],"additionalProperties":false}},` +
		`{"type":"function","name":"apply_patch","description":"Apply a patch to files.","parameters":{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"],"additionalProperties":false}}]},` +
		`{"type":"namespace","name":"clock","description":""},` +
		`{"type":"namespace","name":"collaboration","description":""}]`

	chunk := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 16)
	var hist []string
	hist = append(hist, fmt.Sprintf(
		`{"type":"additional_tools","id":"at_01a2064b-9ca0-7183-a338-99fd67bde3e2","role":"developer","tools":%s}`, tools))
	hist = append(hist, `{"type":"message","id":"msg_01a2064b-0001","role":"developer","content":[{"type":"input_text","text":"You are Codex."}]}`)
	size := 1024
	for size < targetBytes {
		hist = append(hist, fmt.Sprintf(
			`{"type":"message","id":"msg_01a2064b-%04d","role":"user","content":[{"type":"input_text","text":%q}]}`, size, chunk))
		size += len(chunk) + 120
	}

	body := fmt.Sprintf(
		`{"model":"gpt-5.6-luna","stream":true,"input":[%s],"tool_choice":"auto",`+
			`"parallel_tool_calls":false,"reasoning":{"effort":"low","context":"all_turns"},`+
			`"store":false,"include":["reasoning.encrypted_content"],`+
			`"prompt_cache_key":"01a2064b-8ab7-75c3-8517-bd927169a05a",`+
			`"text":{"verbosity":"low"},`+
			`"client_metadata":{"x-codex-turn-metadata":%s,`+
			`"thread_id":"01a2064b-8ab7-75c3-8517-bd927169a05a",`+
			`"turn_id":"01a2064b-8aca-7672-8207-879f6d4e26bf",`+
			`"x-codex-installation-id":"a3f880d7-091f-4604-9c0f-87e4a5016e99",`+
			`"session_id":"01a2064b-8ab7-75c3-8517-bd927169a05a",`+
			`"x-codex-window-id":"01a2064b-8ab7-75c3-8517-bd927169a05a:0",`+
			`"root_turn_id":"01a2064b-8aca-7672-8207-879f6d4e26bf"}}`,
		strings.Join(hist, ","), string(tmJSON))
	return []byte(body)
}

func newPerfBenchAccount() *Account {
	return &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"chatgpt_account_id": "4404e84d-f047-4ff1-8c1d-a6ea6518cbae",
			"chatgpt_user_id":    "user-QTyNe1n3OFlGO9q8Ctv3jIwT",
		},
	}
}

// runCodexPassthroughBodyChain 按 forwardOpenAIPassthrough 的实际顺序执行
// 全部出站 body 变换，用于度量「一次请求要扫几遍 body」。
func runCodexPassthroughBodyChain(b *testing.B, body []byte, account *Account) {
	b.Helper()
	cfg := &config.Config{}
	_ = cfg

	next, _, err := normalizeGPT61SolResponsesRequest(body, gjsonModel(body))
	if err != nil {
		b.Fatal(err)
	}
	body = next

	next, _, err = normalizeOpenAIPassthroughOAuthBody(body, false)
	if err != nil {
		b.Fatal(err)
	}
	body = next

	next, _, err = applyCodexAccountIdentityClientMetadataRaw(body, account, 7, "01a2064b-8ab7-75c3-8517-bd927169a05a")
	if err != nil {
		b.Fatal(err)
	}
	body = next

	next, _, err = normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, account, openAIResponsesCompatibilityOptions{
		Config:        cfg,
		ResponsesLite: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	body = next

	next, _, err = normalizeOpenAIResponsesLitePayloadForAccount(body, account)
	if err != nil {
		b.Fatal(err)
	}
	body = next

	next, _, _, err = aliasOpenAIOAuthReservedToolNamesBody(body)
	if err != nil {
		b.Fatal(err)
	}
	body = next

	next, _, err = sanitizeEmptyBase64InputImagesInOpenAIBody(body)
	if err != nil {
		b.Fatal(err)
	}
	body = next

	body = normalizeDeepSeekResponsesRequestBody(account, body)

	req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", nil)
	next, err = applyCodexRequestMetadata(req, body, account, false)
	if err != nil {
		b.Fatal(err)
	}
	body = next
	if len(body) == 0 {
		b.Fatal("empty body")
	}
}

func gjsonModel(body []byte) string {
	const key = `"model":"`
	i := strings.Index(string(body[:min(len(body), 64)]), key)
	if i < 0 {
		return ""
	}
	rest := string(body[:min(len(body), 64)])[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func BenchmarkCodexPassthroughBodyChain(b *testing.B) {
	account := newPerfBenchAccount()
	for _, size := range []int{256 << 10, 2 << 20, 8 << 20} {
		body := buildRealisticCodexLiteBody(size)
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runCodexPassthroughBodyChain(b, body, account)
			}
		})
	}
}
