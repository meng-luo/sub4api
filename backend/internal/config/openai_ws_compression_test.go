package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaultOpenAIWSCompressionKeepsHistoricalBehaviour(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	require.NoError(t, err)
	// 配置项引入前两条腿都协商压缩；引入后默认值必须保持不变。
	require.Equal(t, OpenAIWSCompressionContextTakeover, cfg.Gateway.OpenAIWS.EffectiveIngressCompression())
	require.Equal(t, OpenAIWSCompressionContextTakeover, cfg.Gateway.OpenAIWS.EffectiveUpstreamCompression())
	require.NoError(t, cfg.Validate())
}

func TestLoadOpenAIWSCompressionFromEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_OPENAI_WS_INGRESS_COMPRESSION", "disabled")
	t.Setenv("GATEWAY_OPENAI_WS_UPSTREAM_COMPRESSION", "context_takeover")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, OpenAIWSCompressionDisabled, cfg.Gateway.OpenAIWS.EffectiveIngressCompression())
	require.Equal(t, OpenAIWSCompressionContextTakeover, cfg.Gateway.OpenAIWS.EffectiveUpstreamCompression())
	require.NoError(t, cfg.Validate())
}

// 拼错的值必须在启动阶段报错，而不是静默回落成另一种握手形态。
func TestValidateRejectsUnknownOpenAIWSCompressionMode(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	cfg.Gateway.OpenAIWS.IngressCompression = "context_takeover"
	cfg.Gateway.OpenAIWS.UpstreamCompression = "context_takeover"
	require.NoError(t, cfg.Validate())

	cfg.Gateway.OpenAIWS.IngressCompression = "zstd"
	require.ErrorContains(t, cfg.Validate(), "gateway.openai_ws.ingress_compression")

	cfg.Gateway.OpenAIWS.IngressCompression = ""
	cfg.Gateway.OpenAIWS.UpstreamCompression = "brotli"
	require.ErrorContains(t, cfg.Validate(), "gateway.openai_ws.upstream_compression")
}
