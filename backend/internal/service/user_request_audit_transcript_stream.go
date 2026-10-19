package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Streaming slots keep item/content identity so split tool arguments accumulate
// while terminal snapshots replace their deltas instead of being shown twice.
func auditDecodeWire(raw string, id int64) []UserRequestAuditMessage {
	var doc any
	if json.Unmarshal([]byte(raw), &doc) == nil {
		return auditDecodeObject(doc, "assistant", id)
	}
	// The builder is separately allocated: growing slots never copies a used
	// strings.Builder. Materialize content only after all stream events.
	type slot struct {
		content *strings.Builder
		message UserRequestAuditMessage
	}
	var slots []slot
	positions := map[string]int{}
	put := func(key, kind, text, name, call string, replace bool) {
		i, exists := positions[key]
		if !exists {
			i = len(slots)
			positions[key] = i
			slots = append(slots, slot{content: new(strings.Builder), message: UserRequestAuditMessage{Role: "assistant", Kind: kind, RequestID: id}})
		}
		m := &slots[i].message
		if replace {
			slots[i].content.Reset()
		}
		_, _ = slots[i].content.WriteString(text)
		if name != "" {
			m.ToolName = name
		}
		if call != "" {
			m.CallID = call
		}
	}
	keyIndex := func(m map[string]any, k string) string {
		if v, ok := m[k]; ok {
			return fmt.Sprint(v)
		}
		return "0"
	}
	terminal := false
	var final []UserRequestAuditMessage
	var errors []UserRequestAuditMessage
	// Blank-line framing is not required for archived one-line JSON events;
	// support standard multi-data-line frames as well.
	var data []string
	consume := func(payload string) {
		if payload == "[DONE]" {
			terminal = true
			return
		}
		var e map[string]any
		if json.Unmarshal([]byte(payload), &e) != nil {
			errors = append(errors, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: "Incomplete/unparsed stream event: " + payload, RequestID: id, Incomplete: true})
			return
		}
		t := auditString(e, "type")
		output := keyIndex(e, "output_index")
		content := keyIndex(e, "content_index")
		summary := keyIndex(e, "summary_index")
		delta := auditString(e, "delta")
		switch t {
		case "response.output_text.delta":
			put("r/"+output+"/text/"+content, "text", delta, "", "", false)
		case "response.output_text.done":
			put("r/"+output+"/text/"+content, "text", auditString(e, "text"), "", "", true)
		case "response.reasoning_text.delta":
			put("r/"+output+"/think/"+content, "think", delta, "", "", false)
		case "response.reasoning_summary_text.delta":
			put("r/"+output+"/summary/"+summary, "think", delta, "", "", false)
		case "response.reasoning_text.done":
			put("r/"+output+"/think/"+content, "think", auditString(e, "text"), "", "", true)
		case "response.reasoning_summary_text.done":
			put("r/"+output+"/summary/"+summary, "think", auditString(e, "text"), "", "", true)
		case "response.function_call_arguments.delta":
			put("r/"+output+"/tool", "tool_call", delta, "", "", false)
		case "response.function_call_arguments.done":
			put("r/"+output+"/tool", "tool_call", auditString(e, "arguments"), auditString(e, "name"), auditString(e, "call_id"), true)
		case "response.output_item.added", "response.output_item.done":
			item, _ := e["item"].(map[string]any)
			if auditString(item, "type") == "function_call" {
				put("r/"+output+"/tool", "tool_call", auditString(item, "arguments"), auditString(item, "name"), auditString(item, "call_id"), true)
			}
			if t == "response.output_item.done" && auditString(item, "type") != "function_call" {
				for _, field := range []string{"content", "summary"} {
					parts, _ := item[field].([]any)
					for index, part := range parts {
						for _, m := range auditDecodeObject(part, "assistant", id) {
							lane := "text"
							if m.Kind == "think" {
								lane = "think"
								if field == "summary" {
									lane = "summary"
								}
							}
							put(fmt.Sprintf("r/%s/%s/%d", output, lane, index), m.Kind, m.Content, m.ToolName, m.CallID, true)
						}
					}
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
			terminal = true
			if response, ok := e["response"].(map[string]any); ok {
				if output, ok := response["output"].([]any); ok && len(output) > 0 {
					final = auditDecodeObject(output, "assistant", id)
				}
			}
			if t != "response.completed" {
				errors = append(errors, UserRequestAuditMessage{Role: "system", Kind: "error", Content: payload, RequestID: id, Incomplete: true})
			}
		case "content_block_start":
			b, _ := e["content_block"].(map[string]any)
			key := "a/" + keyIndex(e, "index")
			switch auditString(b, "type") {
			case "text":
				put(key, "text", auditString(b, "text"), "", "", true)
			case "thinking":
				put(key, "think", auditString(b, "thinking"), "", "", true)
			case "tool_use":
				args := ""
				if input, ok := b["input"].(map[string]any); ok && len(input) > 0 {
					args = auditJSONText(input)
				}
				put(key, "tool_call", args, auditString(b, "name"), auditString(b, "id"), true)
			default:
				errors = append(errors, auditDecodeObject(b, "assistant", id)...)
			}
		case "content_block_delta":
			d, _ := e["delta"].(map[string]any)
			key := "a/" + keyIndex(e, "index")
			switch auditString(d, "type") {
			case "text_delta":
				put(key, "text", auditString(d, "text"), "", "", false)
			case "thinking_delta":
				put(key, "think", auditString(d, "thinking"), "", "", false)
			case "input_json_delta":
				put(key, "tool_call", auditString(d, "partial_json"), "", "", false)
			}
		case "message_stop":
			terminal = true
		case "error":
			errors = append(errors, UserRequestAuditMessage{Role: "system", Kind: "error", Content: payload, RequestID: id, Incomplete: true})
		}
		if choices, ok := e["choices"].([]any); ok {
			for _, rawChoice := range choices {
				c, ok := rawChoice.(map[string]any)
				if !ok {
					continue
				}
				base := "c/" + keyIndex(c, "index")
				if c["finish_reason"] != nil {
					terminal = true
				}
				if message, ok := c["message"].(map[string]any); ok {
					final = append(final, auditDecodeObject(message, "assistant", id)...)
				}
				d, _ := c["delta"].(map[string]any)
				if s := auditString(d, "reasoning_content"); s != "" {
					put(base+"/think", "think", s, "", "", false)
				}
				if s := auditString(d, "content"); s != "" {
					put(base+"/text", "text", s, "", "", false)
				}
				if calls, ok := d["tool_calls"].([]any); ok {
					for _, rawCall := range calls {
						call, ok := rawCall.(map[string]any)
						if !ok {
							continue
						}
						f, _ := call["function"].(map[string]any)
						put(base+"/tool/"+keyIndex(call, "index"), "tool_call", auditString(f, "arguments"), auditString(f, "name"), auditString(call, "id"), false)
					}
				}
			}
		}
	}
	flush := func() {
		if len(data) > 0 {
			consume(strings.Join(data, "\n"))
			data = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			// Some old WS captures use data lines without blank separators.
			if len(data) == 1 && json.Valid([]byte(data[0])) {
				flush()
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		} else if strings.HasPrefix(line, "{") {
			flush()
			consume(line)
		}
	}
	flush()
	for i := range slots {
		m := &slots[i].message
		m.Content = slots[i].content.String()
		if m.Kind == "think" {
			m.Reasoning = m.Content
		}
	}
	out := make([]UserRequestAuditMessage, 0, len(slots)+len(final)+len(errors)+1)
	if len(final) > 0 {
		// Final output is authoritative for text/tools. Preserve streamed THINK
		// omitted from the final snapshot, which often contains encrypted state.
		for _, s := range slots {
			if s.message.Kind == "think" {
				found := false
				for _, m := range final {
					found = found || m.Kind == "think" && m.Content == s.message.Content
				}
				if !found {
					out = append(out, s.message)
				}
			}
		}
		out = append(out, final...)
	} else {
		for _, s := range slots {
			if s.message.Content != "" || s.message.Kind == "tool_call" {
				m := s.message
				m.Incomplete = !terminal
				out = append(out, m)
			}
		}
	}
	out = append(out, errors...)
	if !terminal {
		out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: "Stream terminal event unavailable; captured response may be interrupted or truncated.", RequestID: id, Incomplete: true})
	}
	if len(slots) == 0 && len(final) == 0 && len(errors) == 0 {
		out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: raw, RequestID: id, Incomplete: true})
	}
	var expanded []UserRequestAuditMessage
	for i, m := range out {
		if m.Kind == "text" && strings.Contains(m.Content, "<think>") {
			if expanded == nil {
				expanded = make([]UserRequestAuditMessage, 0, len(out)+2)
				expanded = append(expanded, out[:i]...)
			}
			parts := auditDecodeValueText(m.Content, m.Role, m.RequestID)
			for j := range parts {
				parts[j].Incomplete = parts[j].Incomplete || m.Incomplete
			}
			expanded = append(expanded, parts...)
		} else if expanded != nil {
			expanded = append(expanded, m)
		}
	}
	if expanded != nil {
		out = expanded
	}
	return out
}
