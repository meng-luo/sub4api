package repository

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MACOS-DO/sub4api/internal/config"
)

const codexCompressionTargetURL = "https://chatgpt.com/backend-api/codex/responses"

func newCodexCompressionRequest(t *testing.T, size int) *http.Request {
	t.Helper()
	payload := bytes.Repeat([]byte("a"), size)
	req, err := http.NewRequest(http.MethodPost, codexCompressionTargetURL, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func requireCodexBodyLength(t *testing.T, req *http.Request, want int) {
	t.Helper()
	require.Equal(t, int64(want), req.ContentLength)
	replay, err := req.GetBody()
	require.NoError(t, err)
	defer func() { _ = replay.Close() }()
	data, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.Len(t, data, want)
}

// by_size 模式下，未压缩体积不超过阈值时必须保持官方明文形态。
func TestCompressCodexResponsesRequestBySizeKeepsSmallBodyPlain(t *testing.T) {
	const size = 7 * 1024 * 1024
	req := newCodexCompressionRequest(t, size)

	applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionBySize, minBytes: config.DefaultCodexRequestCompressionMinBytes})
	require.False(t, applied)
	require.Empty(t, req.Header.Get("Content-Encoding"))
	requireCodexBodyLength(t, req, size)
}

// by_size 模式下，超过阈值才压缩，且重试可复用同一份压缩字节。
func TestCompressCodexResponsesRequestBySizeCompressesLargeBody(t *testing.T) {
	const size = 9 * 1024 * 1024
	req := newCodexCompressionRequest(t, size)

	applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionBySize, minBytes: config.DefaultCodexRequestCompressionMinBytes})
	require.True(t, applied)
	require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
	require.Less(t, req.ContentLength, int64(size))

	first, err := req.GetBody()
	require.NoError(t, err)
	defer func() { _ = first.Close() }()
	firstBytes, err := io.ReadAll(first)
	require.NoError(t, err)
	require.Equal(t, req.ContentLength, int64(len(firstBytes)))

	second, err := req.GetBody()
	require.NoError(t, err)
	defer func() { _ = second.Close() }()
	secondBytes, err := io.ReadAll(second)
	require.NoError(t, err)
	require.Equal(t, firstBytes, secondBytes, "重试必须复用同一份压缩结果")
}

// off 模式永不压缩，即使请求体远大于阈值。
func TestCompressCodexResponsesRequestOffNeverCompresses(t *testing.T) {
	const size = 9 * 1024 * 1024
	req := newCodexCompressionRequest(t, size)

	applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionOff, minBytes: 1})
	require.False(t, applied)
	require.Empty(t, req.Header.Get("Content-Encoding"))
	requireCodexBodyLength(t, req, size)
}

// on 模式不设阈值；未知长度时 by_size 回落到实测长度判定。
func TestCompressCodexResponsesRequestOnAndUnknownLength(t *testing.T) {
	t.Run("on ignores threshold", func(t *testing.T) {
		req := newCodexCompressionRequest(t, 32)
		applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionOn, minBytes: config.DefaultCodexRequestCompressionMinBytes})
		require.True(t, applied)
		require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
		require.NotEqual(t, int64(32), req.ContentLength)
	})

	t.Run("unknown length falls back to measured size", func(t *testing.T) {
		req := newCodexCompressionRequest(t, 32)
		req.ContentLength = -1
		applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionBySize, minBytes: config.DefaultCodexRequestCompressionMinBytes})
		require.False(t, applied)
		require.Empty(t, req.Header.Get("Content-Encoding"))
	})
}

// 非目标请求（其他 host / 路径 / 方法 / 已带 Content-Encoding）必须原样返回。
func TestCompressCodexResponsesRequestSkipsNonTargetRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"other host", func(r *http.Request) { r.URL.Host = "api.openai.com" }},
		{"other path", func(r *http.Request) { r.URL.Path = "/v1/responses" }},
		{"other method", func(r *http.Request) { r.Method = http.MethodGet }},
		{"already encoded", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }},
		{"not json", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newCodexCompressionRequest(t, 9*1024*1024)
			tc.mutate(req)
			applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionOn, minBytes: 0})
			require.False(t, applied)
			// 已带编码的请求原样返回：保留调用方自己的 Content-Encoding。
			if want := req.Header.Get("Content-Encoding"); want != "" {
				require.Equal(t, "gzip", want)
			}
		})
	}
}

// doUpstreamRequest 的默认策略取配置；服务与配置缺失时回落 by_size + 8 MiB。
func TestCodexRequestCompressionDefaults(t *testing.T) {
	require.Equal(t, config.CodexRequestCompressionBySize, defaultCodexRequestCompression().mode)
	require.Equal(t, config.DefaultCodexRequestCompressionMinBytes, defaultCodexRequestCompression().minBytes)

	var svc *httpUpstreamService
	require.Equal(t, defaultCodexRequestCompression(), svc.codexRequestCompression())

	svc = &httpUpstreamService{cfg: &config.Config{}}
	require.Equal(t, config.CodexRequestCompressionBySize, svc.codexRequestCompression().mode)

	svc = &httpUpstreamService{cfg: &config.Config{Gateway: config.GatewayConfig{
		Codex: config.GatewayCodexConfig{RequestCompression: config.CodexRequestCompressionOff, RequestCompressionMinBytes: 1024},
	}}}
	require.Equal(t, config.CodexRequestCompressionOff, svc.codexRequestCompression().mode)
	require.Equal(t, int64(1024), svc.codexRequestCompression().minBytes)
}

// 小请求不应该被压缩：这是与官方客户端对齐的核心断言。
func TestCompressCodexResponsesRequestSmallBodyHasNoEncodingHeader(t *testing.T) {
	req := newCodexCompressionRequest(t, 4096)
	applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionBySize, minBytes: config.DefaultCodexRequestCompressionMinBytes})
	require.False(t, applied)
	for name := range req.Header {
		require.False(t, strings.EqualFold(name, "Content-Encoding"))
	}
}
