package openaitransport

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"
)

// dialer 负责「按配置建立到目标主机的 TLS 连接」，包括代理隧道。
type dialer struct {
	cfg   Config
	base  *net.Dialer
	proxy *url.URL
	alpns []string
}

func newDialer(cfg Config, proxyURL string) (*dialer, error) {
	d := &dialer{
		cfg:   cfg,
		base:  &net.Dialer{Timeout: cfg.handshakeTimeout(), KeepAlive: 30 * time.Second},
		alpns: alpnFor(cfg.HTTP2),
	}
	raw := strings.TrimSpace(proxyURL)
	if raw == "" {
		return d, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析代理地址失败: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
	}
	d.proxy = u
	return d, nil
}

// dialTLSContext 供 http.Transport 回调使用。
func (d *dialer) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	rawConn, err := d.dialRaw(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host := hostOnly(addr)
	conn, err := d.handshake(ctx, rawConn, host)
	if err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	return conn, nil
}

// dialRaw 建立到目标的 TCP 连接（必要时先打通代理隧道）。
func (d *dialer) dialRaw(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.proxy == nil {
		return d.base.DialContext(ctx, network, addr)
	}
	switch strings.ToLower(d.proxy.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if d.proxy.User != nil {
			pass, _ := d.proxy.User.Password()
			auth = &proxy.Auth{User: d.proxy.User.Username(), Password: pass}
		}
		sd, err := proxy.SOCKS5("tcp", d.proxy.Host, auth, d.base)
		if err != nil {
			return nil, fmt.Errorf("建立 SOCKS5 拨号器失败: %w", err)
		}
		if cd, ok := sd.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return sd.Dial(network, addr)
	case "http", "https":
		return d.dialHTTPProxy(ctx, addr)
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", d.proxy.Scheme)
	}
}

// dialHTTPProxy 通过 HTTP(S) 代理的 CONNECT 建立隧道。
func (d *dialer) dialHTTPProxy(ctx context.Context, target string) (net.Conn, error) {
	conn, err := d.base.DialContext(ctx, "tcp", d.proxy.Host)
	if err != nil {
		return nil, fmt.Errorf("连接代理失败: %w", err)
	}
	if strings.EqualFold(d.proxy.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: hostOnly(d.proxy.Host), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("与 HTTPS 代理握手失败: %w", err)
		}
		conn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	if d.proxy.User != nil {
		pass, _ := d.proxy.User.Password()
		req.SetBasicAuth(d.proxy.User.Username(), pass)
	}
	req.Header.Set("Proxy-Connection", "Keep-Alive")
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送 CONNECT 失败: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取 CONNECT 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 被拒绝: %s", resp.Status)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// handshake 按配置选择 rustls 形态（utls）或 Go 原生栈。
func (d *dialer) handshake(ctx context.Context, raw net.Conn, host string) (net.Conn, error) {
	if d.cfg.TLSProfile == "go" {
		conn := tls.Client(raw, &tls.Config{
			ServerName:         host,
			MinVersion:         tls.VersionTLS12,
			NextProtos:         d.alpns,
			InsecureSkipVerify: d.cfg.InsecureSkipVerify,
		})
		if err := conn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS 握手失败: %w", err)
		}
		return conn, nil
	}

	uconn := utls.UClient(raw, &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: d.cfg.InsecureSkipVerify,
		NextProtos:         d.alpns,
	}, utls.HelloCustom)
	if err := uconn.ApplyPreset(rustlsClientHelloSpec(d.alpns)); err != nil {
		return nil, fmt.Errorf("应用 ClientHello 模板失败: %w", err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("utls 握手失败: %w", err)
	}
	return &connStateConn{UConn: uconn}, nil
}

// connStateConn 让 utls 连接对外暴露 ConnectionState，
// http.Transport 依赖它判断是否切到 HTTP/2。
type connStateConn struct {
	*utls.UConn
}

func (c *connStateConn) ConnectionState() tls.ConnectionState {
	state := c.UConn.ConnectionState()
	return tls.ConnectionState{
		Version:            state.Version,
		HandshakeComplete:  state.HandshakeComplete,
		ServerName:         state.ServerName,
		NegotiatedProtocol: state.NegotiatedProtocol,
		PeerCertificates:   state.PeerCertificates,
	}
}

// bufferedConn 保留 CONNECT 响应后残留的缓冲字节。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// newTransport 依据配置构建传输层：HTTP/2 与 HTTP/1.1 共用一条 http.Transport，
// 通过 Protocols 显式选择协议，避免依赖库默认行为。
func newTransport(cfg Config, proxyURL string) (*http.Transport, *dialer, error) {
	d, err := newDialer(cfg, proxyURL)
	if err != nil {
		return nil, nil, err
	}
	protocols := new(http.Protocols)
	if cfg.HTTP2 {
		protocols.SetHTTP2(true)
		protocols.SetHTTP1(true)
	} else {
		protocols.SetHTTP1(true)
	}
	t := &http.Transport{
		DialTLSContext:        d.dialTLSContext,
		Protocols:             protocols,
		DisableCompression:    cfg.DisableCompression,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.idleConnTimeout(),
		ResponseHeaderTimeout: cfg.responseHeaderTimeout(),
		TLSHandshakeTimeout:   cfg.handshakeTimeout(),
		// 与官方 hyper 客户端对齐：不主动发健康 PING（SendPingTimeout=0）。
		HTTP2: http2ConfigFor(cfg),
	}
	return t, d, nil
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// http2ConfigFor 把插件配置映射成 Go 的 HTTP/2 旋钮。
//
// 默认只对齐两项「行为级」特征：不发健康 PING、MAX_FRAME_SIZE=16384。
// 流窗口等参数会直接体现在 SETTINGS 里，未与真机对齐前保持 Go 默认，
// 需要省内存时再通过 h2_stream_window_bytes 显式调小。
func http2ConfigFor(cfg Config) *http.HTTP2Config {
	h2 := &http.HTTP2Config{
		SendPingTimeout:  0,
		MaxReadFrameSize: 16384,
	}
	if cfg.H2StreamWindowBytes > 0 {
		h2.MaxReceiveBufferPerStream = cfg.H2StreamWindowBytes
	}
	if cfg.H2ConnWindowBytes > 0 {
		h2.MaxReceiveBufferPerConnection = cfg.H2ConnWindowBytes
	}
	return h2
}
