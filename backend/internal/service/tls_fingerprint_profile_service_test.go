//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MACOS-DO/sub4api/internal/model"
)

// newTestTLSFingerprintProfileService builds the service without touching the
// database. NewTLSFingerprintProfileService eagerly reloads from its repository,
// which a nil repo cannot serve, so tests construct the struct directly.
func newTestTLSFingerprintProfileService() *TLSFingerprintProfileService {
	return &TLSFingerprintProfileService{localCache: make(map[int64]*model.TLSFingerprintProfile)}
}

func anthropicOAuthAccount(extra map[string]any) *Account {
	return &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
}

// TestResolveTLSProfile_AnthropicOAuthIsAlwaysFingerprinted pins the P0 fix:
// the OAuth forwarding path unconditionally rewrites User-Agent, X-Stainless-*
// and the billing attribution block to claim it is the official CLI, so the
// transport layer must present a matching TLS fingerprint. The retired
// `enable_tls_fingerprint` account switch used to allow requests that claim
// Node 24 in their headers while presenting Go's crypto/tls on the wire — a
// cross-layer contradiction stronger than not mimicking at all. The switch is
// gone, and a leftover extra value must be inert either way.
func TestResolveTLSProfile_AnthropicOAuthIsAlwaysFingerprinted(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
	}{
		{name: "no extra", extra: nil},
		{name: "legacy extra says off", extra: map[string]any{"enable_tls_fingerprint": false}},
		{name: "legacy extra says on", extra: map[string]any{"enable_tls_fingerprint": true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestTLSFingerprintProfileService()
			profile := svc.ResolveTLSProfile(anthropicOAuthAccount(tc.extra))
			require.NotNil(t, profile, "Anthropic OAuth must never fall back to Go's native TLS")
			require.NotEmpty(t, profile.Name)
		})
	}
}

func TestResolveTLSProfile_SetupTokenIsAlwaysFingerprinted(t *testing.T) {
	svc := newTestTLSFingerprintProfileService()
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeSetupToken,
		Extra:    map[string]any{"enable_tls_fingerprint": false},
	}
	require.NotNil(t, svc.ResolveTLSProfile(account))
}

// TestResolveTLSProfile_OtherPlatformsStayUnfingerprinted guards the blast radius:
// only Anthropic OAuth/SetupToken accounts get the fingerprint dialer. Every
// other account — including one carrying a leftover extra flag — keeps resolving
// to nil and keeps using the platform's normal transport.
func TestResolveTLSProfile_OtherPlatformsStayUnfingerprinted(t *testing.T) {
	svc := newTestTLSFingerprintProfileService()

	for _, tc := range []struct {
		name string
		acct *Account
	}{
		{name: "openai oauth", acct: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}},
		{name: "openai oauth with legacy extra", acct: &Account{
			Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Extra: map[string]any{"enable_tls_fingerprint": true},
		}},
		{name: "anthropic api key", acct: &Account{
			Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
			Extra: map[string]any{"enable_tls_fingerprint": true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Nil(t, svc.ResolveTLSProfile(tc.acct))
		})
	}
}

// TestResolveTLSProfile_AnthropicOAuthBindsConfiguredProfile checks that forcing
// the fingerprint on OAuth does not discard an explicitly bound profile.
func TestResolveTLSProfile_AnthropicOAuthBindsConfiguredProfile(t *testing.T) {
	svc := newTestTLSFingerprintProfileService()
	svc.localCache[7] = &model.TLSFingerprintProfile{ID: 7, Name: "bound-profile"}

	profile := svc.ResolveTLSProfile(anthropicOAuthAccount(map[string]any{
		"enable_tls_fingerprint":     false, // retired; must be ignored
		"tls_fingerprint_profile_id": 7,
	}))
	require.NotNil(t, profile)
	require.Equal(t, "bound-profile", profile.Name)
}

func TestResolveTLSProfile_NilAccount(t *testing.T) {
	svc := newTestTLSFingerprintProfileService()
	require.Nil(t, svc.ResolveTLSProfile(nil))
}
