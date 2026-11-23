//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 以下用例锁定「零拷贝字面量前置」与「原解码路径」的等价性。
// 每条都对应一类 gjson 视图与 map[string]any 解码语义不同的输入：
// JSON null、\uXXXX 转义、重复键。前置必须把这类请求交回原路径。

func TestHasJSONUnicodeEscape(t *testing.T) {
	require.False(t, hasJSONUnicodeEscape([]byte(`{"a":"b\n\t\"c\\d"}`)))
	require.True(t, hasJSONUnicodeEscape([]byte(`{"a":"\u0074ools"}`)))
	require.True(t, hasJSONUnicodeEscape([]byte(`{"\u0061":1}`)))
}

// tools:null 在原实现里解成 nil，被当作 hasTools=true 走重建路径；
// 前置曾按 gjson 的 Null 类型报「必须是数组」而拒绝，属于行为回退。
func TestLitePayloadToolsNullKeepsNormalisationPath(t *testing.T) {
	body := []byte(`{"tools":null,"input":[{"type":"message","role":"user","content":[]}],"parallel_tool_calls":false,"reasoning":{"context":"all_turns"}}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err, "tools:null 必须走原规范化路径而不是被快速路径拒绝")
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "tools").Exists(), "顶层 tools 应被移除")
}

// instructions:null 在原实现里因 raw == nil 被忽略，既不报错也不改写语义。
func TestLitePayloadInstructionsNullKeepsNormalisationPath(t *testing.T) {
	body := []byte(`{"instructions":null,"input":[{"type":"message","role":"user","content":[]}],"parallel_tool_calls":false,"reasoning":{"context":"all_turns"}}`)
	_, _, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err, "instructions:null 必须走原规范化路径而不是被快速路径拒绝")
}

// 键名含 \u 转义时字面量搜不到，前置必须放弃，让原路径按解码后的键名处理。
func TestLitePayloadEscapedToolsKeyKeepsNormalisationPath(t *testing.T) {
	body := []byte(`{"\u0074ools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"input":[],"parallel_tool_calls":false,"reasoning":{"context":"all_turns"}}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed, `"\u0074ools" 解码后就是 tools，必须被识别`)
	require.Len(t, gjson.GetBytes(updated, "input").Array(), 1, "tools 应被折叠进 additional_tools")
	require.False(t, gjson.GetBytes(updated, "tools").Exists())
}

// detail 写成转义形式时，前置的 "detail" memmem 会漏掉，导致该字段不被剥离。
func TestLitePayloadEscapedImageDetailIsStillStripped(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA==","det\u0061il":"original"}]}],"parallel_tool_calls":false,"reasoning":{"context":"all_turns"}}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "input.0.content.0.detail").Exists(),
		"转义写法的 detail 也必须被剥离")
}

// 重复键：map 解码取后者，gjson 取首个。前置读首个会误判「已是 all_turns」。
func TestLitePayloadDuplicateReasoningContextUsesLastValue(t *testing.T) {
	body := []byte(`{"input":[],"parallel_tool_calls":false,"reasoning":{"context":"all_turns","context":"other"}}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed, "解码后 context=other，必须被改写为 all_turns")
	require.Equal(t, "all_turns", gjson.GetBytes(updated, "reasoning.context").String())
}

// 交接前置：item_reference 的 type 值可用 \u 转义，memmem 会漏判。
func TestHandoffDetectsEscapedItemReference(t *testing.T) {
	h := newOpenAIWSHTTPHandoffHistory(nil)
	payload := []byte(`{"input":[{"type":"item_referenc\u0065","id":"x"}]}`)
	_, safe, _ := h.inputForTurn(payload)
	require.False(t, safe, "转义写法的 item_reference 仍必须让交接放弃复用历史")
}

// 交接前置：previous_response_id 的键名可用 \u 转义。
func TestHandoffDetectsEscapedPreviousResponseID(t *testing.T) {
	h := newOpenAIWSHTTPHandoffHistory(nil)
	plain := []byte(`{"previous_response_id":"resp_1","input":[]}`)
	escaped := []byte(`{"previous_response_\u0069d":"resp_1","input":[]}`)

	_, plainSafe, _ := h.inputForTurn(plain)
	_, escapedSafe, _ := h.inputForTurn(escaped)
	require.Equal(t, plainSafe, escapedSafe,
		"转义写法的 previous_response_id 必须与明文写法得到相同的安全性结论")
	require.False(t, escapedSafe, "缺少历史时带 previous_response_id 必须不可安全复用")
}

// 覆盖分析前置：工具输出类型值可用 \u 转义。
func TestCoverageDetectsEscapedToolOutputType(t *testing.T) {
	plain := []byte(`{"input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)
	escaped := []byte(`{"input":[{"type":"function_call_outpu\u0074","call_id":"c1","output":"ok"}]}`)

	plainCoverage := AnalyzeToolCallOutputContextCoverageBytes(plain)
	escapedCoverage := AnalyzeToolCallOutputContextCoverageBytes(escaped)
	require.True(t, plainCoverage.HasFunctionCallOutput)
	require.Equal(t, plainCoverage.HasFunctionCallOutput, escapedCoverage.HasFunctionCallOutput,
		"转义写法的工具输出类型必须被同样识别")
}

// OAuth 输入元数据清理：字段名可用 \u 转义，前置漏判会把内部字段发给上游。
func TestOAuthMetadataCleanupHandlesEscapedFieldName(t *testing.T) {
	const field = "internal_chat_message_metadata_passthrough"

	escapeField := func(name string) string {
		// 把最后一个字符写成 \uXXXX
		last := name[len(name)-1]
		return name[:len(name)-1] + `\u` + "00" + string("0123456789abcdef"[last>>4]) + string("0123456789abcdef"[last&0xf])
	}

	body := []byte(`{"input":[{"type":"message","role":"user","content":[],"` + escapeField(field) + `":{"k":"v"}}],"model":"gpt-5.6-luna"}`)
	updated, _, err := normalizeOpenAIOAuthResponsesCompatibilityBody(body)
	require.NoError(t, err)

	require.True(t, gjson.GetBytes([]byte(`{"x":1}`), "x").Exists(), "sanity")
	for _, path := range []string{"input.0." + field} {
		require.False(t, gjson.GetBytes(updated, path).Exists(),
			"转义写法的内部字段也必须被清理：%s", path)
	}
}

func TestLitePayloadDuplicateToolsKeyUsesLastValue(t *testing.T) {
	const lastTools = `[{"type":"function","name":"shell","parameters":{"type":"object"}}]`
	const metadata = `{ "tools": ["keep"], "id": 900719925474099312345 }`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"adjacent", `{"tools":{},"tools":` + lastTools + `,"metadata":` + metadata + `,"input":[]}`},
		{"interleaved and escaped", ` { "\u0074ools":{} , "metadata":` + metadata + ` , "tools":null , "input":[] , "tool\u0073":` + lastTools + ` } `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload([]byte(tc.body))
			require.NoError(t, err)
			require.True(t, changed)
			require.True(t, gjson.ValidBytes(updated))
			require.False(t, gjson.GetBytes(updated, "tools").Exists())
			require.Equal(t, "additional_tools", gjson.GetBytes(updated, "input.0.type").String())
			require.Equal(t, "shell", gjson.GetBytes(updated, "input.0.tools.0.tools.0.name").String())
			require.Equal(t, metadata, gjson.GetBytes(updated, "metadata").Raw)

			repeated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(updated)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, updated, repeated)
		})
	}
}

// 首个是 null、末个是数组：同样必须交给原路径，不能在前置里判成「非数组」。
func TestLitePayloadDuplicateToolsKeyNullThenArray(t *testing.T) {
	body := []byte(`{"tools":null,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"input":[],"parallel_tool_calls":false,"reasoning":{"context":"all_turns"}}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "tools").Exists())
}

// 类型校验本身没有放宽：真正的非数组 tools 仍必须报同样的错，只是由原路径给出。
func TestLitePayloadNonArrayToolsStillRejected(t *testing.T) {
	body := []byte(`{"tools":"not-an-array","input":[],"parallel_tool_calls":false,"reasoning":{"context":"all_turns"}}`)
	_, _, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.ErrorContains(t, err, "responses Lite requires tools to be an array")
}
