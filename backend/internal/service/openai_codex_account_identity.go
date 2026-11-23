package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexAccountIdentityNamespaceVersion 参与派生种子，改动即代表派生规则换代。
// v2：种子由「字段名(kind)」改为「同义字段族(family)」，使 session_id / thread_id /
// x-client-request-id / prompt_cache_key 收敛到同一个值（与官方客户端恒等关系一致）。
const codexAccountIdentityNamespaceVersion = "v2"

const codexAccountIdentitySourceContextKey = "openai_codex_account_identity_source"

// prepareCodexAccountIdentitySource resolves credential shadows once per selected
// attempt. The handler reuses gin.Context across failover attempts, so every entry
// point overwrites the staged source before projecting outbound identity.
func (s *OpenAIGatewayService) prepareCodexAccountIdentitySource(ctx context.Context, c *gin.Context, account *Account) (*Account, error) {
	source := account
	if account != nil && account.IsShadow() {
		resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return nil, err
		}
		source = resolved
	}
	if c != nil {
		c.Set(codexAccountIdentitySourceContextKey, source)
	}
	return source, nil
}

func codexAccountIdentitySource(c *gin.Context, fallback *Account) *Account {
	if c != nil {
		if staged, ok := c.Get(codexAccountIdentitySourceContextKey); ok {
			if source, ok := staged.(*Account); ok && source != nil {
				return source
			}
		}
	}
	return fallback
}

// codexClientSessionID returns the client's own session header, before any
// outbound rewrite.
func codexClientSessionID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return extractClientSessionID(c.Request.Header)
}

// codexAccountIdentityNamespace returns a stable, credential-scoped namespace.
// Multiple local rows that use the same ChatGPT account intentionally share the
// same namespace. Setup tokens use an irreversible bearer fingerprint because
// they have no refresh lifecycle or imported account metadata. Refreshable OAuth
// otherwise falls back only to a persistent fingerprint seed: local row IDs are
// deployment-relative and must never become upstream identity.
func codexAccountIdentityNamespace(account *Account) string {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return ""
	}
	if upstreamAccountID := strings.TrimSpace(account.GetChatGPTAccountID()); upstreamAccountID != "" {
		if upstreamUserID := strings.TrimSpace(account.GetCredential("chatgpt_user_id")); upstreamUserID != "" {
			return "chatgpt:" + upstreamAccountID + ":user:" + upstreamUserID
		}
		return "chatgpt:" + upstreamAccountID
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		return "seed:" + seed
	}
	if account.Type == AccountTypeSetupToken {
		if token := strings.TrimSpace(account.GetOpenAIAccessToken()); token != "" {
			sum := sha256.Sum256([]byte("openai-setup-token:" + token))
			return fmt.Sprintf("setup-token:%x", sum[:16])
		}
	}
	return ""
}

// isolateOpenAIUpstreamSessionID preserves the existing API-key isolation while
// adding the selected OAuth credential namespace. A scheduler failover therefore
// cannot send the same session/conversation identity through two upstream accounts.
func isolateOpenAIUpstreamSessionID(apiKeyID int64, account *Account, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	namespace := codexAccountIdentityNamespace(account)
	if namespace == "" {
		return isolateOpenAISessionID(apiKeyID, raw)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("u%d:a%s:%s", apiKeyID, namespace, raw)))
	return fmt.Sprintf("%x", sum[:8])
}

func scopeCodexAccountIdentityValue(account *Account, apiKeyID int64, kind, raw string) string {
	raw = strings.TrimSpace(raw)
	namespace := codexAccountIdentityNamespace(account)
	if raw == "" || namespace == "" {
		return raw
	}
	// Window IDs carry the thread identity and its context-window ordinal.
	// Hashing the whole value loses that relationship after compaction.
	if kind == "window" {
		if thread, ordinal, ok := splitCodexWindowID(raw); ok {
			return scopeCodexAccountIdentityValue(account, apiKeyID, "thread", thread) + ":" + ordinal
		}
	}
	family, derivation := codexAccountIdentityFamily(kind)
	seed := fmt.Sprintf(
		"sub2api:codex-account-identity:%s:user:%d:account:%s:family:%s:value:%s",
		codexAccountIdentityNamespaceVersion,
		apiKeyID,
		namespace,
		family,
		raw,
	)
	// 官方 session_id / thread_id / turn_id / context_window_id 恒为 Uuid::now_v7()，
	// installation_id 才是 Uuid::new_v4()。改写成账号级值时必须保持同一版本，否则出站头
	// 的版本半字节（v4 vs v7）与官方客户端不一致；同时保留原值的 v7 时间戳，使
	// 「turn_id 时刻 ≈ turn_started_at」这类官方时间一致性在改写后依然成立。
	// 原值不是 v7 时退回种子派生的稳定过去锚点。
	if derivation == codexIdentityV7 {
		anchorMs, ok := codexV7TimestampMs(raw)
		if !ok {
			anchorMs = deriveCodexStableTimestampMs(seed)
		}
		return deriveStableCodexUUIDv7(seed, anchorMs)
	}
	return deriveStableUUIDv4(seed)
}

// codexIdentityDerivation 是派生结果的版本形态：官方 installation_id 是 v4，
// 其余会话/轮次标识都是 v7。
type codexIdentityDerivation int

const (
	codexIdentityV4 codexIdentityDerivation = iota
	codexIdentityV7
)

// codexAccountIdentityFamily 把同义字段归并到同一个派生族，并给出该族的 uuid 版本。
//
// 官方 Codex 客户端对同一份身份有多个出口，且它们在真机上**恒等**：
// session_id == thread_id == x-client-request-id == window_id 前缀 == prompt_cache_key。
// 派生种子若带上字段名（kind），同样的原值会派生出不同结果，直接破坏这条恒等式——
// 出站后 session_id 与 thread_id 互不相等，而真实客户端永远相等。因此种子只按「族」
// 区分，族内所有字段收敛到同一个值。
//
// 族之间必须保持不等的：installation（v4）、thread、turn、context-window。
// 官方 turn_id 与 thread_id 不同，root_turn_id 与首 turn 的 turn_id 相同，故 turn
// 族内 turn_id / root_turn_id / parent_turn_id 由同一份原值派生即自动保持恒等。
func codexAccountIdentityFamily(kind string) (string, codexIdentityDerivation) {
	switch kind {
	case "session", "thread":
		return "thread", codexIdentityV7
	case "turn":
		return "turn", codexIdentityV7
	case "context-window":
		return "context-window", codexIdentityV7
	case "prompt-cache":
		return "prompt-cache", codexIdentityV7
	default:
		return "installation", codexIdentityV4
	}
}

var codexAccountIdentityFields = []struct {
	name string
	kind string
}{
	{name: "installation_id", kind: "installation"},
	{name: "x-codex-installation-id", kind: "installation"},
	// session / thread / x-client-request-id 在官方客户端上是同一个值（同一派生族）。
	{name: "session_id", kind: "session"},
	{name: "session-id", kind: "session"},
	{name: "thread_id", kind: "thread"},
	{name: "thread-id", kind: "thread"},
	{name: "x-client-request-id", kind: "thread"},
	{name: "parent_thread_id", kind: "thread"},
	{name: "forked_from_thread_id", kind: "thread"},
	// 子线程同时通过元数据 parent_thread_id 与握手头 x-codex-parent-thread-id 引用父线程
	// （openai_ws_forwarder_payload.go 逐字转发该头）。只改一处会让同一请求携带两个
	// 不同的父线程引用，必须与元数据同族、同原值、同派生值。
	{name: codexParentThreadIDHeader, kind: "thread"},
	// turn 族：官方新 turn 时 turn_id == root_turn_id，二者必须由同一份原值派生。
	{name: "turn_id", kind: "turn"},
	{name: "turn-id", kind: "turn"},
	{name: "root_turn_id", kind: "turn"},
	{name: "parent_turn_id", kind: "turn"},
	// context_window_id 是与 thread 同族但独立的一条 v7。
	{name: "context_window_id", kind: "context-window"},
	{name: "window_id", kind: "window"},
	{name: "x-codex-window-id", kind: "window"},
}

func applyCodexAccountIdentityFields(values map[string]any, account *Account, apiKeyID int64) bool {
	if values == nil || codexAccountIdentityNamespace(account) == "" {
		return false
	}
	changed := false
	for _, field := range codexAccountIdentityFields {
		raw, ok := values[field.name].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		next := scopeCodexAccountIdentityValue(account, apiKeyID, field.kind, raw)
		if next != raw {
			values[field.name] = next
			changed = true
		}
	}
	return changed
}

func applyCodexAccountIdentityEmbeddedMetadata(values map[string]any, account *Account, apiKeyID int64) bool {
	raw, ok := values[openAIWSTurnMetadataHeader].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return false
	}
	metadata, err := decodeCodexTurnMetadata(raw)
	if err != nil || metadata == nil {
		return false
	}
	if !applyCodexAccountIdentityFields(metadata, account, apiKeyID) {
		return false
	}
	rebuilt, err := marshalCodexTurnMetadata(metadata)
	if err != nil {
		return false
	}
	values[openAIWSTurnMetadataHeader] = string(rebuilt)
	return true
}

// codexPromptCacheKeyKind scopes a prompt_cache_key that names the client's
// session in the session namespace, so it stays equal to the scoped session.
// Codex derives the key from its session-id header, which matches
// client_metadata.session_id except on an ephemeral fork such as thread/sample:
// the fork names itself in the metadata but keeps its parent's header and key.
func codexPromptCacheKeyKind(promptCacheKey, bodySessionID, clientSessionID string) string {
	for _, sessionID := range []string{bodySessionID, clientSessionID} {
		if strings.TrimSpace(sessionID) != "" && promptCacheKey == sessionID {
			return "session"
		}
	}
	return "prompt-cache"
}

func applyCodexAccountIdentityClientMetadataMap(requestBody map[string]any, account *Account, apiKeyID int64, clientSessionID string) bool {
	if requestBody == nil || codexAccountIdentityNamespace(account) == "" {
		return false
	}
	changed := false
	clientMetadata, _ := requestBody["client_metadata"].(map[string]any)
	originalBodySessionID := ""
	if clientMetadata != nil {
		originalBodySessionID, _ = clientMetadata["session_id"].(string)
		if applyCodexAccountIdentityFields(clientMetadata, account, apiKeyID) {
			changed = true
		}
		if applyCodexAccountIdentityEmbeddedMetadata(clientMetadata, account, apiKeyID) {
			changed = true
		}
	}
	if raw, ok := requestBody["prompt_cache_key"].(string); ok && strings.TrimSpace(raw) != "" {
		kind := codexPromptCacheKeyKind(raw, originalBodySessionID, clientSessionID)
		next := scopeCodexAccountIdentityValue(account, apiKeyID, kind, raw)
		if next != raw {
			requestBody["prompt_cache_key"] = next
			changed = true
		}
	}
	return changed
}

// applyCodexAccountIdentityClientMetadataRaw scopes only the small identity
// subobjects with gjson/sjson. The passthrough hot path never unmarshals the
// potentially multi-megabyte request body.
func applyCodexAccountIdentityClientMetadataRaw(body []byte, account *Account, apiKeyID int64, clientSessionID string) ([]byte, bool, error) {
	if len(body) == 0 || codexAccountIdentityNamespace(account) == "" {
		return body, false, nil
	}
	// parseRawJSONView 走 unsafe 零拷贝视图：gjson.ParseBytes 会把
	// []byte 转成 string 从而为整个请求体复制一份（最大数十 MB）。
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return body, false, nil
	}

	next := body
	changed := false
	originalBodySessionID := ""
	if cm := gjson.GetBytes(body, "client_metadata"); cm.IsObject() {
		clientMetadata := map[string]any{}
		if err := decodeCodexMetadataObject(cm.Raw, &clientMetadata); err != nil {
			return body, false, fmt.Errorf("decode client_metadata for account identity: %w", err)
		}
		originalBodySessionID, _ = clientMetadata["session_id"].(string)
		metadataChanged := applyCodexAccountIdentityFields(clientMetadata, account, apiKeyID)
		if applyCodexAccountIdentityEmbeddedMetadata(clientMetadata, account, apiKeyID) {
			metadataChanged = true
		}
		if metadataChanged {
			raw, err := json.Marshal(clientMetadata)
			if err != nil {
				return body, false, fmt.Errorf("encode account-scoped client_metadata: %w", err)
			}
			var setErr error
			next, setErr = sjson.SetRawBytes(next, "client_metadata", raw)
			if setErr != nil {
				return body, false, fmt.Errorf("splice account-scoped client_metadata: %w", setErr)
			}
			changed = true
		}
	}
	if promptCacheKey := gjson.GetBytes(body, "prompt_cache_key"); promptCacheKey.Type == gjson.String && strings.TrimSpace(promptCacheKey.String()) != "" {
		raw := promptCacheKey.String()
		kind := codexPromptCacheKeyKind(raw, originalBodySessionID, clientSessionID)
		scoped := scopeCodexAccountIdentityValue(account, apiKeyID, kind, raw)
		if scoped != raw {
			rewritten, err := sjson.SetBytes(next, "prompt_cache_key", scoped)
			if err != nil {
				return body, false, fmt.Errorf("splice account-scoped prompt_cache_key: %w", err)
			}
			next = rewritten
			changed = true
		}
	}
	return next, changed, nil
}

func applyCodexAccountIdentityHeaders(headers http.Header, account *Account, apiKeyID int64) {
	if headers == nil || codexAccountIdentityNamespace(account) == "" {
		return
	}
	for _, field := range codexAccountIdentityFields {
		// Underscore session/conversation headers are rebuilt separately from the
		// prompt cache key by each request builder.
		if field.name == "session_id" {
			continue
		}
		raw := strings.TrimSpace(headers.Get(field.name))
		if raw != "" {
			headers.Set(field.name, scopeCodexAccountIdentityValue(account, apiKeyID, field.kind, raw))
		}
	}
	if raw := strings.TrimSpace(headers.Get(openAIWSTurnMetadataHeader)); raw != "" {
		metadata, err := decodeCodexTurnMetadata(raw)
		if err == nil && metadata != nil && applyCodexAccountIdentityFields(metadata, account, apiKeyID) {
			if rebuilt, err := marshalCodexTurnMetadata(metadata); err == nil {
				headers.Set(openAIWSTurnMetadataHeader, string(rebuilt))
			}
		}
	}
}
