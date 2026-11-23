package service

import (
	"sync/atomic"

	coderws "github.com/coder/websocket"

	"github.com/MACOS-DO/sub4api/internal/config"
)

// WebSocket permessage-deflate 的两条腿各自独立开关。
//
// 与 codexIdentityEnforcement / codexEmitVersionHeader 同一模式：握手点分散在
// handler（接客户端）与 service（连上游）两个包，拿不到 *config.Config，因此在
// 装配期发布进程级快照，热路径只做一次原子读。
var (
	openAIWSIngressCompression  atomic.Int32
	openAIWSUpstreamCompression atomic.Int32
)

func init() {
	openAIWSIngressCompression.Store(int32(resolveOpenAIWSCompressionMode(config.DefaultOpenAIWSCompression)))
	openAIWSUpstreamCompression.Store(int32(resolveOpenAIWSCompressionMode(config.DefaultOpenAIWSCompression)))
}

// resolveOpenAIWSCompressionMode 把配置枚举映射为 coder/websocket 的压缩模式。
// 未知值一律退到 CompressionDisabled：握手上少一个扩展总比多点开销安全。
func resolveOpenAIWSCompressionMode(mode string) coderws.CompressionMode {
	switch config.NormalizeOpenAIWSCompressionMode(mode) {
	case config.OpenAIWSCompressionContextTakeover:
		return coderws.CompressionContextTakeover
	case config.OpenAIWSCompressionDisabled:
		return coderws.CompressionDisabled
	default:
		return coderws.CompressionDisabled
	}
}

// SetOpenAIWSCompressionModes 发布两条腿的压缩策略，由装配期调用。
// 空串按 DefaultOpenAIWSCompression 处理。
func SetOpenAIWSCompressionModes(ingress, upstream string) {
	openAIWSIngressCompression.Store(int32(resolveOpenAIWSCompressionMode(ingress)))
	openAIWSUpstreamCompression.Store(int32(resolveOpenAIWSCompressionMode(upstream)))
}

// OpenAIWSIngressCompressionMode 供 handler 在 Accept 时读取客户端腿的压缩策略。
func OpenAIWSIngressCompressionMode() coderws.CompressionMode {
	return coderws.CompressionMode(openAIWSIngressCompression.Load())
}

// openAIWSUpstreamCompressionMode 供 WS dialer 在握手时读取上游腿的压缩策略。
func openAIWSUpstreamCompressionMode() coderws.CompressionMode {
	return coderws.CompressionMode(openAIWSUpstreamCompression.Load())
}
