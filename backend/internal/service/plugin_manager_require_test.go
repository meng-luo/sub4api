package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRequirePluginManager(require bool) *PluginManager {
	return &PluginManager{cfg: &config.Config{Plugins: config.PluginConfig{RequireOpenAIOAuthPlugin: require}}}
}

// plugins.require_openai_oauth_plugin 只在「必须经插件 + 未命中绑定」时生效，
// 其余组合一律保持既有回落行为。
func TestPluginManagerMissingOpenAIOAuthPluginBinding(t *testing.T) {
	oauth := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "acc"}}
	apiKey := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	t.Run("flag off keeps legacy fallback", func(t *testing.T) {
		assert.False(t, newRequirePluginManager(false).MissingOpenAIOAuthPluginBinding(oauth))
	})

	t.Run("flag on without binding rejects", func(t *testing.T) {
		assert.True(t, newRequirePluginManager(true).MissingOpenAIOAuthPluginBinding(oauth))
	})

	t.Run("flag on with binding passes", func(t *testing.T) {
		manager := newRequirePluginManager(true)
		manager.route.Store(&pluginRoute{pluginID: 7, rolloutPercent: 100})
		assert.False(t, manager.MissingOpenAIOAuthPluginBinding(oauth))
	})

	t.Run("api key accounts are out of scope", func(t *testing.T) {
		assert.False(t, newRequirePluginManager(true).MissingOpenAIOAuthPluginBinding(apiKey))
	})

	t.Run("nil account and nil manager", func(t *testing.T) {
		assert.False(t, newRequirePluginManager(true).MissingOpenAIOAuthPluginBinding(nil))
		var manager *PluginManager
		assert.False(t, manager.MissingOpenAIOAuthPluginBinding(oauth))
	})
}

func TestPluginManagerRoundTripRejectsUnboundOAuthWhenRequired(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/v1/responses", nil)
	require.NoError(t, err)
	account := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "acc"}}

	response, handled, routeErr := newRequirePluginManager(true).RoundTripOpenAIOAuth(context.Background(), request, "", account)
	assert.Nil(t, response)
	assert.True(t, handled, "拒绝回落必须标记为已处理，避免调用方再走内置传输层")
	require.Error(t, routeErr)
	assert.Contains(t, routeErr.Error(), errOpenAIOAuthPluginNotBound)

	response, handled, routeErr = newRequirePluginManager(false).RoundTripOpenAIOAuth(context.Background(), request, "", account)
	assert.Nil(t, response)
	assert.False(t, handled, "开关关闭时保持旧的静默回落")
	assert.NoError(t, routeErr)
}

// WebSocket 入口必须与 HTTP 面同样 fail-closed：未绑定的 OAuth 账号不允许
// 通过内置传输层建立上游 WS 会话。
func TestRejectUnboundOpenAIOAuthWSAccount(t *testing.T) {
	oauth := &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "acc"}}

	svc := &OpenAIGatewayService{pluginManager: newRequirePluginManager(true)}
	require.Error(t, svc.rejectUnboundOpenAIOAuthWSAccount(oauth))
	assert.Contains(t, svc.rejectUnboundOpenAIOAuthWSAccount(oauth).Error(), errOpenAIOAuthPluginNotBound)

	// 开关关闭：保持旧行为，不拒绝。
	svc.pluginManager = newRequirePluginManager(false)
	assert.NoError(t, svc.rejectUnboundOpenAIOAuthWSAccount(oauth))

	// 绑定存在：交给插件路径。
	manager := newRequirePluginManager(true)
	manager.route.Store(&pluginRoute{pluginID: 7, rolloutPercent: 100})
	svc.pluginManager = manager
	assert.NoError(t, svc.rejectUnboundOpenAIOAuthWSAccount(oauth))

	// 非 OAuth / nil 账号 / 无插件管理器：不拒绝。
	svc.pluginManager = newRequirePluginManager(true)
	assert.NoError(t, svc.rejectUnboundOpenAIOAuthWSAccount(&Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}))
	assert.NoError(t, svc.rejectUnboundOpenAIOAuthWSAccount(nil))
	assert.NoError(t, (&OpenAIGatewayService{}).rejectUnboundOpenAIOAuthWSAccount(oauth))
}
