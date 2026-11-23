//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// countRootJSONKey 统计根对象上名为 key 的成员个数。
// 用顶层遍历而非字节计数，避免被字符串值里的同名文本干扰。
func countRootJSONKey(body, key string) int {
	n := 0
	gjson.Parse(body).ForEach(func(k, _ gjson.Result) bool {
		if k.Str == key {
			n++
		}
		return true
	})
	return n
}

// deleteRootJSONField 是纯字节拼接，索引算术容易出错，
// 这里用单元表把各种成员位置与重复形态钉住（端到端行为另有 lite 载荷用例覆盖）。
func TestDeleteRootJSONFieldShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"field_first", `{"tools":1,"a":2}`, `{"a":2}`},
		{"field_last", `{"a":2,"tools":1}`, `{"a":2}`},
		{"field_middle", `{"a":1,"tools":2,"b":3}`, `{"a":1,"b":3}`},
		{"dup_split", `{"tools":1,"a":2,"tools":3}`, `{"a":2}`},
		{"dup_adjacent", `{"tools":1,"tools":2,"a":3}`, `{"a":3}`},
		{"dup_all", `{"tools":1,"tools":2}`, `{}`},
		{"only_key", `{"tools":1}`, `{}`},
		{"nested_same_name", `{"a":{"tools":1},"tools":2}`, `{"a":{"tools":1}}`},
		{"string_value", `{"a":"has \"tools\" inside","tools":2}`, `{"a":"has \"tools\" inside"}`},
		{"absent", `{"a":1}`, `{"a":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := deleteRootJSONField([]byte(c.in), "tools")
			require.NoError(t, err)
			require.True(t, json.Valid(got), "输出必须是合法 JSON: %s", got)
			require.False(t, gjson.GetBytes(got, "tools").Exists(), "顶层 tools 必须被摘除")

			var wantAny, gotAny any
			require.NoError(t, json.Unmarshal([]byte(c.want), &wantAny))
			require.NoError(t, json.Unmarshal(got, &gotAny))
			require.Equal(t, wantAny, gotAny)
		})
	}
}

func TestRootJSONFieldEditsPreserveSingleKeyBytes(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(" \n" + `{ "tools" : null , "opaque":{"tools":7,"number":9007199254740993} }` + "\t"),
		[]byte(`{ "opaque":1 , "\u0074ools" : [1] }`),
		[]byte(`{ "opaque":{"tools":7} }`),
	} {
		raw := []byte(`["<new>"]`)
		wantSet, err := sjson.SetRawBytes(body, "tools", raw)
		require.NoError(t, err)
		gotSet, err := setRootJSONField(body, "tools", raw)
		require.NoError(t, err)
		require.Equal(t, wantSet, gotSet)

		wantDelete, err := sjson.DeleteBytes(body, "tools")
		require.NoError(t, err)
		gotDelete, err := deleteRootJSONField(body, "tools")
		require.NoError(t, err)
		require.Equal(t, wantDelete, gotDelete)
	}
}

// 重复 reasoning 键且值需要改写时，回写分支必须也只留一份。
// sjson.SetRawBytes 只覆盖首个 occurrence，末个（解码路径实际采用的那个）若残留，
// 上游按末值取值会让改写形同无效。
func TestLitePayloadDuplicateReasoningKeyRewritesLastValueInPlace(t *testing.T) {
	body := []byte(`{"input":[],"parallel_tool_calls":false,"reasoning":{"context":"x","effort":"low"},"metadata":{},"reasoning":{"context":"y","effort":"high"}}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, json.Valid(updated), "输出必须是合法 JSON")

	require.Equal(t, 1, countRootJSONKey(string(updated), "reasoning"),
		"重复的 reasoning 键必须收敛为一份")
	require.Equal(t, "all_turns", gjson.GetBytes(updated, "reasoning.context").String(),
		"改写必须以末值语义为准并真正落到唯一一份上")
	require.Equal(t, "high", gjson.GetBytes(updated, "reasoning.effort").String())
	require.Less(t, strings.Index(string(updated), `"reasoning"`),
		strings.Index(string(updated), `"metadata"`), "保留首个键的位置")
}

func TestLitePayloadDuplicateRewrittenFields(t *testing.T) {
	body := []byte(`{"\u0069nput":"old input","instructions":"old instructions","parallel_tool_calls":false,"metadata":{ "sequence": 900719925474099312345 },"input":"current input","instructions":"current instructions","parallel_tool_calls":true}`)
	updated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, json.Valid(updated))
	for _, field := range []string{"input", "instructions", "parallel_tool_calls"} {
		require.Equal(t, 1, countRootJSONKey(string(updated), field), field)
	}
	require.Equal(t, "additional_tools", gjson.GetBytes(updated, "input.0.type").String())
	require.Equal(t, "current instructions", gjson.GetBytes(updated, "input.1.content.0.text").String())
	require.Equal(t, "current input", gjson.GetBytes(updated, "input.2.content.0.text").String())
	require.Equal(t, `""`, gjson.GetBytes(updated, "instructions").Raw)
	require.Equal(t, "false", gjson.GetBytes(updated, "parallel_tool_calls").Raw)
	require.Equal(t, gjson.GetBytes(body, "metadata").Raw, gjson.GetBytes(updated, "metadata").Raw)

	repeated, changed, err := normalizeOpenAIResponsesLiteToolsPayload(updated)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, updated, repeated)
}

func TestLitePayloadDuplicateParallelToolCallsAPIKey(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":false,"tools":[{"type":"function","name":"lookup"}],"parallel_tool_calls":true}`)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	updated, changed, err := normalizeOpenAIResponsesLitePayloadForAccount(body, account)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, json.Valid(updated))
	require.Equal(t, 1, countRootJSONKey(string(updated), "parallel_tool_calls"))
	require.Equal(t, "false", gjson.GetBytes(updated, "parallel_tool_calls").Raw)
	require.Equal(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(updated, "tools").Raw)
}
