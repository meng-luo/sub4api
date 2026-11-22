// Package openaitransport 是 Sub4API 自研的 OpenAI OAuth 出站传输插件实现。
//
// 它替代原先由闭源厂商包（ID: local.sub2api.openai-transport）承担的职责：
// 以与官方 Codex 客户端一致的 TLS/HTTP2 行为，把宿主已组装好的 HTTP 请求发往
// 上游，并把原始响应流式回传。协议契约见 backend/pkg/pluginapi/v1/plugin.proto。
package openaitransport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Config 是插件配置。宿主通过 ValidateConfig/ApplyConfig 下发 JSON。
//
// 设计原则：所有会影响出站字节形状的开关都必须显式，不能依赖库默认值。
type Config struct {
	// TLSProfile 决定 ClientHello 形态：
	//   rustls —— 模拟 Codex CLI（Rust/rustls）的握手特征，默认值
	//   go     —— Go 原生栈，仅用于对照排查
	TLSProfile string `json:"tls_profile"`
	// HTTP2 为 true 时协商 h2 并按 HTTP/2 发送；为 false 时强制 HTTP/1.1
	HTTP2 bool `json:"http2"`
	// DisableCompression 保持请求不压缩。官方 0.171.0 实测不发送
	// Content-Encoding，保持 true 是正确形态。
	DisableCompression bool `json:"disable_compression"`
	// BodyCompression 允许对请求体做 zstd 压缩（默认关闭）
	BodyCompression bool `json:"body_compression"`
	// InsecureSkipVerify 仅用于自建测试服务器
	InsecureSkipVerify bool `json:"insecure_skip_verify"`
	// HandshakeTimeout / ResponseHeaderTimeout / IdleConnTimeout
	HandshakeTimeout    time.Duration `json:"-"`
	HandshakeTimeoutSec int           `json:"handshake_timeout_seconds"`
	ResponseHeaderSec   int           `json:"response_header_timeout_seconds"`
	IdleConnTimeoutSec  int           `json:"idle_conn_timeout_seconds"`
	MaxConnsPerHost     int           `json:"max_conns_per_host"`
	MaxIdleConnsPerHost int           `json:"max_idle_conns_per_host"`
	// H2StreamWindowBytes / H2ConnWindowBytes 对应 SETTINGS 的流级/连接级
	// INITIAL_WINDOW_SIZE。0 沿用 Go 默认；调小可降低高并发内存，但会改变
	// SETTINGS 数值，改动前必须先与真机抓包对齐。
	H2StreamWindowBytes int `json:"h2_stream_window_bytes"`
	H2ConnWindowBytes   int `json:"h2_conn_window_bytes"`
	// DebugHeaders 为 true 时把出站头名写日志（不写值）。默认 false。
	DebugHeaders bool `json:"debug_headers"`
}

// DefaultConfig 返回与官方客户端一致的保守默认值。
func DefaultConfig() Config {
	return Config{
		TLSProfile:          "rustls",
		HTTP2:               true,
		DisableCompression:  true,
		BodyCompression:     false,
		InsecureSkipVerify:  false,
		HandshakeTimeoutSec: 15,
		ResponseHeaderSec:   600,
		IdleConnTimeoutSec:  90,
		MaxConnsPerHost:     0,
		MaxIdleConnsPerHost: 16,
	}
}

// normalize 把 0 值换算成默认值，并校验取值范围。
func (c *Config) normalize() error {
	if strings.TrimSpace(c.TLSProfile) == "" {
		c.TLSProfile = "rustls"
	}
	switch c.TLSProfile {
	case "rustls", "go":
	default:
		return fmt.Errorf("tls_profile 必须是 rustls 或 go，收到 %q", c.TLSProfile)
	}
	if c.HandshakeTimeoutSec <= 0 || c.HandshakeTimeoutSec > 300 {
		c.HandshakeTimeoutSec = 15
	}
	if c.ResponseHeaderSec < 0 || c.ResponseHeaderSec > 3600 {
		return fmt.Errorf("response_header_timeout_seconds 必须在 0..3600")
	}
	if c.IdleConnTimeoutSec < 0 || c.IdleConnTimeoutSec > 3600 {
		return fmt.Errorf("idle_conn_timeout_seconds 必须在 0..3600")
	}
	if c.MaxConnsPerHost < 0 || c.MaxConnsPerHost > 4096 {
		return fmt.Errorf("max_conns_per_host 必须在 0..4096")
	}
	if c.MaxIdleConnsPerHost <= 0 {
		c.MaxIdleConnsPerHost = 16
	}
	if c.H2StreamWindowBytes < 0 || c.H2StreamWindowBytes >= 4<<20 {
		return fmt.Errorf("h2_stream_window_bytes 必须在 0..4MiB 之间（0 表示默认）")
	}
	if c.H2ConnWindowBytes < 0 || c.H2ConnWindowBytes >= 4<<20 {
		return fmt.Errorf("h2_conn_window_bytes 必须在 0..4MiB 之间（0 表示默认）")
	}
	return nil
}

func (c Config) handshakeTimeout() time.Duration {
	return time.Duration(c.HandshakeTimeoutSec) * time.Second
}

func (c Config) responseHeaderTimeout() time.Duration {
	if c.ResponseHeaderSec <= 0 {
		return 0
	}
	return time.Duration(c.ResponseHeaderSec) * time.Second
}

func (c Config) idleConnTimeout() time.Duration {
	if c.IdleConnTimeoutSec <= 0 {
		return 90 * time.Second
	}
	return time.Duration(c.IdleConnTimeoutSec) * time.Second
}

// parseConfig 严格解析：拒绝未知字段，与宿主对 ValidateConfig 的约定一致。
func parseConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置失败: %w", err)
	}
	if err := cfg.normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// marshal 输出规范化配置。
func (c Config) marshal() []byte {
	type alias Config
	out, err := json.Marshal(alias(c))
	if err != nil {
		return []byte("{}")
	}
	return out
}
