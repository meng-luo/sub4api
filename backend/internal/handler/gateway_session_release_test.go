//go:build unit

package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	middleware "github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type gatewaySessionReleaseCache struct {
	service.SessionLimitCache
	active          map[int64]string
	registered      int
	released        int
	releaseContexts []error
}

func (c *gatewaySessionReleaseCache) RegisterSession(_ context.Context, accountID int64, sessionID string, _ int, _ time.Duration) (bool, error) {
	c.active[accountID] = sessionID
	c.registered++
	return true, nil
}

func (c *gatewaySessionReleaseCache) UnregisterSession(ctx context.Context, accountID int64, sessionID string) error {
	if c.active[accountID] == sessionID {
		delete(c.active, accountID)
	}
	c.released++
	c.releaseContexts = append(c.releaseContexts, ctx.Err())
	return nil
}

type gatewaySessionQueueFullCache struct {
	fakeConcurrencyCache
	onQueueFull func()
}

func (c *gatewaySessionQueueFullCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return false, nil
}

func (c *gatewaySessionQueueFullCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	if c.onQueueFull != nil {
		c.onQueueFull()
	}
	return false, nil
}

type gatewaySessionReleaseWriter struct {
	*httptest.ResponseRecorder
	beforeHeader func()
}

func (w *gatewaySessionReleaseWriter) WriteHeader(status int) {
	if w.beforeHeader != nil {
		w.beforeHeader()
	}
	w.ResponseRecorder.WriteHeader(status)
}

func TestGatewayMessagesReleasesUnservedSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"forward_failure", "warmup_intercept", "wait_queue_full", "cancelled_wait"} {
		t.Run(scenario, func(t *testing.T) {
			groupID := int64(9400)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			account := &service.Account{
				ID: 9401, Platform: service.PlatformAnthropic, Type: service.AccountTypeSetupToken,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Extra:         map[string]any{"max_sessions": 1},
				AccountGroups: []service.AccountGroup{{AccountID: 9401, GroupID: groupID}},
			}
			prompt := "hello"
			if scenario == "warmup_intercept" {
				account.Credentials = map[string]any{"intercept_warmup_requests": true}
				prompt = "Warmup"
			}
			cache := &gatewaySessionReleaseCache{active: make(map[int64]string)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var concurrency *service.ConcurrencyService
			if scenario == "wait_queue_full" || scenario == "cancelled_wait" {
				queueCache := &gatewaySessionQueueFullCache{}
				if scenario == "cancelled_wait" {
					queueCache.onQueueFull = cancel
				}
				concurrency = service.NewConcurrencyService(queueCache)
			}
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
			cfg := &config.Config{RunMode: config.RunModeSimple}
			gw := service.NewGatewayService(
				nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg,
				snapshot, concurrency, nil, nil, nil, nil, nil, nil, nil, cache,
				nil, nil, nil, nil, nil, nil, nil, nil, nil,
			)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			h := &GatewayHandler{
				gatewayService: gw, billingCacheService: billing, cfg: cfg,
				concurrencyHelper:  NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
				maxAccountSwitches: 1,
			}
			if concurrency != nil {
				h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatClaude, 0)
			}
			writer := &gatewaySessionReleaseWriter{ResponseRecorder: httptest.NewRecorder()}
			if scenario == "forward_failure" {
				// Missing credentials fail before any real upstream call. The
				// session must be gone before the handler writes its error.
				writer.beforeHeader = func() {
					require.Equal(t, 1, cache.registered)
					require.Equal(t, 1, cache.released)
					require.Empty(t, cache.active)
				}
			}
			c, _ := gin.CreateTestContext(writer)
			body := `{"model":"claude-sonnet-4-5","max_tokens":256,"messages":[{"role":"user","content":"` + prompt + `"}]}`
			ctx = context.WithValue(ctx, ctxkey.Group, group)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			apiKey := &service.APIKey{
				ID: 9402, UserID: 9403, GroupID: &groupID, Group: group, Status: service.StatusActive,
				User: &service.User{ID: 9403, Concurrency: 10, Balance: 100},
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

			h.Messages(c)

			require.Equal(t, 1, cache.registered, "selection must reserve a session before the tested exit")
			require.Equal(t, 1, cache.released, "release immediately or through the early-return safeguard, exactly once")
			require.Empty(t, cache.active)
			require.Equal(t, []error{nil}, cache.releaseContexts, "cleanup must not inherit client cancellation")
			if scenario == "warmup_intercept" {
				require.Equal(t, http.StatusOK, writer.Code)
			} else {
				require.GreaterOrEqual(t, writer.Code, http.StatusBadRequest)
			}
		})
	}
}
