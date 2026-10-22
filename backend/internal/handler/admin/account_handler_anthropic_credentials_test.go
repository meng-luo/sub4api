package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type anthropicCredentialsAdminService struct {
	*stubAdminService
	account *service.Account
}

func (s *anthropicCredentialsAdminService) UpdateAccount(ctx context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	if _, err := s.stubAdminService.UpdateAccount(ctx, id, input); err != nil {
		return nil, err
	}
	s.account.Credentials = input.Credentials
	return s.account, nil
}

type anthropicCredentialsTokenInvalidator struct {
	accounts []*service.Account
	err      error
}

func (s *anthropicCredentialsTokenInvalidator) InvalidateToken(_ context.Context, account *service.Account) error {
	s.accounts = append(s.accounts, account)
	return s.err
}

func TestAccountHandler_Update_AnthropicOAuthCredentialCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name           string
		platform       string
		accountType    string
		credentials    map[string]any
		updateErr      error
		invalidateErr  error
		wantInvalidate bool
	}{
		{name: "access token", credentials: map[string]any{"access_token": "new-access"}, wantInvalidate: true},
		{name: "refresh token", credentials: map[string]any{"refresh_token": "new-refresh"}, wantInvalidate: true},
		{name: "expiry", credentials: map[string]any{"expires_at": "1893456000"}, wantInvalidate: true},
		{name: "clear expiry", credentials: map[string]any{"expires_at": ""}, wantInvalidate: true},
		{name: "all credentials invalidate once", credentials: map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "expires_at": "1893456000",
		}, wantInvalidate: true},
		{name: "unrelated settings", credentials: map[string]any{"intercept_warmup_requests": true}},
		{name: "no credentials"},
		{name: "other platform", platform: service.PlatformOpenAI, credentials: map[string]any{"access_token": "new-access"}},
		{name: "setup token", accountType: service.AccountTypeSetupToken, credentials: map[string]any{"access_token": "new-access"}},
		{name: "api key", accountType: service.AccountTypeAPIKey, credentials: map[string]any{"expires_at": "1893456000"}},
		{name: "failed update", credentials: map[string]any{"access_token": "new-access"}, updateErr: errors.New("update failed")},
		{name: "cache failure preserves save", credentials: map[string]any{"access_token": "new-access"}, invalidateErr: errors.New("cache unavailable"), wantInvalidate: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform, accountType := tt.platform, tt.accountType
			if platform == "" {
				platform = service.PlatformAnthropic
			}
			if accountType == "" {
				accountType = service.AccountTypeOAuth
			}
			stub := newStubAdminService()
			stub.updateAccountErr = tt.updateErr
			svc := &anthropicCredentialsAdminService{
				stubAdminService: stub,
				account: &service.Account{
					ID: 1, Platform: platform, Type: accountType, Status: service.StatusActive,
				},
			}
			invalidator := &anthropicCredentialsTokenInvalidator{err: tt.invalidateErr}
			handler := &AccountHandler{adminService: svc, tokenCacheInvalidator: invalidator}
			router := gin.New()
			router.PUT("/accounts/:id", handler.Update)
			body, err := json.Marshal(map[string]any{"credentials": tt.credentials})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPut, "/accounts/1", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if tt.updateErr != nil {
				require.Equal(t, http.StatusInternalServerError, recorder.Code)
			} else {
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, tt.credentials, stub.lastUpdateAccountInput.Credentials)
				require.NotContains(t, recorder.Body.String(), "new-access")
				require.NotContains(t, recorder.Body.String(), "new-refresh")
			}
			if tt.wantInvalidate {
				require.Equal(t, []*service.Account{svc.account}, invalidator.accounts)
			} else {
				require.Empty(t, invalidator.accounts)
			}
		})
	}
}
