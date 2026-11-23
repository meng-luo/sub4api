//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MACOS-DO/sub4api/internal/pkg/claude"
)

// beta 门控字段的删除必须覆盖全部重复键：sjson.DeleteBytes 只删首个 occurrence，
// 残留的那份会带着门控字段发往上游，而函数仍报告 deleted=true。
func TestStripAnthropicBodyFieldUnlessBetaRemovesDuplicateKeys(t *testing.T) {
	for _, field := range []string{"context_management", "fallbacks", "fallback_credit_token"} {
		t.Run(field, func(t *testing.T) {
			body := []byte(`{"` + field + `":{"enabled":true},"x":1,"` + field + `":{"enabled":false}}`)
			out, changed := sanitizeAnthropicBodyForBetaTokens(body, "oauth-2025-04-20")
			require.True(t, changed)
			require.True(t, json.Valid(out))
			require.False(t, gjson.GetBytes(out, field).Exists(),
				"重复键必须全部删除，否则门控字段仍会发往上游: %s", out)
			require.Equal(t, "1", gjson.GetBytes(out, "x").Raw, "无关字段必须保留")
		})
	}
}

// 带所需 beta token 时必须原样保留（含重复键，不做改写）。
func TestStripAnthropicBodyFieldUnlessBetaKeepsWhenTokenPresent(t *testing.T) {
	body := []byte(`{"fallbacks":[],"fallbacks":[]}`)
	out, deleted := stripAnthropicBodyFieldUnlessBeta(body, "fallbacks", claude.BetaServerSideFallback, claude.BetaServerSideFallback)
	require.False(t, deleted)
	require.Equal(t, string(body), string(out))
}

// 嵌套路径应删除内层字段，并保留根对象上含点的同名键。
func TestDeleteSanitizedBodyFieldKeepsNestedSemantics(t *testing.T) {
	body := []byte(`{"thinking":{"block_binding":true,"type":"enabled"},"thinking.block_binding":false}`)
	out, err := deleteSanitizedBodyField(body, "thinking.block_binding")
	require.NoError(t, err)
	require.Equal(t, `{"thinking":{"type":"enabled"},"thinking.block_binding":false}`, string(out))
}

// 单份键时行为必须与 sjson.DeleteBytes 逐字节一致（常温路径不变）。
func TestDeleteSanitizedBodyFieldMatchesSjsonForSingleKey(t *testing.T) {
	body := []byte(`{ "fallbacks" : [1] , "x" : 2 }`)
	want, err := sjson.DeleteBytes(body, "fallbacks")
	require.NoError(t, err)
	out, err := deleteSanitizedBodyField(body, "fallbacks")
	require.NoError(t, err)
	require.Equal(t, want, out)
}

// requiredTokens 为空表示无条件剥离：上一条用例漏传 token 时曾误判，
// 这里把该语义显式钉住，避免调用方误写。
func TestStripAnthropicBodyFieldUnlessBetaEmptyTokensAlwaysStrips(t *testing.T) {
	body := []byte(`{"fallbacks":[],"x":1}`)
	out, deleted := stripAnthropicBodyFieldUnlessBeta(body, "fallbacks", claude.BetaServerSideFallback)
	require.True(t, deleted)
	require.False(t, gjson.GetBytes(out, "fallbacks").Exists())
}
