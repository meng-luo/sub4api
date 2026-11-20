package openaitransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func benchServer(b *testing.B, respBytes int) *httptest.Server {
	b.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte("x"), respBytes))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	b.Cleanup(srv.Close)
	return srv
}

func benchmarkRoundTrip(b *testing.B, reqBytes int) {
	srv := benchServer(b, 256)
	cfg := DefaultConfig()
	cfg.InsecureSkipVerify = true
	tr, _, err := newTransport(cfg, "")
	if err != nil {
		b.Fatalf("newTransport: %v", err)
	}
	defer tr.CloseIdleConnections()
	payload := bytes.Repeat([]byte("A"), reqBytes)
	client := &http.Client{Transport: tr}
	b.SetBytes(int64(reqBytes))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/backend-api/codex/responses", bytes.NewReader(payload))
			req.Header.Set("content-type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				b.Errorf("do: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}

func BenchmarkRoundTrip1K(b *testing.B)   { benchmarkRoundTrip(b, 1<<10) }
func BenchmarkRoundTrip100K(b *testing.B) { benchmarkRoundTrip(b, 100<<10) }
func BenchmarkRoundTrip1M(b *testing.B)   { benchmarkRoundTrip(b, 1<<20) }

// BenchmarkHandshake 对比 utls(rustls 形态) 与 Go 原生 TLS 的握手成本。
func BenchmarkHandshake(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	run := func(b *testing.B, profile string) {
		b.Helper()
		cfg := DefaultConfig()
		cfg.TLSProfile = profile
		cfg.InsecureSkipVerify = true
		d, err := newDialer(cfg, "")
		if err != nil {
			b.Fatalf("dialer: %v", err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn, err := d.dialTLSContext(ctx, "tcp", addr)
			cancel()
			if err != nil {
				b.Fatalf("handshake: %v", err)
			}
			_ = conn.Close()
		}
	}

	b.Run("utls-rustls", func(b *testing.B) { run(b, "rustls") })
	b.Run("go-tls", func(b *testing.B) { run(b, "go") })
}

var _ = utls.HelloCustom
var _ = tls.VersionTLS13
