//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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

			require.JSONEq(t, c.want, string(got))
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
