package service

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These helpers take plain ASCII identifier keys. Values are read
// with encoding/json's last-member semantics. Writes keep the first key in
// place and remove later copies so only the written value can take effect.
func lastRootJSONField(body []byte, field string) gjson.Result {
	value, _ := lastJSONObjectField(parseRawJSONView(body), field)
	return value
}

func lastJSONObjectField(object gjson.Result, field string) (value gjson.Result, repeated bool) {
	first := object.Get(field)
	if !first.Exists() || !object.IsObject() {
		return first, false
	}
	// A duplicate must follow the first value. Skip preceding fields and
	// the value itself, which can contain a large messages or input array.
	if end := first.Index - object.Index + len(first.Raw); end >= 0 && end <= len(object.Raw) {
		tail := object.Raw[end:]
		if !strings.Contains(tail, `"`+field+`"`) && !strings.Contains(tail, `\u`) {
			return first, false
		}
	}
	object.ForEach(func(key, member gjson.Result) bool {
		if key.Str == field {
			repeated = value.Exists()
			value = member
		}
		return true
	})
	return value, repeated
}

func jsonObjectFieldMayRepeat(object gjson.Result, field string) bool {
	// A key can also be written with escapes. When the raw check is
	// inconclusive, look at the decoded names of direct members only.
	return object.IsObject() && (strings.Count(object.Raw, `"`+field+`"`) > 1 ||
		strings.Contains(object.Raw, `\u`))
}

func setRootJSONField(body []byte, field string, raw []byte) ([]byte, error) {
	return sjson.SetRawBytes(withoutDuplicateRootJSONField(body, field), field, raw)
}

func setRootJSONValue(body []byte, field string, value any) ([]byte, error) {
	return sjson.SetBytes(withoutDuplicateRootJSONField(body, field), field, value)
}

func withoutDuplicateRootJSONField(body []byte, field string) []byte {
	object := parseRawJSONView(body)
	if !jsonObjectFieldMayRepeat(object, field) {
		return body
	}
	return removeRootJSONFieldMembers(body, object, field, true)
}

func deleteRootJSONField(body []byte, field string) ([]byte, error) {
	object := parseRawJSONView(body)
	value, repeated := lastJSONObjectField(object, field)
	if !value.Exists() {
		return body, nil
	}
	if !repeated {
		// Preserve sjson's whitespace handling for ordinary requests.
		return sjson.DeleteBytes(body, field)
	}
	return removeRootJSONFieldMembers(body, object, field, false), nil
}

// removeRootJSONFieldMembers 删除根对象上名为 field 的成员。
// keepFirst 为 true 时只摘除后续重复。没有成员需要删除时返回原始切片。
func removeRootJSONFieldMembers(body []byte, object gjson.Result, field string, keepFirst bool) []byte {
	var out []byte
	cursor, lastKeptEnd := 0, -1
	removeStart, removeEnd := -1, 0
	keptFirst := false
	object.ForEach(func(key, value gjson.Result) bool {
		valueEnd := value.Index + len(value.Raw)
		if key.Str == field {
			if keepFirst && !keptFirst {
				// 首个成员视为保留成员，后续删除需要它作为逗号参照。
				keptFirst = true
				lastKeptEnd = valueEnd
				return true
			}
			if out == nil {
				out = make([]byte, 0, len(body)-(valueEnd-key.Index))
			}
			if removeStart < 0 {
				removeStart = key.Index
				if lastKeptEnd >= 0 {
					removeStart = lastKeptEnd
				}
			}
			removeEnd = valueEnd
			return true
		}
		if removeStart >= 0 {
			out = append(out, body[cursor:removeStart]...)
			cursor = removeEnd // Keep the comma after the removed run.
			if lastKeptEnd < 0 {
				cursor = key.Index // A leading run has no preceding member.
			}
			removeStart = -1
		}
		lastKeptEnd = valueEnd
		return true
	})
	if out == nil {
		return body
	}
	if removeStart >= 0 {
		out = append(out, body[cursor:removeStart]...)
		cursor = removeEnd
	}
	return append(out, body[cursor:]...)
}
