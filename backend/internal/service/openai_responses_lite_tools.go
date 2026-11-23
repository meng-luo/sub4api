package service

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
)

type openAIResponsesLiteValidationError struct {
	param   string
	message string
}

func (e *openAIResponsesLiteValidationError) Error() string { return e.message }

func newOpenAIResponsesLiteValidationError(param, format string, args ...any) error {
	return &openAIResponsesLiteValidationError{param: param, message: fmt.Sprintf(format, args...)}
}

// normalizeOpenAIResponsesLiteTools mirrors Codex's build_responses_request:
// tools and nonempty instructions precede history, reasoning covers all turns,
// and parallel calls are disabled. It also accepts already serialized Lite
// history and incremental tool-output requests without adding another prefix.
func normalizeOpenAIResponsesLiteTools(reqBody map[string]any) (bool, error) {
	if reqBody == nil {
		return false, nil
	}
	if parallel, exists := reqBody["parallel_tool_calls"]; exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if rawReasoning, exists := reqBody["reasoning"]; exists && rawReasoning != nil {
		if _, ok := rawReasoning.(map[string]any); !ok {
			return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
		}
	}
	instructions, instructionsOK := reqBody["instructions"].(string)
	if raw := reqBody["instructions"]; raw != nil && !instructionsOK {
		return false, newOpenAIResponsesLiteValidationError("instructions", "responses Lite requires instructions to be a string")
	}
	rawTools, hasTools := reqBody["tools"]
	var tools []any
	if rawTools != nil {
		var ok bool
		tools, ok = rawTools.([]any)
		if !ok {
			return false, newOpenAIResponsesLiteValidationError("tools", "responses Lite requires tools to be an array")
		}
	}
	var err error
	tools, err = groupOpenAIResponsesLiteTools(tools)
	if err != nil {
		return false, err
	}
	input := reqBody["input"]
	if hasTools || instructions != "" {
		items, err := appendOpenAIResponsesLiteAdditionalTools(input, tools)
		if err != nil {
			return false, err
		}
		if instructions != "" {
			// Only inspect the prompt prefix. Equal text in later history is not
			// the current instruction, and must not suppress its insertion.
			index := 0
			for index < len(items) {
				item, _ := items[index].(map[string]any)
				if item["type"] != "additional_tools" {
					break
				}
				index++
			}
			if index == len(items) || !isOpenAIResponsesLiteInstruction(items[index], instructions) {
				message := map[string]any{
					"type": "message", "role": "developer",
					"content": []any{map[string]any{"type": "input_text", "text": instructions}},
				}
				prefixed := make([]any, 0, len(items)+1)
				prefixed = append(prefixed, items[:index]...)
				prefixed = append(prefixed, message)
				items = append(prefixed, items[index:]...)
			}
		}
		input = items
	}
	changed := hasTools || instructions != ""
	if stripOpenAIResponsesLiteImageDetails(input) {
		changed = true
	}
	if changed {
		reqBody["input"] = input
	}
	if hasTools {
		delete(reqBody, "tools")
	}
	if hasTools || instructions != "" {
		reqBody["instructions"] = ""
	}
	reasoningChanged, err := ensureOpenAIResponsesLiteReasoningContext(reqBody)
	if err != nil {
		return false, err
	}
	return ensureOpenAIResponsesLiteParallelToolCalls(reqBody, changed || reasoningChanged)
}

// Group functions exactly as codex-tools/tool_spec.rs does, without rebuilding
// schemas or discarding provider extensions on tools/namespaces.
func groupOpenAIResponsesLiteTools(tools []any) ([]any, error) {
	grouped := make([]any, 0, len(tools))
	functions := map[string]any{"type": "namespace", "name": "functions", "description": ""}
	members := []any{}
	functionsIndex := -1
	for index, rawTool := range tools {
		if shorthand, ok := rawTool.(string); ok && strings.TrimSpace(shorthand) != "" {
			grouped = append(grouped, rawTool)
			continue
		}
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite tool at index %d must be an object", index)
		}
		switch tool["type"] {
		case "function", "custom":
			members = append(members, rawTool)
		case "namespace":
			if tool["name"] != "functions" {
				grouped = append(grouped, rawTool)
				continue
			}
			nested, ok := tool["tools"].([]any)
			if !ok {
				return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite functions namespace tools must be an array")
			}
			members = append(members, nested...)
			for key, value := range tool {
				if key == "tools" {
					continue
				}
				if key == "description" && strings.TrimSpace(firstNonEmptyString(value)) == "" {
					continue
				}
				functions[key] = value
			}
		default:
			if strings.TrimSpace(firstNonEmptyString(tool["type"])) == "" {
				return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite tool at index %d is missing type", index)
			}
			// ToolSearch and WebSearch are official ToolSpec variants. Forward
			// other typed extensions for upstream validation, never drop them.
			grouped = append(grouped, rawTool)
			continue
		}
		if functionsIndex < 0 {
			functionsIndex = len(grouped)
		}
	}
	if functionsIndex >= 0 {
		functions["tools"] = members
		grouped = append(grouped, nil)
		copy(grouped[functionsIndex+1:], grouped[functionsIndex:])
		grouped[functionsIndex] = functions
	}
	return grouped, nil
}

func isOpenAIResponsesLiteInstruction(raw any, instructions string) bool {
	item, _ := raw.(map[string]any)
	if item["role"] != "developer" || (item["type"] != nil && item["type"] != "message") {
		return false
	}
	if text, ok := item["content"].(string); ok {
		return text == instructions
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		return false
	}
	part, _ := content[0].(map[string]any)
	return part["type"] == "input_text" && part["text"] == instructions
}

func stripOpenAIResponsesLiteImageDetails(input any) bool {
	items, _ := input.([]any)
	changed := false
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		var content []any
		switch item["type"] {
		case "message", nil:
			content, _ = item["content"].([]any)
		case "function_call_output", "custom_tool_call_output":
			content, _ = item["output"].([]any)
		}
		for _, rawPart := range content {
			part, _ := rawPart.(map[string]any)
			if part["type"] == "input_image" {
				if _, exists := part["detail"]; exists {
					delete(part, "detail")
					changed = true
				}
			}
		}
	}
	return changed
}

func ensureOpenAIResponsesLiteParallelToolCalls(reqBody map[string]any, changed bool) (bool, error) {
	parallel, exists := reqBody["parallel_tool_calls"]
	if exists {
		if _, ok := parallel.(bool); !ok {
			return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
		}
	}
	if parallel == false {
		return changed, nil
	}
	reqBody["parallel_tool_calls"] = false
	return true, nil
}

func ensureOpenAIResponsesLiteReasoningContext(reqBody map[string]any) (bool, error) {
	rawReasoning, exists := reqBody["reasoning"]
	if !exists || rawReasoning == nil {
		reqBody["reasoning"] = map[string]any{"context": "all_turns"}
		return true, nil
	}
	reasoning, ok := rawReasoning.(map[string]any)
	if !ok {
		return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
	}
	if context, ok := reasoning["context"].(string); ok && context == "all_turns" {
		return false, nil
	}
	reasoning["context"] = "all_turns"
	return true, nil
}

func appendOpenAIResponsesLiteAdditionalTools(input any, moved []any) ([]any, error) {
	var items []any
	switch typed := input.(type) {
	case nil:
		items = make([]any, 0, 1)
	case string:
		items = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": typed}},
		}}
	case []any:
		items = typed
	default:
		return nil, newOpenAIResponsesLiteValidationError("input", "responses Lite tools and instructions require input to be a string or array")
	}
	// Later additional_tools items are historical declaration updates. They
	// must neither suppress the current prefix nor be moved ahead of history.
	var target map[string]any
	var existing []any
	if len(items) > 0 {
		first, _ := items[0].(map[string]any)
		if first["type"] == "additional_tools" {
			target = first
			var ok bool
			existing, ok = first["tools"].([]any)
			if !ok && first["tools"] != nil {
				return nil, newOpenAIResponsesLiteValidationError("input", "responses Lite input.additional_tools tools must be an array")
			}
		}
	}
	if len(moved) == 0 && target != nil {
		return items, nil
	}
	merged, err := mergeOpenAIResponsesLiteAdditionalTools(existing, moved)
	if err != nil {
		return nil, err
	}
	if target == nil {
		if merged == nil {
			merged = []any{}
		}
		prefix := map[string]any{"type": "additional_tools", "role": "developer", "tools": merged}
		return append([]any{prefix}, items...), nil
	}
	if reflect.DeepEqual(existing, merged) {
		return items, nil
	}
	result := append([]any(nil), items...)
	carrier := maps.Clone(target)
	carrier["tools"] = merged
	result[0] = carrier
	return result, nil
}

func mergeOpenAIResponsesLiteAdditionalTools(existing []any, moved []any) ([]any, error) {
	merged := append([]any(nil), existing...)
	seen := make(map[string]int, len(existing)+len(moved))
	for index, rawTool := range existing {
		if identity := openAIResponsesLiteToolIdentity(rawTool); identity != "" {
			seen[identity] = index
		}
	}
nextTool:
	for _, rawTool := range moved {
		identity := openAIResponsesLiteToolIdentity(rawTool)
		if identity == "" {
			for _, previous := range merged {
				if reflect.DeepEqual(previous, rawTool) {
					continue nextTool
				}
			}
		}
		if previous, exists := seen[identity]; identity != "" && exists {
			if reflect.DeepEqual(merged[previous], rawTool) {
				continue
			}
			if identity != "namespace\x00functions" {
				return nil, fmt.Errorf("responses Lite additional_tools conflicts with migrated %s", openAIResponsesLiteToolIdentityForError(rawTool))
			}
			oldNamespace, oldMapOK := merged[previous].(map[string]any)
			newNamespace, newMapOK := rawTool.(map[string]any)
			if !oldMapOK || !newMapOK {
				return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite functions namespace must be an object")
			}
			oldTools, oldOK := oldNamespace["tools"].([]any)
			newTools, newOK := newNamespace["tools"].([]any)
			if !oldOK || !newOK {
				return nil, newOpenAIResponsesLiteValidationError("tools", "responses Lite functions namespace tools must be an array")
			}
			members, err := mergeOpenAIResponsesLiteAdditionalTools(oldTools, newTools)
			if err != nil {
				return nil, err
			}
			namespace := maps.Clone(oldNamespace)
			for key, value := range newNamespace {
				if key == "tools" || (key == "description" && strings.TrimSpace(firstNonEmptyString(value)) == "") {
					continue
				}
				namespace[key] = value
			}
			namespace["tools"] = members
			merged[previous] = namespace
			continue
		}
		if identity != "" {
			seen[identity] = len(merged)
		}
		merged = append(merged, rawTool)
	}
	return merged, nil
}

func openAIResponsesLiteToolIdentity(rawTool any) string {
	tool, ok := rawTool.(map[string]any)
	if !ok {
		return ""
	}
	toolType := strings.TrimSpace(firstNonEmptyString(tool["type"]))
	name := strings.TrimSpace(firstNonEmptyString(tool["name"]))
	if toolType == "" || name == "" {
		return ""
	}
	return toolType + "\x00" + name
}

func openAIResponsesLiteToolIdentityForError(rawTool any) string {
	tool, _ := rawTool.(map[string]any)
	return fmt.Sprintf("tool type %q name %q", strings.TrimSpace(firstNonEmptyString(tool["type"])), strings.TrimSpace(firstNonEmptyString(tool["name"])))
}

// openAIResponsesLiteToolsPayloadIsCanonical 是 normalizeOpenAIResponsesLiteToolsPayload
// 的零拷贝前置判定：真机 Codex 的 responses-lite 请求体已经符合目标形态
// （无顶层 tools、instructions 为空），此时整段 Unmarshal + 逐字段重新编码
// 不可能产生任何修改。
//
// 动机（alloc_space profile，2 MiB 请求）：该函数此前单次调用触发 1 次全量
// Unmarshal 与 5 个子树的两次 Marshal（input 最大），占透传改写总分配的 40%。
//
// **前置结论必须与原解码路径逐点等价**，gjson 视图与 map[string]any 解码在三种
// 输入上语义不同，因此这三种情况一律放弃快速路径、回到原实现：
//
//  1. JSON null：解码为 nil interface（原实现视作「字段不存在」语义，例如
//     tools:null 会走 hasTools=true 重建、instructions:null 被忽略），而 gjson
//     给出 Type=Null 的 Result，两者判断不同。
//  2. 键名/取值含 \uXXXX 转义：原始字节里的字面量匹配不再可信（"\u0074ools"
//     解码后就是 tools），因此只要出现 \u 转义就不做任何字面量前置。
//  3. 重复键：编码器对同一键取后者，gjson 取首个，前置会读到错误的值。
func openAIResponsesLiteToolsPayloadIsCanonical(body []byte) (bool, error) {
	// 只对「可以证明是 JSON 对象」的请求体走前置；其余保持原解码错误语义。
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return false, nil
	}
	// 任一 \u 转义都可能藏在键名或类型值里，字面量前置整体失效，回落原路径。
	if hasJSONUnicodeEscape(body) {
		return false, nil
	}
	// 重复键：map 解码取最后一个，而前置读的是第一个。这里用原始出现次数兜底，
	// 计数只会偏大（键名出现在字符串值里），偏大即回落原路径，方向安全。
	if !openAIResponsesLiteKeyCountMatches(body, `"instructions"`, 1) ||
		!openAIResponsesLiteKeyCountMatches(body, `"parallel_tool_calls"`, 1) ||
		!openAIResponsesLiteKeyCountMatches(body, `"reasoning"`, 1) ||
		!openAIResponsesLiteKeyCountMatches(body, `"context"`, 1) {
		return false, nil
	}
	// 顶层 tools 一旦出现（数组 / 对象 / null 都算）就交回原解码路径：
	//   - 数组：需要折叠进 additional_tools；
	//   - null：原实现解成 nil，被当作 hasTools=true 走重建；
	//   - 重复键：gjson 取首个、map 解码取末个，首个为对象而末个为合法数组时，
	//     在前置里做类型校验会误报 400。
	// 因此这里只做「存在性」判断，任何类型校验都留给原实现，避免与解码语义分叉。
	if gjson.GetBytes(body, "tools").Raw != "" {
		return false, nil
	}
	instructions := gjson.GetBytes(body, "instructions")
	if instructions.Raw != "" {
		// instructions:null 在原实现里因为 raw == nil 被跳过（不报错也不改写）。
		if instructions.Type == gjson.Null {
			return false, nil
		}
		if instructions.Type != gjson.String {
			return false, newOpenAIResponsesLiteValidationError("instructions", "responses Lite requires instructions to be a string")
		}
		if instructions.String() != "" {
			return false, nil
		}
	}
	// stripOpenAIResponsesLiteImageDetails 会删除 input_image 上的 detail 字段。
	// 整个请求体不含 "detail" 时该步骤必然无修改，用一次 memmem 前置跳过。
	if bytes.Contains(body, []byte(`"detail"`)) {
		return false, nil
	}
	parallel := gjson.GetBytes(body, "parallel_tool_calls")
	if parallel.Raw != "" && parallel.Type != gjson.True && parallel.Type != gjson.False {
		return false, newOpenAIResponsesLiteValidationError("parallel_tool_calls", "responses Lite requires parallel_tool_calls to be a boolean")
	}
	// ensureOpenAIResponsesLiteParallelToolCalls 只在已显式为 false 时不动。
	if parallel.Type != gjson.False {
		return false, nil
	}
	reasoning := gjson.GetBytes(body, "reasoning")
	if reasoning.Raw != "" && reasoning.Type != gjson.Null && !reasoning.IsObject() {
		return false, newOpenAIResponsesLiteValidationError("reasoning", "responses Lite requires reasoning to be an object")
	}
	// ensureOpenAIResponsesLiteReasoningContext 在缺失或 context 非 all_turns 时改写。
	if reasoning.Type != gjson.JSON || reasoning.Get("context").String() != "all_turns" {
		return false, nil
	}
	return true, nil
}

// openAIResponsesLiteKeyCountMatches 判断原始 body 里某个带引号键名的出现次数
// 是否恰好等于最大允许次数。计数基于原始字节，因此字符串值里出现的同名文本也
// 会被计入——计数只会偏大，偏大即让调用方回落原路径。
func openAIResponsesLiteKeyCountMatches(body []byte, quotedKey string, maxOccurrences int) bool {
	return bytes.Count(body, []byte(quotedKey)) <= maxOccurrences
}

func normalizeOpenAIResponsesLiteToolsPayload(body []byte) ([]byte, bool, error) {
	if canonical, err := openAIResponsesLiteToolsPayloadIsCanonical(body); err != nil {
		return body, false, err
	} else if canonical {
		return body, false, nil
	}
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	// Preserve untouched request fields verbatim, including unknown extensions
	// and large integers. Do not reorder/rebuild the request for CLI cosmetics.
	fields := []string{"input", "tools", "instructions", "reasoning", "parallel_tool_calls"}
	before := make(map[string][]byte, len(fields))
	for _, field := range fields {
		if value, exists := requestBody[field]; exists {
			encoded, err := marshalOpenAIUpstreamJSON(value)
			if err != nil {
				return body, false, err
			}
			before[field] = encoded
		}
	}
	changed, err := normalizeOpenAIResponsesLiteTools(requestBody)
	if err != nil || !changed {
		return body, false, err
	}
	updated := body
	for _, field := range fields {
		value, exists := requestBody[field]
		if !exists {
			if _, existed := before[field]; existed {
				updated, err = deleteRootJSONField(updated, field)
			}
		} else {
			var encoded []byte
			encoded, err = marshalOpenAIUpstreamJSON(value)
			if err == nil && !bytes.Equal(before[field], encoded) {
				updated, err = setRootJSONField(updated, field, encoded)
			}
		}
		if err != nil {
			return body, false, fmt.Errorf("normalize responses Lite %s: %w", field, err)
		}
	}
	return updated, true, nil
}

func normalizeOpenAIResponsesLiteParallelToolCallsPayload(body []byte) ([]byte, bool, error) {
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, false, fmt.Errorf("decode responses Lite request body: %w", err)
	}
	changed, err := ensureOpenAIResponsesLiteParallelToolCalls(requestBody, false)
	if err != nil || !changed {
		return body, false, err
	}
	updated, err := setRootJSONField(body, "parallel_tool_calls", []byte("false"))
	if err != nil {
		return body, false, fmt.Errorf("normalize responses Lite parallel_tool_calls: %w", err)
	}
	return updated, true, nil
}

func normalizeOpenAIResponsesLitePayloadForAccount(body []byte, account *Account) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAI() {
		return body, false, nil
	}
	if account.IsOpenAIOAuthLike() {
		return normalizeOpenAIResponsesLiteToolsPayload(body)
	}
	return normalizeOpenAIResponsesLiteParallelToolCallsPayload(body)
}
