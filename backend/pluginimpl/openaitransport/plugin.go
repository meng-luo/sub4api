package openaitransport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	pluginv1 "github.com/MACOS-DO/sub4api/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

const (
	// PluginID 是本实现的插件标识。宿主按 manifest 中的 id 建立绑定。
	PluginID = "local.sub4api.openai-transport"
	// PluginVersion 是插件版本。
	PluginVersion = "0.1.0"
)

// Plugin 实现 TransportPlugin 服务。
type Plugin struct {
	pluginv1.UnimplementedTransportPluginServer

	mu         sync.RWMutex
	cfg        Config
	applied    bool
	transports map[string]*http.Transport
}

// New 构造插件实例。
func New() *Plugin {
	return &Plugin{
		cfg:        DefaultConfig(),
		transports: make(map[string]*http.Transport),
	}
}

// GetInfo 返回身份与协议版本，必须与 manifest 一致。
func (p *Plugin) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{
		PluginId:            PluginID,
		PluginVersion:       PluginVersion,
		ProtocolVersion:     pluginv1.ProtocolVersion,
		TransportApiVersion: pluginv1.TransportAPIVersion,
		Capabilities:        []string{"openai.oauth.outbound_transport.v1"},
	}, nil
}

// Health 只回答「能否接单」，不做网络探测。
func (p *Plugin) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	p.mu.RLock()
	applied := p.applied
	cfg := p.cfg
	p.mu.RUnlock()
	if !applied {
		return &pluginv1.HealthResponse{Healthy: true, Message: "尚未应用配置，使用默认值"}, nil
	}
	return &pluginv1.HealthResponse{
		Healthy:    true,
		Message:    "ok",
		StatusJson: fmt.Sprintf(`{"tls_profile":%q,"http2":%t}`, cfg.TLSProfile, cfg.HTTP2),
	}, nil
}

// ValidateConfig 严格解析并回传规范化配置。
func (p *Plugin) ValidateConfig(_ context.Context, req *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	cfg, err := parseConfig(req.GetConfigJson())
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: err.Error()}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, Message: "ok", NormalizedConfigJson: cfg.marshal()}, nil
}

// ApplyConfig 原子切换配置，并关闭旧连接池里的空闲连接。
func (p *Plugin) ApplyConfig(_ context.Context, req *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	cfg, err := parseConfig(req.GetConfigJson())
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: err.Error()}, nil
	}
	p.mu.Lock()
	p.closeTransportsLocked()
	p.cfg = cfg
	p.applied = true
	p.mu.Unlock()
	return &pluginv1.ApplyConfigResponse{Applied: true, Message: "ok"}, nil
}

// TestConfig 用当前配置做一次轻量握手（不携带凭据、不访问业务路径）。
func (p *Plugin) TestConfig(ctx context.Context, req *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	cfg, err := parseConfig(req.GetConfigJson())
	if err != nil {
		return &pluginv1.TestConfigResponse{Success: false, Message: err.Error()}, nil
	}
	start := time.Now()
	tr, _, err := newTransport(cfg, "")
	if err != nil {
		return &pluginv1.TestConfigResponse{Success: false, Message: err.Error()}, nil
	}
	tr.CloseIdleConnections()
	return &pluginv1.TestConfigResponse{
		Success:   true,
		Message:   fmt.Sprintf("配置可用（tls_profile=%s http2=%t）", cfg.TLSProfile, cfg.HTTP2),
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// InitHostServices 本实现不需要宿主反向服务。
func (p *Plugin) InitHostServices(context.Context, *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	return &pluginv1.InitHostServicesResponse{Ready: false, Message: "本插件不依赖宿主服务"}, nil
}

// transportFor 返回（可能是新建的）传输层。
//
// 池键是 (account_id, proxy_url) 而不是只按 proxy 聚合：一个 OAuth 账号在真机上
// 对应一台设备、一条到 chatgpt.com 的长连接；若让多个账号共用同一条 TCP 连接，
// 上游会在同一条连接上看到多个 chatgpt-account-id，这是典型的多号共池特征。
// 代价是 FD 数量随账号数线性增长，需要按部署规模调 ulimit 与 IdleConnTimeout。
func (p *Plugin) transportFor(accountID int64, proxyURL string) (*http.Transport, error) {
	key := fmt.Sprintf("%d|%s", accountID, proxyURL)
	p.mu.Lock()
	defer p.mu.Unlock()
	if tr, ok := p.transports[key]; ok {
		return tr, nil
	}
	tr, _, err := newTransport(p.cfg, proxyURL)
	if err != nil {
		return nil, err
	}
	p.transports[key] = tr
	return tr, nil
}

func (p *Plugin) currentConfig() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *Plugin) closeTransportsLocked() {
	for key, tr := range p.transports {
		tr.CloseIdleConnections()
		delete(p.transports, key)
	}
}

var (
	errNoStartFrame  = errors.New("首个帧必须是 start")
	errBodyBeforeEnd = errors.New("请求体帧序非法")
)

// 确保接口实现完整。
var _ pluginv1.TransportPluginServer = (*Plugin)(nil)
var _ = grpc.ServerStream(nil)
