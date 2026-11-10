package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func pageContextItem(name, output string) map[string]any {
	return map[string]any{"type": "function_call_output", "id": "fco_page", "namespace": "agent", "name": name, "output": output}
}

func pageContextRequest(t *testing.T, items ...any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "gpt-5", "input": items})
	require.NoError(t, err)
	return body
}

func TestNormalizeCodexPageContextFirstMessage(t *testing.T) {
	open := `{"page_id":null}`
	selected := "Companion Page snapshot part 1 of 1:\n" + `{"page_id":null,"document_view":null}`
	body := pageContextRequest(t, pageContextItem("codex_apps_open_page", open), pageContextItem("codex_apps_selected_page_part_1_of_1", selected), map[string]any{"role": "user", "content": "hello"})
	h := &OpenAIGatewayHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	require.False(t, h.validateFunctionCallOutputRequest(c, body, zap.NewNop()))
	require.Equal(t, 400, w.Code)
	require.Contains(t, w.Body.String(), "function_call_output requires call_id")

	got, changed := normalizeCodexPageContext(body)
	require.True(t, changed)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	require.True(t, h.validateFunctionCallOutputRequest(c, got, zap.NewNop()))
	require.False(t, c.Writer.Written())
	for i, original := range []string{open, selected} {
		item := gjson.GetBytes(got, "input").Array()[i]
		name := gjson.GetBytes(body, "input").Array()[i].Get("name").String()
		require.Equal(t, "message", item.Get("type").String())
		require.Equal(t, "user", item.Get("role").String())
		require.Equal(t, "input_text", item.Get("content.0.type").String())
		require.Equal(t, "<external_"+name+">\n"+original+"\n</external_"+name+">", item.Get("content.0.text").String())
		require.False(t, item.Get("call_id").Exists())
		require.False(t, item.Get("id").Exists())
	}
	require.Equal(t, "hello", gjson.GetBytes(got, "input.2.content").String())
	again, changed := normalizeCodexPageContext(got)
	require.False(t, changed)
	require.Equal(t, got, again)
}

func TestOpenAIResponses_CodexPageContextBeforeToolValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		item map[string]any
		pass bool
	}{
		{"page only", nil, true},
		{"with automation", map[string]any{"type": "function_call_output", "namespace": "codex_app", "name": "automation_update", "output": "<heartbeat><automation_id>test</automation_id></heartbeat>"}, true},
		{"with delegation", map[string]any{"type": "function_call_output", "namespace": "codex_app", "name": "create_thread", "output": delegationEnvelope}, true},
		{"with invalid tool output", pageContextItem("read_file", "result"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := []any{pageContextItem("codex_apps_open_page", `{"page_id":null}`)}
			if tc.item != nil {
				items = append(items, tc.item)
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(pageContextRequest(t, items...)))
			c.Request.Header.Set("Content-Type", "application/json")
			groupID := int64(2)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 101, GroupID: &groupID, User: &service.User{ID: 1},
			})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})

			// Stop at concurrency admission, immediately after request validation.
			admitted := false
			h := newOpenAIHandlerForPreviousResponseIDValidation(t, &concurrencyCacheMock{
				acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
					admitted = true
					return false, errors.New("stop after request validation")
				},
			})
			h.Responses(c)

			require.Equal(t, tc.pass, admitted, w.Body.String())
			if !tc.pass {
				require.Equal(t, http.StatusBadRequest, w.Code)
				require.Contains(t, w.Body.String(), "function_call_output requires call_id")
			}
		})
	}
}

func TestNormalizeCodexPageContextPreservesHistoryAndToolValidation(t *testing.T) {
	body := []byte(`{"model":"gpt-5","previous_response_id":"resp_history","metadata":{"large":9007199254740993},"input":[{"type":"function_call","name":"read_file","call_id":"call_real","arguments":"{}"},{"type":"function_call_output","call_id":"call_real","output":"result"},{"type":"function_call_output","namespace":"agent","name":"codex_apps_open_page","output":"{\"page_id\":\"page_1\"}"}]}`)
	got, changed := normalizeCodexPageContext(body)
	require.True(t, changed)
	require.Equal(t, "resp_history", gjson.GetBytes(got, "previous_response_id").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(got, "metadata.large").Raw)
	for _, index := range []string{"input.0", "input.1"} {
		require.JSONEq(t, gjson.GetBytes(body, index).Raw, gjson.GetBytes(got, index).Raw)
	}

	badTool := pageContextItem("read_file", "result")
	body = pageContextRequest(t, pageContextItem("codex_apps_open_page", `{"page_id":null}`), badTool)
	got, changed = normalizeCodexPageContext(body)
	require.True(t, changed)
	require.True(t, service.ValidateFunctionCallOutputContextBytes(got).HasFunctionCallOutputMissingCallID)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	require.False(t, (&OpenAIGatewayHandler{}).validateFunctionCallOutputRequest(c, got, zap.NewNop()))
	require.Equal(t, 400, w.Code)
}

func TestNormalizeCodexPageContextMixedBootstrap(t *testing.T) {
	for _, item := range []map[string]any{
		{"type": "function_call_output", "namespace": "codex_app", "name": "create_thread", "output": delegationEnvelope},
		{"type": "function_call_output", "namespace": "codex_app", "name": "automation_update", "output": "<heartbeat><automation_id>test</automation_id></heartbeat>"},
	} {
		t.Run(item["name"].(string), func(t *testing.T) {
			body := pageContextRequest(t, pageContextItem("codex_apps_open_page", `{"page_id":null}`), item)
			got, changed := normalizeCodexPageContext(body)
			require.True(t, changed)
			got, _ = normalizeCodexAutomationBootstrap(got)
			got, _ = normalizeCodexDelegationBootstrap(got)
			require.False(t, service.ValidateFunctionCallOutputContextBytes(got).HasFunctionCallOutput)
		})
	}
}

func TestNormalizeCodexPageContextMultipart(t *testing.T) {
	first := "Companion Page snapshot part 1 of 2:\n" + `{"page_id":"page1","document_view":"hello`
	second := "Companion Page snapshot part 2 of 2:\n" + ` world"}`
	body := pageContextRequest(t, pageContextItem("codex_apps_selected_page_part_1_of_2", first), pageContextItem("codex_apps_selected_page_part_2_of_2", second))
	got, changed := normalizeCodexPageContext(body)
	require.True(t, changed)
	require.Contains(t, gjson.GetBytes(got, "input.0.content.0.text").String(), first)
	require.Contains(t, gjson.GetBytes(got, "input.1.content.0.text").String(), second)
}

func TestNormalizeCodexPageContextRejectsUnrelatedOrMalformedItems(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value any
	}{
		{"real call", "call_id", "call_real"},
		{"invalid call id", "call_id", 42},
		{"null call id", "call_id", nil},
		{"other namespace", "namespace", "codex_app"},
		{"missing namespace", "namespace", ""},
		{"other tool", "name", "read_file"},
		{"other type", "type", "custom_tool_call_output"},
		{"not JSON", "output", "some text"},
		{"not snapshot", "output", `{"other":null}`},
		{"wrong page id", "output", `{"page_id":1}`},
		{"empty page id", "output", `{"page_id":" "}`},
		{"duplicate snapshot key", "output", `{"page_id":null,"page_id":"other"}`},
		{"part without header", "name", "codex_apps_selected_page_part_1_of_1"},
		{"part out of range", "name", "codex_apps_selected_page_part_2_of_1"},
		{"non string output", "output", map[string]any{"page_id": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := pageContextItem("codex_apps_open_page", `{"page_id":null}`)
			item[tc.key] = tc.value
			body := pageContextRequest(t, item)
			got, changed := normalizeCodexPageContext(body)
			require.False(t, changed)
			require.Equal(t, body, got)
		})
	}
	for _, body := range []string{
		`{"input":[{"type":"function_call_output","namespace":"agent","name":"codex_apps_open_page","call_id":"real","call_id":"","output":"{\"page_id\":null}"}]}`,
		`{"input":[{"type":"function_call_output","namespace":"agent","name":"codex_apps_open_page","output":"{\"page_id\":null}"}]} trailing`,
		`{"input":"codex_apps_open_page function_call_output"}`,
	} {
		got, changed := normalizeCodexPageContext([]byte(body))
		require.False(t, changed)
		require.Equal(t, body, string(got))
	}
}

func TestNormalizeCodexPageContextBlankCallIDAndEscapedName(t *testing.T) {
	for _, callID := range []string{"", " \t"} {
		item := pageContextItem("codex_apps_open_page", `{"page_id":"page_1"}`)
		item["call_id"] = callID
		body := pageContextRequest(t, item)
		body = []byte(strings.Replace(string(body), `"codex_apps_open_page"`, `"\u0063odex_apps_open_page"`, 1))
		got, changed := normalizeCodexPageContext(body)
		require.True(t, changed)
		require.Equal(t, "user", gjson.GetBytes(got, "input.0.role").String())
	}
}

func TestValidCodexSelectedPageContext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		valid  bool
	}{
		{"codex_apps_selected_page_part_1_of_1", "Companion Page snapshot part 1 of 1:\r\n{\"page_id\":null}", true},
		{"codex_apps_selected_page_part_1_of_1", "Companion Page snapshot part 1 of 1:\nnot JSON", false},
		{"codex_apps_selected_page_part_1_of_2", "Companion Page snapshot part 2 of 2:\nfragment", false},
		{"codex_apps_selected_page_part_1_of_2", "Companion Page snapshot part 1 of 2:\n \n", false},
		{"codex_apps_selected_page_part_0_of_2", "Companion Page snapshot part 0 of 2:\nfragment", false},
		{"codex_apps_selected_page_part_2_of_1", "Companion Page snapshot part 2 of 1:\nfragment", false},
		{"codex_apps_selected_page_part_1_of_999999999999999999999999", "fragment", false},
	} {
		require.Equal(t, tc.valid, validCodexPageContext(tc.name, tc.output), "%s: %s", tc.name, tc.output)
	}
}
