//go:build unit

package service

import (
	"testing"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/MACOS-DO/sub4api/internal/config"
)

// 默认必须保持配置引入前的行为：两条腿都协商 context takeover。
func TestOpenAIWSCompressionDefaultsPreserveHistoricalBehaviour(t *testing.T) {
	require.Equal(t, config.OpenAIWSCompressionContextTakeover, config.DefaultOpenAIWSCompression)

	var zero config.GatewayOpenAIWSConfig
	require.Equal(t, config.OpenAIWSCompressionContextTakeover, zero.EffectiveIngressCompression())
	require.Equal(t, config.OpenAIWSCompressionContextTakeover, zero.EffectiveUpstreamCompression())

	require.Equal(t, coderws.CompressionContextTakeover, resolveOpenAIWSCompressionMode(""))
	require.Equal(t, coderws.CompressionContextTakeover, OpenAIWSIngressCompressionMode())
	require.Equal(t, coderws.CompressionContextTakeover, openAIWSUpstreamCompressionMode())
}

func TestNormalizeOpenAIWSCompressionMode(t *testing.T) {
	cases := map[string]string{
		"":                   config.OpenAIWSCompressionContextTakeover,
		"   ":                config.OpenAIWSCompressionContextTakeover,
		"context_takeover":   config.OpenAIWSCompressionContextTakeover,
		"Context_Takeover":   config.OpenAIWSCompressionContextTakeover,
		"context":            config.OpenAIWSCompressionContextTakeover,
		"on":                 config.OpenAIWSCompressionContextTakeover,
		"enabled":            config.OpenAIWSCompressionContextTakeover,
		"disabled":           config.OpenAIWSCompressionDisabled,
		"DISABLED":           config.OpenAIWSCompressionDisabled,
		"off":                config.OpenAIWSCompressionDisabled,
		"none":               config.OpenAIWSCompressionDisabled,
		"context_takeover_x": "",
		"gzip":               "",
	}
	for input, want := range cases {
		require.Equal(t, want, config.NormalizeOpenAIWSCompressionMode(input), "input=%q", input)
	}
}

// 非法配置值必须回落到 disabled（握手少一个扩展优于多一份开销），
// 而 Effective* 仍返回默认值以避免调用方拿到空串。
func TestResolveOpenAIWSCompressionModeFallsBackToDisabled(t *testing.T) {
	require.Equal(t, coderws.CompressionDisabled, resolveOpenAIWSCompressionMode("gzip"))
	require.Equal(t, coderws.CompressionDisabled, resolveOpenAIWSCompressionMode("disabled"))
	require.Equal(t, coderws.CompressionContextTakeover, resolveOpenAIWSCompressionMode("context_takeover"))

	cfg := config.GatewayOpenAIWSConfig{IngressCompression: "gzip"}
	require.Equal(t, config.OpenAIWSCompressionContextTakeover, cfg.EffectiveIngressCompression())
}

// 两条腿必须能独立切换：把 ingress 关掉不影响 upstream。
func TestSetOpenAIWSCompressionModesAreIndependent(t *testing.T) {
	t.Cleanup(func() { SetOpenAIWSCompressionModes("", "") })

	SetOpenAIWSCompressionModes(config.OpenAIWSCompressionDisabled, config.OpenAIWSCompressionContextTakeover)
	require.Equal(t, coderws.CompressionDisabled, OpenAIWSIngressCompressionMode())
	require.Equal(t, coderws.CompressionContextTakeover, openAIWSUpstreamCompressionMode())

	SetOpenAIWSCompressionModes(config.OpenAIWSCompressionContextTakeover, config.OpenAIWSCompressionDisabled)
	require.Equal(t, coderws.CompressionContextTakeover, OpenAIWSIngressCompressionMode())
	require.Equal(t, coderws.CompressionDisabled, openAIWSUpstreamCompressionMode())
}
