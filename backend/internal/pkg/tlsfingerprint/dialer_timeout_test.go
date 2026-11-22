//go:build unit

package tlsfingerprint

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDialTLSContextTimesOutWhenPeerStalls(t *testing.T) {
	for _, mode := range []string{"direct", "http", "socks5h"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })

			// The peer accepts TCP but never answers ClientHello or the proxy
			// negotiation. The outer timeout only keeps a regression from hanging CI.
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					defer func() { _ = conn.Close() }()
					<-ctx.Done()
				}
			}()

			addr := listener.Addr().String()
			dialTLS := NewDialer(nil, nil).DialTLSContext
			switch mode {
			case "http":
				dialTLS = NewHTTPProxyDialer(nil, &url.URL{Scheme: mode, Host: addr}).DialTLSContext
			case "socks5h":
				dialTLS = NewSOCKS5ProxyDialer(nil, &url.URL{Scheme: mode, Host: addr}).DialTLSContext
			}
			conn, err := dialTLS(ctx, "tcp", addr)
			if conn != nil {
				_ = conn.Close()
			}
			var timeout net.Error
			require.ErrorAs(t, err, &timeout)
			require.True(t, timeout.Timeout(), "expected a timeout, got %v", err)
			require.NoError(t, ctx.Err(), "the dialer must enforce its own timeout")
		})
	}
}
