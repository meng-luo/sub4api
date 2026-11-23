package repository

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/MACOS-DO/sub4api/internal/config"
)

const codexCompressionTargetURL = "https://chatgpt.com/backend-api/codex/responses"

// codexCompressionTestThreshold 是 by_size 边界用例显式使用的阈值：这些用例验证的是
// 「阈值上下的取舍机制」，与 DefaultCodexRequestCompressionMinBytes 的取值解耦，
// 以免默认阈值调整时连带改动边界样本的体积。
const codexCompressionTestThreshold int64 = 8 << 20

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

	applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionBySize, minBytes: codexCompressionTestThreshold})
	require.False(t, applied)
	require.Empty(t, req.Header.Get("Content-Encoding"))
	requireCodexBodyLength(t, req, size)
}

// by_size 模式下，超过阈值才压缩，且重试可复用同一份压缩字节。
func TestCompressCodexResponsesRequestBySizeCompressesLargeBody(t *testing.T) {
	const size = 9 * 1024 * 1024
	req := newCodexCompressionRequest(t, size)

	applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionBySize, minBytes: codexCompressionTestThreshold})
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

// doUpstreamRequest 的默认策略取配置；服务与配置缺失时回落 by_size + 默认阈值。
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

// readCodexCompressedBody 解压请求的线上 body，用于验证压缩字节可还原。
func readCodexCompressedBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	replay, err := req.GetBody()
	require.NoError(t, err)
	defer func() { _ = replay.Close() }()
	decoder, err := zstd.NewReader(replay)
	require.NoError(t, err)
	defer decoder.Close()
	data, err := io.ReadAll(decoder)
	require.NoError(t, err)
	return data
}

// 流式编码路径必须产出可正确解压的字节；连续多次调用验证池化编码器状态
// （Reset/Write/Close）在请求之间不会串味。
func TestCompressCodexResponsesRequestStreamingRoundTrip(t *testing.T) {
	const size = 9 * 1024 * 1024
	for attempt := 0; attempt < 3; attempt++ {
		req := newCodexCompressionRequest(t, size)
		applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionOn, minBytes: 0})
		require.True(t, applied, "attempt %d", attempt)
		require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
		require.Less(t, req.ContentLength, int64(size), "attempt %d", attempt)
		require.Len(t, readCodexCompressedBody(t, req), size, "attempt %d", attempt)
	}
}

// 长度未知（ContentLength <= 0）走 EncodeAll 兜底路径，同样必须可还原。
func TestCompressCodexResponsesRequestUnknownLengthRoundTrip(t *testing.T) {
	const size = 2 * 1024 * 1024
	req := newCodexCompressionRequest(t, size)
	req.ContentLength = -1
	applied := compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionBySize, minBytes: 1 << 10})
	require.True(t, applied)
	require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
	require.Len(t, readCodexCompressedBody(t, req), size)
}

// 并发压缩：共享单例只走 EncodeAll，流式编码器走池，两者都不得互相覆盖状态。
func TestCompressCodexResponsesRequestConcurrent(t *testing.T) {
	const size = 1 << 20
	const workers = 8
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func(index int) {
			defer func() { done <- struct{}{} }()
			payload := bytes.Repeat([]byte{byte('a' + index)}, size)
			req, err := http.NewRequest(http.MethodPost, codexCompressionTargetURL, bytes.NewReader(payload))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if !compressCodexResponsesRequest(req, codexRequestCompression{mode: config.CodexRequestCompressionOn, minBytes: 0}) {
				t.Errorf("worker %d: compression not applied", index)
				return
			}
			data := readCodexCompressedBody(t, req)
			if len(data) != size {
				t.Errorf("worker %d: decoded %d bytes, want %d", index, len(data), size)
				return
			}
			if data[0] != byte('a'+index) {
				t.Errorf("worker %d: content mismatch across pooled encoder", index)
			}
		}(i)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}

// 池化 zstd 编码器归还前应 Reset(io.Discard) 解除对输出缓冲的引用；
// 归还后仍必须能正确编码，不能因为解除引用而破坏状态。
func TestCodexZstdPooledEncoderSurvivesReuse(t *testing.T) {
	first := newCodexCompressionRequest(t, 2<<20)
	require.True(t, compressCodexResponsesRequest(first, codexRequestCompression{
		mode:     config.CodexRequestCompressionOn,
		minBytes: 0,
	}))
	require.Equal(t, "zstd", first.Header.Get("Content-Encoding"))
	require.Len(t, readCodexCompressedBody(t, first), 2<<20)

	second := newCodexCompressionRequest(t, 1<<20)
	require.True(t, compressCodexResponsesRequest(second, codexRequestCompression{
		mode:     config.CodexRequestCompressionOn,
		minBytes: 0,
	}))
	require.Len(t, readCodexCompressedBody(t, second), 1<<20)
}
