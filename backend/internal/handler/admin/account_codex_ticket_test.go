package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountResponseCodexTicketsUsesConfiguredPolicy(t *testing.T) {
	account := &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive}
	h := &AccountHandler{cfg: &config.Config{}}
	models, err := service.ModelTraceTicketModels()
	require.NoError(t, err)
	initial := h.accountResponseFromService(account).CodexTurnTickets
	require.Len(t, initial, len(models))
	for _, ticket := range initial {
		require.False(t, ticket.HarvestEnabled)
	}
	h.cfg.Gateway.OpenAICodexTicket = config.OpenAICodexTicketConfig{Enabled: true, Models: models, FailClosed: false}
	status := h.accountListResponseFromService(account).CodexTurnTickets
	require.Len(t, status, len(models))
	require.Equal(t, models[0], status[0].Model)
	require.True(t, status[0].HarvestEnabled)
	require.False(t, status[0].Blocked)
	h.cfg.Gateway.OpenAICodexTicket.FailClosed = true
	require.True(t, h.accountResponseFromService(account).CodexTurnTickets[0].Blocked)
}

func TestCodexFingerprintVersionIncludesSharedModel(t *testing.T) {
	writer := httptest.NewRecorder()
	client, _ := gin.CreateTestContext(writer)
	(&AccountHandler{}).GetCodexFingerprintVersion(client)
	var models []string
	for _, model := range gjson.GetBytes(writer.Body.Bytes(), "data.models").Array() {
		models = append(models, model.String())
	}
	require.Equal(t, 200, writer.Code)
	require.Contains(t, models, "gpt-6.1-sol")
	require.Contains(t, models, "gpt-6-astra")
}

func TestAccountResponseCodexTicketsReadsLiveSettingsAfterRestart(t *testing.T) {
	cfg := &config.Config{}
	repo := &settingHandlerRepoStub{values: map[string]string{service.SettingKeyOpenAICodexTicketEnabled: "true"}}
	settings := service.NewSettingService(repo, cfg)
	h := &AccountHandler{cfg: cfg}
	h.SetCodexTicketSettings(settings)
	account := &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeSetupToken, Status: service.StatusActive}
	models, err := service.ModelTraceTicketModels()
	require.NoError(t, err)
	status := h.accountListResponseFromService(account).CodexTurnTickets
	require.Len(t, status, len(models))
	require.True(t, status[0].HarvestEnabled)
	require.False(t, cfg.Gateway.OpenAICodexTicket.Enabled)
	repo.values[service.SettingKeyOpenAICodexTicketEnabled] = "false"
	settings.InvalidateOpenAICodexTicketEnabledCache()
	status = h.accountResponseFromService(account).CodexTurnTickets
	require.Len(t, status, len(models))
	require.False(t, status[0].HarvestEnabled)
}
