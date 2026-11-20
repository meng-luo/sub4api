// Command openai-transport-plugin 是 Sub4API 自研的 OpenAI OAuth 出站传输插件。
//
// 它通过 go-plugin + gRPC 与宿主通信，实现 pkg/pluginapi/v1 的 TransportPlugin
// 契约；所有影响出站字节形状的行为都由 Config 显式控制。
package main

import (
	pluginv1 "github.com/MACOS-DO/sub4api/pkg/pluginapi/v1"
	"github.com/MACOS-DO/sub4api/pluginimpl/openaitransport"
)

func main() {
	pluginv1.Serve(openaitransport.New())
}
