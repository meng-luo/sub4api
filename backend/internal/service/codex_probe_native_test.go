package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// nativeProbeAccountRepo 只认已知账号：未知 ID 返回 (nil, nil)，模拟真实
// 仓储查不到行的行为，供 identity_resolution_failed 用例使用。
type nativeProbeAccountRepo struct {
	AccountRepository
	known *Account
}

func (r *nativeProbeAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.known == nil || r.known.ID != id {
		return nil, nil
	}
	return r.known, nil
}

// nativeProbeUnrelatedUserKey 是任何请求都不应携带的“用户侧 API Key”哨兵。
const nativeProbeUnrelatedUserKey = "sk-user-key-native-probe-must-not-use"

type nativeDiagnosticProxyUpstream struct {
	HTTPUpstream
	proxyURL string
}

func (u *nativeDiagnosticProxyUpstream) Do(_ *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	u.proxyURL = proxyURL
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"73 12 91 4\"}\n\ndata: [DONE]\n\n")),
	}, nil
}

func TestRunCodexNativeDiagnosticFollowsTicketProxySource(t *testing.T) {
	for _, tc := range []struct {
		name      string
		source    string
		withProxy bool
		emptyPool bool
		wantProxy string
	}{
		{name: "pool", source: CodexTicketProxySourcePool, withProxy: true, wantProxy: "http://proxy.example.com:8080"},
		{name: "account", source: CodexTicketProxySourceAccount, withProxy: true, wantProxy: "http://account.example.com:8080"},
		{name: "account direct", source: CodexTicketProxySourceAccount},
		{name: "empty pool direct fallback", source: CodexTicketProxySourcePool, emptyPool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &nativeDiagnosticProxyUpstream{}
			svc, account, _ := challengeHarvestService(t, upstream)
			repo := newRuntimeSettingRepoStub()
			repo.values[codexTicketProxySourceSettingKey] = tc.source
			if tc.emptyPool {
				repo.values[codexTicketPoolSettingKey] = `{"mode":"custom","proxy_ids":[99]}`
			}
			svc.settingService.settingRepo = repo
			if tc.withProxy {
				id := int64(7)
				account.ProxyID = &id
				account.Proxy = &Proxy{ID: id, Protocol: "http", Host: "account.example.com", Port: 8080, Status: StatusActive}
			}
			result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID, "gpt-6-astra",
				ModelTraceChallenge{Prompt: "challenge", ExpectedCount: 4}, 2*time.Second)
			require.Empty(t, result.Reason)
			require.Equal(t, "73 12 91 4", result.Output)
			require.Equal(t, tc.wantProxy, upstream.proxyURL)
		})
	}
}

func TestRunCodexNativeDiagnosticSuccessUsesAccountToken(t *testing.T) {
	const model = "gpt-6-astra"
	const streamed = "73 12 91 4"
	var calls int
	var method, host, authorization string
	var body []byte
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		calls++
		method = req.Method
		host = req.URL.Host
		authorization = req.Header.Get("Authorization")
		var err error
		body, err = io.ReadAll(req.Body)
		require.NoError(t, err)
		assertCodexProbeIdentity(t, body, req.Header)
		return challengeHarvestResponse(t, streamed), nil
	}}
	svc, account, _ := challengeHarvestService(t, upstream)
	challenge := ModelTraceChallenge{Prompt: "native diagnostic challenge", ExpectedCount: 4}

	result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID, model, challenge, 2*time.Second)

	require.Equal(t, 1, calls)
	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "chatgpt.com", host)
	require.Equal(t, "tok", account.GetOpenAIAccessToken(), "fixture account token")
	require.Equal(t, "Bearer "+account.GetOpenAIAccessToken(), authorization)
	require.NotEqual(t, "Bearer "+nativeProbeUnrelatedUserKey, authorization)
	require.Equal(t, model, gjson.GetBytes(body, "model").String())
	require.Contains(t, string(body), challenge.Prompt)
	require.Equal(t, "", result.Reason)
	require.Equal(t, http.StatusOK, result.HTTPStatus)
	require.Equal(t, streamed, result.Output)
}

func TestRunCodexNativeDiagnosticMissingAccountToken(t *testing.T) {
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	}}
	svc, account, _ := challengeHarvestService(t, upstream)
	account.Credentials = map[string]any{"chatgpt_account_id": "acc-1"}

	result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID, "gpt-6-astra",
		ModelTraceChallenge{Prompt: "challenge", ExpectedCount: 4}, 2*time.Second)

	require.Equal(t, "credentials", result.Reason)
	require.Equal(t, 0, calls, "credential failure must not reach upstream")
	require.Equal(t, 0, result.HTTPStatus)
}

func TestRunCodexNativeDiagnosticInvalidTemplateHidesSecret(t *testing.T) {
	const secret = "{private-secret"
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	}}
	svc, account, _ := challengeHarvestService(t, upstream)
	svc.settingService.settingRepo = &codexPolicyMigrationRepoStub{values: map[string]string{
		SettingKeyOpenAICodexTicketPromptTemplate: secret,
	}}

	result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID, "gpt-6-astra",
		ModelTraceChallenge{Prompt: "challenge", ExpectedCount: 4}, 2*time.Second)

	require.Equal(t, "template_invalid", result.Reason)
	require.Equal(t, 0, calls, "invalid template must not reach upstream")
	rendered := fmt.Sprintf("%+v", result)
	require.NotContains(t, rendered, "private-secret")
	require.NotContains(t, rendered, "{private-secret")
}

func TestRunCodexNativeDiagnosticHTTP503(t *testing.T) {
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("service unavailable")),
		}, nil
	}}
	svc, account, _ := challengeHarvestService(t, upstream)

	result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID, "gpt-6-astra",
		ModelTraceChallenge{Prompt: "challenge", ExpectedCount: 4}, 2*time.Second)

	require.Equal(t, 1, calls)
	require.Equal(t, "http_status", result.Reason)
	require.Equal(t, http.StatusServiceUnavailable, result.HTTPStatus)
	require.Equal(t, "", result.Output)
}

func TestRunCodexNativeDiagnosticEmptyResponseIncomplete(t *testing.T) {
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		// 200 且只有 SSE 心事件，没有任何 output_text。
		return codexTicketResponse(), nil
	}}
	svc, account, _ := challengeHarvestService(t, upstream)

	result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID, "gpt-6-astra",
		ModelTraceChallenge{Prompt: "challenge", ExpectedCount: 4}, 2*time.Second)

	require.Equal(t, 1, calls)
	require.Equal(t, "response_incomplete", result.Reason)
	require.Equal(t, http.StatusOK, result.HTTPStatus)
	require.Equal(t, "", result.Output)
}

func TestRunCodexNativeDiagnosticTimeout(t *testing.T) {
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return nil, context.DeadlineExceeded
	}}
	svc, account, _ := challengeHarvestService(t, upstream)

	result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID, "gpt-6-astra",
		ModelTraceChallenge{Prompt: "challenge", ExpectedCount: 4}, 2*time.Second)

	require.Equal(t, 1, calls)
	require.Equal(t, "timeout", result.Reason)
	require.Equal(t, 0, result.HTTPStatus)
}

func TestRunCodexNativeDiagnosticUnknownAccount(t *testing.T) {
	calls := 0
	upstream := &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	}}
	svc, account, _ := challengeHarvestService(t, upstream)
	svc.accountRepo = &nativeProbeAccountRepo{known: account}

	result := svc.RunCodexNativeDiagnostic(context.Background(), account.ID+1000, "gpt-6-astra",
		ModelTraceChallenge{Prompt: "challenge", ExpectedCount: 4}, 2*time.Second)

	require.Equal(t, "identity_resolution_failed", result.Reason)
	require.Equal(t, 0, calls, "unknown account must not reach upstream")
	require.Equal(t, 0, result.HTTPStatus)
}
