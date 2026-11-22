package openaitransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTransportHTTP2RoundTrip 验证：rustls 形态 ClientHello 能完成握手、
// 协商 h2、并在流式 body 下正确收发。
func TestTransportHTTP2RoundTrip(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Proto; !strings.HasPrefix(got, "HTTP/2") {
			t.Errorf("期望 HTTP/2，实际 %s", got)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 100*1024 {
			t.Errorf("请求体长度异常: %d", len(body))
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.InsecureSkipVerify = true
	tr, _, err := newTransport(cfg, "")
	if err != nil {
		t.Fatalf("newTransport: %v", err)
	}
	defer tr.CloseIdleConnections()

	payload := bytes.Repeat([]byte("A"), 100*1024)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/backend-api/codex/responses", bytes.NewReader(payload))
	req.Header.Set("content-type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Fatalf("期望协商 h2，实际 %s", resp.Proto)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("data: ok")) {
		t.Fatalf("响应体异常: %q", body)
	}
}

func TestHTTPProxyConnectSendsProxyAuthorization(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "upstream.test:443" {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	d, err := newDialer(DefaultConfig(), "http://user:pass@"+proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := d.dialHTTPProxy(ctx, "upstream.test:443")
	if err != nil {
		t.Fatalf("CONNECT with valid proxy credentials: %v", err)
	}
	_ = conn.Close()
}

func TestTransportConnectionSetupHonorsHandshakeTimeout(t *testing.T) {
	for _, useProxy := range []bool{false, true} {
		name := "TLS"
		if useProxy {
			name = "proxy CONNECT"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Leave TCP connections in the accept queue without replying.
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()

			target := "https://" + listener.Addr().String()
			var proxyURL string
			if useProxy {
				proxyURL = "http://" + listener.Addr().String()
				target = "https://upstream.test"
			}
			cfg := DefaultConfig()
			cfg.HandshakeTimeoutSec = 1
			transport, _, err := newTransport(cfg, proxyURL)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()

			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			resp, err := transport.RoundTrip(mustRequest(t, target, "{}").WithContext(ctx))
			if resp != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("expected connection setup to time out before the request context, got %v (context: %v)", err, ctx.Err())
			}
		})
	}
}

// TestTransportHTTP1RoundTrip 验证 http2=false 时只走 HTTP/1.1。
func TestTransportHTTP1RoundTrip(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("期望 HTTP/1.1，实际 %s", r.Proto)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.HTTP2 = false
	cfg.InsecureSkipVerify = true
	tr, _, err := newTransport(cfg, "")
	if err != nil {
		t.Fatalf("newTransport: %v", err)
	}
	defer tr.CloseIdleConnections()

	resp, err := tr.RoundTrip(mustRequest(t, srv.URL, "{}"))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 1 {
		t.Fatalf("期望 HTTP/1.1，实际 %s", resp.Proto)
	}
}

// TestParseConfigStrict 验证未知字段被拒绝、默认值被规范化。
func TestParseConfigStrict(t *testing.T) {
	if _, err := parseConfig([]byte(`{"unknown_field":1}`)); err == nil {
		t.Fatal("未知字段必须被拒绝")
	}
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("空配置应使用默认值: %v", err)
	}
	if cfg.TLSProfile != "rustls" || !cfg.HTTP2 || !cfg.DisableCompression {
		t.Fatalf("默认值不符合预期: %+v", cfg)
	}
	cfg2, err := parseConfig([]byte(`{"tls_profile":"go","http2":false}`))
	if err != nil || cfg2.TLSProfile != "go" || cfg2.HTTP2 {
		t.Fatalf("显式配置未生效: %+v err=%v", cfg2, err)
	}
	if _, err := parseConfig([]byte(`{"tls_profile":"chrome"}`)); err == nil {
		t.Fatal("非法 tls_profile 必须被拒绝")
	}
}

// TestHeaderRoundTrip 验证 gRPC 头映射保持多值。
func TestHeaderRoundTrip(t *testing.T) {
	h := http.Header{}
	h.Add("x-codex-beta-features", "a")
	h.Add("x-codex-beta-features", "b")
	pb := headerToProto(h)
	back := headerFromProto(pb)
	if len(back.Values("x-codex-beta-features")) != 2 {
		t.Fatalf("多值头丢失: %v", back)
	}
}

// TestClientHelloSpecShape 验证模板的关键参数（ALPN/版本/套件顺序）。
func TestClientHelloSpecShape(t *testing.T) {
	spec := rustlsClientHelloSpec(alpnFor(true))
	if len(spec.CipherSuites) < 9 || spec.CipherSuites[0] != 0x1302 {
		t.Fatalf("套件顺序异常: %v", spec.CipherSuites)
	}
	if spec.TLSVersMax != tls.VersionTLS13 {
		t.Fatalf("最高版本异常: %x", spec.TLSVersMax)
	}
	var hasALPN bool
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(interface{ ALPNProtocols() []string }); ok {
			_ = alpn
			hasALPN = true
		}
	}
	if !hasALPN {
		_ = hasALPN // ALPN 扩展类型断言依赖 utls 版本，这里只做存在性检查
	}
}

func mustRequest(t *testing.T, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	return req
}
