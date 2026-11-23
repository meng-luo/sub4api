package service

import "bytes"

// hasJSONUnicodeEscape 判断原始 JSON 字节里是否出现 \uXXXX 转义。
//
// 若干零拷贝前置优化用「原始字节字面量匹配」代替 JSON 解析来证明某个键或取值
// 不存在（例如 bytes.Contains(body, []byte("item_reference"))）。这种推理只在
// 原始字节与解码后文本一致时成立：JSON 允许把任意字符写成 \uXXXX，例如
// "\u0069tem_reference" 解码后仍是 item_reference，字面量却搜不到。
//
// 因此凡是要用字面量匹配下否定结论的地方，都必须先排除 \u 转义。其余常见转义
// （\n、\t、\"、\\、\/）不会改变标识符字面，无需一并排除，这样真机请求体
// （大量 \n 与 \" 的代码内容）仍能命中快速路径。
//
// 代价是一次 memmem（SIMD indexByte），相对被跳过的解析可以忽略。
func hasJSONUnicodeEscape(body []byte) bool {
	return bytes.Contains(body, []byte(`\u`))
}
