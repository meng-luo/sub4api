package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexProbeBuildersHonorTemplateSnapshot(t *testing.T) {
	custom := strings.Replace(DefaultCodexProbeTemplate(), "anonymous workspace", "pinned workspace", 1)
	repo := &codexPolicyMigrationRepoStub{values: map[string]string{SettingKeyOpenAICodexTicketPromptTemplate: custom}}
	var harvestHeaders http.Header
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assertCodexProbeIdentity(t, body, req.Header)
		require.Contains(t, string(body), "pinned workspace")
		require.Equal(t, "harvest challenge", gjson.GetBytes(body, "input.4.content.0.text").String())
		harvestHeaders = req.Header.Clone()
		return codexTicketResponse(), nil
	}}
	svc, account, _ := challengeHarvestService(t, upstream)
	svc.settingService.settingRepo = repo
	pinned, err := svc.PrepareCodexProbeContext(context.Background())
	require.NoError(t, err)
	_, err = svc.fireOpenAICodexTicketProbe(pinned, account, "test-token", "gpt-6-astra", "", ModelTraceChallenge{Prompt: "harvest challenge"}, time.Second)
	require.NoError(t, err)
	repo.values[SettingKeyOpenAICodexTicketPromptTemplate] = strings.Replace(custom, "pinned workspace", "updated workspace", 1)
	svc.settingService.InvalidateCodexProbeTemplateCache()
	body, headers, err := svc.buildCodexProbeRequest(pinned, account, "gpt-6-astra", ModelTraceChallenge{Prompt: "diagnostic challenge"})
	require.NoError(t, err)
	assertCodexProbeIdentity(t, body, headers)
	require.Contains(t, string(body), "pinned workspace")
	require.Equal(t, "diagnostic challenge", gjson.GetBytes(body, "input.4.content.0.text").String())
	require.NotEqual(t, harvestHeaders.Get("session-id"), headers.Get("session-id"))
	require.Equal(t, harvestHeaders.Get("x-codex-installation-id"), headers.Get("x-codex-installation-id"))
	body, _, err = svc.buildCodexProbeRequest(context.Background(), account, "gpt-5.4", ModelTraceChallenge{Prompt: "next model challenge"})
	require.NoError(t, err)
	require.Contains(t, string(body), "updated workspace")
}

func TestCodexProbeGatewayAccountAndFingerprintRewritesStayConsistent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(41, map[string]any{codexFingerprintModeExtraKey: mode})
			account.Credentials = map[string]any{"chatgpt_account_id": "probe-test-account"}
			svc := &OpenAIGatewayService{}
			body, headers, err := svc.buildCodexProbeRequest(context.Background(), account, "gpt-5.4", ModelTraceChallenge{Prompt: "probe challenge"})
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Request.Header = headers.Clone()
			c.Set("api_key", &APIKey{ID: 77})
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(body, &decoded))
			// Exercise the same identity stages and order used by Forward.
			require.True(t, applyCodexAccountIdentityClientMetadataMap(decoded, account, 77, ""))
			ids := resolveCodexFingerprintIDsFromRequest(account, headers)
			applyCodexFingerprintClientMetadata(decoded, ids)
			stageCodexFingerprintIDs(c, ids)
			encoded, err := json.Marshal(decoded)
			require.NoError(t, err)
			request, err := svc.buildUpstreamRequest(context.Background(), c, account, encoded, "test-token", true, headers.Get("session-id"), true)
			require.NoError(t, err)
			cm := gjson.GetBytes(encoded, "client_metadata")
			// 官方 /responses 只把 session-id / thread-id / window-id 作为头发送，
			// 安装 ID 只在 body client_metadata 里（见 codex-rs responses.rs /
			// client.rs）。收口后这些头必须与 client_metadata 一致，且安装 ID 头缺席。
			for _, pair := range [][2]string{{"session-id", "session_id"}, {"thread-id", "thread_id"}, {"x-codex-window-id", "x-codex-window-id"}} {
				require.Equal(t, cm.Get(pair[1]).String(), request.Header.Get(pair[0]), pair[0])
			}
			require.NotEmpty(t, cm.Get("x-codex-installation-id").String(), "installation id must remain in client_metadata")
			require.Empty(t, request.Header.Get("x-codex-installation-id"), "official /responses never sends the installation-id header")
			require.Empty(t, request.Header.Get("session_id"), "official /responses never sends the underscored session header")
			require.Equal(t, cm.Get("session_id").String(), gjson.GetBytes(encoded, "prompt_cache_key").String())
			// 出站 metadata 已按官方 payload 形状补齐（21 键），这里比对身份字段而不是整串。
			headerMetadata := map[string]any{}
			require.NoError(t, json.Unmarshal([]byte(request.Header.Get("x-codex-turn-metadata")), &headerMetadata))
			bodyMetadata := map[string]any{}
			require.NoError(t, json.Unmarshal([]byte(cm.Get("x-codex-turn-metadata").String()), &bodyMetadata))
			for _, key := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id"} {
				require.Equal(t, bodyMetadata[key], headerMetadata[key], key)
			}
			require.NotEqual(t, headers.Get("session-id"), request.Header.Get("session-id"))
			require.Equal(t, "probe challenge", gjson.GetBytes(encoded, "input.4.content.0.text").String())
		})
	}
}
