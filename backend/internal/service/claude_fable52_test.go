//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestFable52PricingAndFastMode(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		var catalog *PricingService
		if dynamic {
			catalog = &PricingService{pricingData: map[string]*LiteLLMModelPricing{
				"claude-fable-5-1": {InputCostPerToken: 10e-6, OutputCostPerToken: 50e-6},
			}}
		}
		svc := NewBillingService(&config.Config{}, catalog)
		for _, model := range []string{"claude-fable-5-2", "anthropic/claude-fable-5-2", "global.anthropic.claude-fable-5-2", "CLAUDE-FABLE-5-2", "claude-fable-5-2-20261109"} {
			p, err := svc.GetModelPricing(model)
			require.NoError(t, err)
			require.InDelta(t, 8e-6, p.InputPricePerToken, 1e-12)
			require.InDelta(t, 40e-6, p.OutputPricePerToken, 1e-12)
			require.InDelta(t, 10e-6, p.CacheCreationPricePerToken, 1e-12)
			require.InDelta(t, 10e-6, p.CacheCreation5mPrice, 1e-12)
			require.InDelta(t, 16e-6, p.CacheCreation1hPrice, 1e-12)
			require.InDelta(t, .2e-6, p.CacheReadPricePerToken, 1e-12)
			require.True(t, p.SupportsCacheBreakdown)
			require.Zero(t, p.LongContextInputThreshold)
			require.Empty(t, p.ReasoningEffortMultipliers)
			for _, tier := range []string{"", "fast"} {
				cost, err := svc.CalculateCostWithServiceTier(model, UsageTokens{
					InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000,
					CacheCreationTokens: 2_000_000, CacheCreation5mTokens: 1_000_000, CacheCreation1hTokens: 1_000_000,
				}, 1, tier)
				require.NoError(t, err)
				multiplier := 1.0
				if tier == "fast" {
					multiplier = 2
				}
				require.InDelta(t, 8*multiplier, cost.InputCost, 1e-10)
				require.InDelta(t, 40*multiplier, cost.OutputCost, 1e-10)
				require.InDelta(t, 26*multiplier, cost.CacheCreationCost, 1e-10)
				require.InDelta(t, .2*multiplier, cost.CacheReadCost, 1e-10)
				require.False(t, cost.LongContextBillingApplied)
			}
		}
		for _, model := range []string{"claude-fable-5", "claude-fable-5-1"} {
			p, err := svc.GetModelPricing(model)
			require.NoError(t, err)
			require.InDelta(t, 10e-6, p.InputPricePerToken, 1e-12)
		}
	}
	native := &Account{Platform: PlatformAnthropic}
	require.NotNil(t, anthropicSpeedServiceTier(native, "fast", "claude-fable-5-2"))
	require.Nil(t, anthropicSpeedServiceTier(native, "standard", "claude-fable-5-2"))
	require.Nil(t, anthropicSpeedServiceTier(&Account{Platform: PlatformAnthropic, Type: AccountTypeBedrock}, "fast", "claude-fable-5-2"))
	require.Nil(t, anthropicSpeedServiceTier(&Account{Platform: PlatformAntigravity}, "fast", "claude-fable-5-2"))
	for _, model := range []string{"claude-fable-5", "claude-fable-5-1", "claude-fable-5-20"} {
		require.Nil(t, anthropicSpeedServiceTier(native, "fast", model))
	}
}

func TestFable52RequestValidationBeforeForwarding(t *testing.T) {
	for _, body := range []string{`{}`, `{"thinking":{"type":"adaptive"}}`, `{"temperature":1}`, `{"top_p":0.99}`} {
		require.NoError(t, validateClaude55Request([]byte(body), "claude-fable-5-2"))
	}
	for _, field := range []string{
		`"thinking":{"type":"enabled","budget_tokens":1024}`, `"thinking":{"type":"disabled"}`, `"thinking":{"type":"between_tools"}`,
		`"tool_choice":{"type":"any"}`, `"tool_choice":{"type":"tool","name":"lookup"}`, `"tool_choice":"required"`,
		`"temperature":0.5`, `"top_p":1`, `"top_k":40`, `"temperature":1,"top_p":0.99`,
	} {
		for _, typ := range []string{AccountTypeOAuth, AccountTypeAPIKey, AccountTypeBedrock} {
			for _, count := range []bool{false, true} {
				model := "claude-fable-5-2"
				account := &Account{ID: 1, Platform: PlatformAnthropic, Type: typ}
				if typ == AccountTypeAPIKey {
					model = "public-fable"
					account.Credentials = map[string]any{"model_mapping": map[string]any{model: "claude-fable-5-2"}}
				}
				body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],` + field + `}`)
				parsed := &ParsedRequest{Model: model, Body: NewRequestBodyRef(body)}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				svc := &GatewayService{}
				var err error
				if count {
					err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
				} else {
					_, err = svc.Forward(context.Background(), c, account, parsed)
				}
				require.Error(t, err, field)
				require.Equal(t, http.StatusBadRequest, rec.Code, field)
				require.Contains(t, rec.Body.String(), "invalid_request_error", field)
			}
		}
	}
}

func TestFable52PreservesSignedHistoryAndSamplingDefaults(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5-2","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed"},{"type":"redacted_thinking","data":"encrypted"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}],"tool_choice":{"type":"none"},"top_p":0.99}`)
	require.JSONEq(t, string(body), string(FilterThinkingBlocks(body, "claude-fable-5-2")))
	out, _ := normalizeClaudeOAuthRequestBody(body, "claude-fable-5-2", claudeOAuthNormalizeOptions{})
	require.False(t, gjson.GetBytes(out, "temperature").Exists())
	require.Equal(t, "none", gjson.GetBytes(out, "tool_choice.type").String())
	require.JSONEq(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(out, "messages").Raw)
	require.NoError(t, validateClaude55Request(out, "claude-fable-5-2"))
}

func TestFable52CodexDescriptor(t *testing.T) {
	for _, model := range []string{"claude-fable-5-2", "anthropic/claude-fable-5-2"} {
		descriptor := newConfiguredCodexModelDescriptor(model)
		require.Equal(t, "Claude Fable 5.2", descriptor.DisplayName)
		require.Equal(t, int64(1_000_000), descriptor.ContextWindow)
		require.Equal(t, int64(1_000_000), descriptor.MaxContextWindow)
		require.Equal(t, "high", *descriptor.DefaultReasoningLevel)
		require.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, effortsFromConfiguredCodexLevels(descriptor.SupportedReasoningLevels))
	}
}
