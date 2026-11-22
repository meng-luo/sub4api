package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type codexTicketFuncUpstream struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (u *codexTicketFuncUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.do(req)
}
func codexTicketResponse() *http.Response {
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}
}

func ticketProbeChallenge(t *testing.T) ModelTraceChallenge {
	t.Helper()
	challenge, err := newModelTraceChallenge(bytes.NewReader([]byte{40, 0, 0, 0, 0}))
	require.NoError(t, err)
	return challenge
}

func TestCodexTicketProbeUsesGatewayPluginPolicy(t *testing.T) {
	manager := &PluginManager{}
	var calls int
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, HTTPUpstreamProfileOpenAIHarvest, HTTPUpstreamProfileFromContext(req.Context()))
		require.True(t, req.Close)
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, upstream)
	svc.SetPluginManager(manager)
	account := ticketTestAccount(41)
	probe := func() (codexTicketProbeResult, error) {
		return svc.fireOpenAICodexTicketProbe(context.Background(), account, "test-token", "gpt-6-astra", "http://proxy.example.com:8080", ticketProbeChallenge(t), time.Second)
	}
	result, err := probe()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.status)
	require.Len(t, result.state, 292)

	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "test plugin unavailable"})
	_, err = probe()
	require.ErrorContains(t, err, "test plugin unavailable")
	manager.route.Store(nil)
	manager.cfg = &config.Config{Plugins: config.PluginConfig{RequireOpenAIOAuthPlugin: true}}
	_, err = probe()
	require.ErrorContains(t, err, errOpenAIOAuthPluginNotBound)
	require.Equal(t, 1, calls, "plugin failures must not fall back to the built-in transport")
}

func TestCodexTicketProbeUsesGatewayVersionAndCanHarvestWithoutTicket(t *testing.T) {
	const model = "gpt-6.2-astra"
	const configuredVersion = "0.171.0"
	oldEnforcement, oldEmit := codexIdentityEnforcement.Load(), codexEmitVersionHeader.Load()
	codexCanonicalUAMu.RLock()
	oldResolver := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	SetCodexIdentityEnforcementEnabled(true)
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent(configuredVersion) })
	t.Cleanup(func() {
		SetCodexIdentityEnforcementEnabled(oldEnforcement)
		SetCodexEmitVersionHeader(oldEmit)
		SetCodexCanonicalUserAgentResolver(oldResolver)
	})
	account := ticketTestAccount(41)
	for _, emitVersion := range []bool{false, true} {
		SetCodexEmitVersionHeader(emitVersion)
		upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
			require.Equal(t, chatgptCodexURL, req.URL.String())
			require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))
			require.False(t, req.Close)
			require.Equal(t, buildCodexCLIUserAgent(configuredVersion), req.UserAgent())
			if emitVersion {
				require.Equal(t, configuredVersion, req.Header.Get("version"))
			} else {
				require.Empty(t, req.Header.Get("version"))
			}
			require.Empty(t, req.Header.Get(openAICodexTurnStateHeader))
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, model, gjson.GetBytes(body, "model").String())
			return codexTicketResponse(), nil
		}}
		svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, upstream)
		require.ErrorIs(t, svc.applyOpenAICodexTicket(context.Background(), account, model, make(http.Header)), ErrOpenAICodexTicketUnavailable)
		result, err := svc.fireOpenAICodexTicketProbe(context.Background(), account, "test-token", model, "", ticketProbeChallenge(t), time.Second)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, result.status)
	}
}

func TestCodexTicketProbeParsesNormalSSEAndCookies(t *testing.T) {
	account := ticketTestAccount(41)
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		header := http.Header{}
		header.Set(openAICodexTurnStateHeader, "new-state")
		header.Add("Set-Cookie", "__cfruid=first; HttpOnly")
		header.Add("Set-Cookie", "cf_clearance=second; Secure")
		// 真实 Codex 客户端只持久化 Cloudflare 基础设施 Cookie
		// （http-client/src/chatgpt_cloudflare_cookies.rs），账号/会话
		// Cookie 回放是真实客户端不会产生的特征，必须整对丢弃。
		header.Add("Set-Cookie", "session=secret; HttpOnly")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"1 2 3\"}\n\ndata: [DONE]\n\n"))}, nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, upstream)
	probe, err := svc.fireOpenAICodexTicketProbe(context.Background(), account, "token", "gpt-6-astra", "http://proxy.example.com:8080", ticketProbeChallenge(t), time.Second)
	require.NoError(t, err)
	require.Equal(t, "1 2 3", probe.output)
	require.Equal(t, "new-state", probe.state)
	require.Contains(t, probe.cookie, "__cfruid=first")
	require.Contains(t, probe.cookie, "cf_clearance=second")
	require.NotContains(t, probe.cookie, "session")
	require.Equal(t, http.StatusOK, probe.status)
}

func TestCodexTicketProbeDoesNotCarryExistingTicket(t *testing.T) {
	account := ticketTestAccount(41)
	const model = "gpt-6-astra"
	ticket := verifiedTicket(account, model, fakeCodexTicketState(292), "__oailb=old")
	account.Extra = map[string]any{openAICodexTicketExtraKey(model): ticket}
	upstream := &codexTicketFuncUpstream{do: func(request *http.Request) (*http.Response, error) {
		require.Empty(t, request.Header.Get(openAICodexTurnStateHeader))
		require.Empty(t, request.Header.Get("Cookie"))
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, upstream)
	h := make(http.Header)
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, model, h))
	require.Equal(t, ticket.State, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, "__oailb=old", h.Get("Cookie"))
	probe, err := svc.fireOpenAICodexTicketProbe(context.Background(), account, "token", model, "http://proxy.example.com:8080", ticketProbeChallenge(t), time.Second)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, probe.status)
}

func TestCodexTicketPolicyExemptsCredentialShadows(t *testing.T) {
	parentID := int64(41)
	parent := ticketTestAccount(parentID)
	shadow := ticketTestAccount(42)
	shadow.ParentAccountID = &parentID
	shadow.Status = StatusActive
	cfg := config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true, HarvestProxyURL: "http://proxy.example.com:8080"}
	upstream := &httpUpstreamRecorder{}
	svc := ticketTestService(t, cfg, upstream)
	svc.accountRepo = &codexTicketRefreshRepo{accounts: []Account{*shadow}}
	require.True(t, svc.openAICodexTicketBlocksAccount(parent, "gpt-6-astra"))
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		shadow.Type = accountType
		require.False(t, svc.openAICodexTicketBlocksAccount(shadow, "gpt-6-astra"))
		headers := http.Header{}
		headers.Set(openAICodexTurnStateHeader, "client-state")
		require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), shadow, "gpt-6-astra", headers))
		require.Equal(t, "client-state", headers.Get(openAICodexTurnStateHeader))
		require.Empty(t, OpenAICodexTicketStatuses(shadow, cfg, time.Now()))
		svc.probeOnceOpenAICodexTicket(context.Background(), shadow, "gpt-6-astra")
	}
	svc.refreshOpenAICodexTickets(context.Background())
	require.Empty(t, upstream.requests)
}

type codexTicketRefreshRepo struct {
	AccountRepository
	accounts []Account
}

func (repo *codexTicketRefreshRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	return repo.accounts, nil
}
