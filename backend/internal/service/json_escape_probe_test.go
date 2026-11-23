//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONMayEscapePlainASCII(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"no escapes", `{"input":[{"type":"message","content":"hi"}]}`, false},
		{"ansi control escape from shell output", `{"output":"\u001b[31mred\u001b[0m"}`, false},
		{"go html-safe escapes", `{"text":"a \u003c b \u0026 c \u003e d"}`, false},
		{"quote apostrophe and backslash escapes", `{"text":"\u0022\u0027\u005c"}`, false},
		{"non-ascii and surrogate pair", `{"text":"caf\u00e9 \ud83d\ude00 \u4e2d"}`, false},
		{"escaped letter hides a keyword", `{"type":"\u0063ompaction_trigger"}`, true},
		{"upper-case hex escaped underscore", `{"type":"input\u005Fimage"}`, true},
		{"escaped digit", `{"n":"\u0031"}`, true},
		{"escaped backslash before u is literal text", `{"code":"\\u0041"}`, false},
		{"escaped backslash in a path", `{"cwd":"C:\\users\\dev","re":"\\u{1F600}"}`, false},
		{"escape after an escaped backslash", `{"t":"\\\u0063"}`, true},
		{"truncated escape", `{"t":"\u00`, true},
		{"invalid hex", `{"t":"\uZZZZ"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, JSONMayEscapePlainASCII([]byte(tc.body)))
		})
	}
}

func TestHasJSONUnicodeEscape(t *testing.T) {
	require.False(t, hasJSONUnicodeEscape([]byte(`{"a":"b\n\t\"c\\d"}`)))
	require.True(t, hasJSONUnicodeEscape([]byte(`{"a":"\u0074ools"}`)))
	require.True(t, hasJSONUnicodeEscape([]byte(`{"\u0061":1}`)))
}
