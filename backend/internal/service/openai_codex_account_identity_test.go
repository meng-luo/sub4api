package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexAccountIdentityRepoStub struct {
	AccountRepository
	account *Account
}

func (s *codexAccountIdentityRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	return s.account, nil
}

func TestCodexRequestBodyIdentityNamespaceIsStablePerOAuthAccount(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-codex","prompt_cache_key":"client-session","client_metadata":{"x-codex-installation-id":"client-installation","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window","x-codex-turn-metadata":"{\"installation_id\":\"client-installation\",\"session_id\":\"client-session\",\"thread_id\":\"client-thread\",\"turn_id\":\"client-turn\",\"window_id\":\"client-window\"}"}}`)
	account11 := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11"}}
	account19 := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-19"}}

	first, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account11, 77, "")
	require.NoError(t, err)
	require.True(t, changed)
	firstAgain, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account11, 77, "")
	require.NoError(t, err)
	require.True(t, changed)
	second, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account19, 77, "")
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, string(first), string(firstAgain))

	paths := []string{
		"prompt_cache_key",
		"client_metadata.x-codex-installation-id",
		"client_metadata.session_id",
		"client_metadata.thread_id",
		"client_metadata.x-codex-window-id",
	}
	for _, path := range paths {
		require.NotEqual(t, gjson.GetBytes(body, path).String(), gjson.GetBytes(first, path).String(), path)
		require.NotEqual(t, gjson.GetBytes(first, path).String(), gjson.GetBytes(second, path).String(), path)
	}
	require.Equal(t, gjson.GetBytes(first, "prompt_cache_key").String(), gjson.GetBytes(first, "client_metadata.session_id").String())

	var embeddedFirst map[string]any
	var embeddedSecond map[string]any
	require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(first, "client_metadata.x-codex-turn-metadata").String()), &embeddedFirst))
	require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(second, "client_metadata.x-codex-turn-metadata").String()), &embeddedSecond))
	for _, field := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id"} {
		require.NotEqual(t, embeddedFirst[field], embeddedSecond[field], field)
	}
}

func TestCodexAccountIdentityNamespaceUsesStableCredentialSource(t *testing.T) {
	firstRow := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "shared-upstream-account"}}
	secondRow := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "shared-upstream-account"}}
	require.Equal(t, codexAccountIdentityNamespace(firstRow), codexAccountIdentityNamespace(secondRow))

	firstUser := &Account{ID: 20, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-account", "chatgpt_user_id": "user-1"}}
	sameUser := &Account{ID: 21, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-account", "chatgpt_user_id": "user-1"}}
	secondUser := &Account{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-account", "chatgpt_user_id": "user-2"}}
	require.Equal(t, codexAccountIdentityNamespace(firstUser), codexAccountIdentityNamespace(sameUser))
	require.NotEqual(t, codexAccountIdentityNamespace(firstUser), codexAccountIdentityNamespace(secondUser))

	seed := "11111111-1111-4111-8111-111111111111"
	seeded := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexFingerprintSeedExtraKey: seed}}
	require.Equal(t, "seed:"+seed, codexAccountIdentityNamespace(seeded))

	// Local row IDs repeat across independent deployments, so they are not a
	// safe fallback for upstream identity.
	require.Empty(t, codexAccountIdentityNamespace(&Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth}))

	setupTokenA := &Account{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{"access_token": "setup-token-a"}}
	setupTokenADuplicate := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{"access_token": "setup-token-a"}}
	setupTokenB := &Account{ID: 32, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{"access_token": "setup-token-b"}}
	setupNamespace := codexAccountIdentityNamespace(setupTokenA)
	require.NotEmpty(t, setupNamespace)
	require.NotContains(t, setupNamespace, "setup-token-a")
	require.Equal(t, setupNamespace, codexAccountIdentityNamespace(setupTokenADuplicate))
	require.NotEqual(t, setupNamespace, codexAccountIdentityNamespace(setupTokenB))
}

func TestCodexAccountIdentitySourceResolvesShadowAndOverwritesFailoverContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	parentID := int64(11)
	parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"chatgpt_account_id": "team-account",
		"chatgpt_user_id":    "user-1",
	}}
	shadow := &Account{ID: 111, ParentAccountID: &parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	service := &OpenAIGatewayService{accountRepo: &codexAccountIdentityRepoStub{account: parent}}

	resolved, err := service.prepareCodexAccountIdentitySource(context.Background(), c, shadow)
	require.NoError(t, err)
	require.Same(t, parent, resolved)
	require.Same(t, parent, codexAccountIdentitySource(c, shadow))

	req, err := service.buildUpstreamRequest(
		context.Background(), c, shadow,
		[]byte(`{"model":"gpt-5.6-codex","stream":true,"prompt_cache_key":"client-session"}`),
		"token", true, "client-session", true,
	)
	require.NoError(t, err)
	// 隔离值现在只体现在官方字段上：连字符 session-id 与 body 的 client_metadata/
	// prompt_cache_key（applyCodexAccountIdentity* 已作用域化）。官方从不发的
	// session_id 头必须缺席。
	require.Equal(t, isolateOpenAIUpstreamSessionID(0, parent, "client-session"), req.Header.Get("session-id"))
	require.Empty(t, req.Header.Get("session_id"))

	next := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"chatgpt_account_id": "other-account",
		"chatgpt_user_id":    "user-2",
	}}
	resolved, err = service.prepareCodexAccountIdentitySource(context.Background(), c, next)
	require.NoError(t, err)
	require.Same(t, next, resolved)
	require.Same(t, next, codexAccountIdentitySource(c, shadow))
}

func TestBuildOpenAIWSHeadersNamespacesCodexIdentityByOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set("api_key_id", int64(77))
	c.Request.Header.Set("x-codex-installation-id", "client-installation")
	c.Request.Header.Set("thread-id", "client-thread")
	c.Request.Header.Set("x-codex-window-id", "client-window")
	c.Request.Header.Set("x-client-request-id", "client-request")

	account11 := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11"}}
	account19 := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-19"}}
	service := &OpenAIGatewayService{}
	build := func(account *Account) http.Header {
		headers, _, err := service.buildOpenAIWSHeaders(
			context.Background(), c, account, "token",
			OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
			true, "", "", "client-session", "", "",
		)
		require.NoError(t, err)
		return headers
	}

	first := build(account11)
	firstAgain := build(account11)
	second := build(account19)
	// 线序收口后，官方 WS 握手只保留连字符会话头与 window/request 头；
	// session_id、x-codex-installation-id 是官方从不发的名字，必须缺席（值仍
	// 保存在帧内 client_metadata 中）。隔离性由官方字段承担。
	for _, header := range []string{"session-id", "thread-id", "x-codex-window-id", "x-client-request-id"} {
		require.NotEmpty(t, first.Get(header), header)
		require.Equal(t, first.Get(header), firstAgain.Get(header), header)
		require.NotEqual(t, first.Get(header), second.Get(header), header)
	}
	require.Empty(t, first.Get("session_id"), "official WS handshake never sends the underscored session header")
	require.Empty(t, first.Get("x-codex-installation-id"), "installation id belongs to client_metadata, not the handshake")
	require.Empty(t, first.Get("conversation_id"), "official WS handshake never sends the conversation_id header")
	require.Empty(t, first.Get("x-oai-attestation"), "ordinary Responses WS must not fabricate attestation")

	httpRequest, err := service.buildUpstreamRequest(
		context.Background(), c, account11,
		[]byte(`{"model":"gpt-5.6-codex","stream":true,"prompt_cache_key":"client-session"}`),
		"token", true, "client-session", true,
	)
	require.NoError(t, err)
	require.Equal(t, httpRequest.Header.Get("session-id"), first.Get("session-id"), "HTTP and WS must derive the same identity from the raw client key")
}

func TestBuildUpstreamRequestNamespacesCodexIdentityByOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6-codex","stream":true,"prompt_cache_key":"client-session"}`)

	build := func(accountID int64, chatgptAccountID string) http.Header {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Set("api_key", &APIKey{ID: 77})
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.0")
		c.Request.Header.Set("x-codex-installation-id", "client-installation")
		c.Request.Header.Set("x-codex-window-id", "client-window")
		c.Request.Header.Set("session-id", "client-session")
		c.Request.Header.Set("thread-id", "client-thread")
		c.Request.Header.Set("x-client-request-id", "client-request")
		c.Request.Header.Set("x-codex-turn-metadata", `{"installation_id":"client-installation","session_id":"client-session","thread_id":"client-thread","turn_id":"client-turn","window_id":"client-window"}`)

		account := &Account{
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"chatgpt_account_id": chatgptAccountID,
			},
		}
		req, err := svc.buildUpstreamRequest(
			context.Background(), c, account, body, "oauth-token", true, "client-session", true,
		)
		require.NoError(t, err)
		return req.Header
	}

	first := build(11, "chatgpt-account-11")
	firstAgain := build(11, "chatgpt-account-11")
	second := build(19, "chatgpt-account-19")

	// 官方 /responses 线序只携带连字符会话头 + window/request/turn-metadata；
	// session_id、conversation_id、x-codex-installation-id 官方从不发，必须缺席
	// （隔离与安装 ID 仍通过官方头与 body client_metadata 传递）。
	identityHeaders := []string{
		"x-codex-window-id",
		"session-id",
		"thread-id",
		"x-client-request-id",
	}
	checked := 0
	for _, header := range identityHeaders {
		require.NotEmpty(t, first.Get(header), header)
		require.Equal(t, first.Get(header), firstAgain.Get(header), "same account must retain stable identity: %s", header)
		require.NotEqual(t, first.Get(header), second.Get(header), "account failover must rotate upstream identity: %s", header)
		checked++
	}
	for _, header := range []string{"session_id", "conversation_id", "x-codex-installation-id"} {
		require.Empty(t, first.Get(header), "official /responses never sends %s", header)
	}

	// turn-metadata 含逐请求生成的 turn_started_at_unix_ms，只能比较身份字段，
	// 否则时间戳跨毫秒抖动会让断言随机失败。
	metadataIdentity := func(h http.Header) map[string]any {
		t.Helper()
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(h.Get(openAIWSTurnMetadataHeader)), &decoded))
		delete(decoded, "turn_started_at_unix_ms")
		return decoded
	}
	firstMeta, repeatMeta, secondMeta := metadataIdentity(first), metadataIdentity(firstAgain), metadataIdentity(second)
	require.Equal(t, firstMeta, repeatMeta, "same account must retain stable identity: x-codex-turn-metadata")
	require.NotEqual(t, firstMeta, secondMeta, "account failover must rotate upstream identity: x-codex-turn-metadata")
	require.Equal(t, first.Get("session-id"), firstMeta["session_id"], "session identity must agree between header and body metadata")
	require.GreaterOrEqual(t, checked, 4, "test must exercise the real outbound identity surface")
}

// An ephemeral Codex fork, such as thread/sample, names itself in
// client_metadata.session_id but keeps its parent's session-id header and
// prompt_cache_key. Upstream, the fork must keep the parent's cache key.
func TestCodexForkKeepsParentPromptCacheKeyUpstream(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "transformed", true: "passthrough"}[passthrough], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			forward := func(promptCacheKey, bodySessionID string) (http.Header, []byte) {
				t.Helper()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.167.0")
				c.Request.Header.Set("originator", "codex_cli_rs")
				c.Request.Header.Set("session-id", "parent-session")
				body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":"hi"}]}`)
				body, err := sjson.SetBytes(body, "prompt_cache_key", promptCacheKey)
				require.NoError(t, err)
				body, err = sjson.SetBytes(body, "client_metadata.session_id", bodySessionID)
				require.NoError(t, err)

				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid"}},
					Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
				}}
				svc := &OpenAIGatewayService{
					cfg:           &config.Config{},
					httpUpstream:  upstream,
					toolCorrector: NewCodexToolCorrector(),
				}
				account := newTestOAuthAccount(4501, map[string]any{"openai_oauth_passthrough": passthrough})
				account.Status = StatusActive
				account.Schedulable = true
				account.Concurrency = 1
				account.Credentials = map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"}

				_, err = svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, upstream.lastReq)
				return upstream.lastReq.Header, upstream.lastBody
			}

			parentHeader, parentBody := forward("parent-session", "parent-session")
			parentKey := gjson.GetBytes(parentBody, "prompt_cache_key").String()
			require.NotEqual(t, "parent-session", parentKey, "the key is account-scoped")
			require.Equal(t, gjson.GetBytes(parentBody, "client_metadata.session_id").String(), parentKey)

			forkHeader, forkBody := forward("parent-session", "fork-session")
			require.Equal(t, parentKey, gjson.GetBytes(forkBody, "prompt_cache_key").String(),
				"a fork keeps its parent's upstream cache key")
			require.Equal(t, parentHeader.Get("session_id"), forkHeader.Get("session_id"))
			require.NotEqual(t,
				gjson.GetBytes(parentBody, "client_metadata.session_id").String(),
				gjson.GetBytes(forkBody, "client_metadata.session_id").String(),
				"the fork keeps its own session identity",
			)

			_, customBody := forward("caller-cache-key", "parent-session")
			require.NotEqual(t, parentKey, gjson.GetBytes(customBody, "prompt_cache_key").String(),
				"a caller-chosen key keeps its own namespace")
		})
	}
}

// 官方 Codex 客户端对同一份身份有多个出口，且它们在真机上恒等：
// session_id == thread_id == x-client-request-id == window_id 前缀 == prompt_cache_key，
// 新 turn 时 turn_id == root_turn_id，且所有会话/轮次标识都是 UUIDv7，
// 只有 installation_id 是 UUIDv4。
//
// 账号级 namespace 改写必须逐条保住这些关系：种子若按字段名派生，同样的原值会
// 派生出不同结果，出站就成了「session_id 与 thread_id 互不相等」这种真实客户端
// 永远发不出的形状。
func TestCodexAccountIdentityPreservesOfficialCodexIdentities(t *testing.T) {
	const (
		thread = "01a2064b-8ab7-75c3-8517-bd927169a05a"
		turn   = "01a2064b-8aca-7672-8207-879f6d4e26bf"
		ctxWin = "01a2064b-8aba-7070-a8cf-65af63980838"
		inst   = "a3f880d7-091f-4604-9c0f-87e4a5016e99"
	)
	embedded := `{"installation_id":"` + inst + `","session_id":"` + thread + `","thread_id":"` + thread +
		`","turn_id":"` + turn + `","root_turn_id":"` + turn + `","window_id":"` + thread + `:0","window_number":0,"context_window_id":"` + ctxWin +
		`","turn_started_at_unix_ms":1795401943756}`
	body := []byte(`{"prompt_cache_key":"` + thread + `","client_metadata":{"thread_id":"` + thread +
		`","root_turn_id":"` + turn + `","turn_id":"` + turn + `","session_id":"` + thread +
		`","x-codex-installation-id":"` + inst + `","x-codex-window-id":"` + thread + `:0","context_window_id":"` + ctxWin +
		`","x-codex-turn-metadata":` + strconv.Quote(embedded) + `}}`)
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11", "chatgpt_user_id": "user-11"}}

	scoped, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 77, thread)
	require.NoError(t, err)
	require.True(t, changed)

	value := func(path string) string { return gjson.GetBytes(scoped, path).String() }
	session := value("client_metadata.session_id")
	scopedThread := value("client_metadata.thread_id")
	scopedTurn := value("client_metadata.turn_id")
	scopedRootTurn := value("client_metadata.root_turn_id")
	scopedWindow := value("client_metadata.x-codex-window-id")
	scopedCtx := value("client_metadata.context_window_id")
	scopedInst := value("client_metadata.x-codex-installation-id")

	// 恒等关系逐条保住。
	require.Equal(t, scopedThread, session, "session_id 必须等于 thread_id")
	require.Equal(t, scopedThread, value("prompt_cache_key"), "prompt_cache_key 必须等于 thread_id")
	require.Equal(t, scopedThread+":0", scopedWindow, "window_id 必须是 thread_id + 序号")
	require.Equal(t, scopedTurn, scopedRootTurn, "新 turn 的 root_turn_id 必须等于 turn_id")

	// 不该相等的必须不相等。
	require.NotEqual(t, scopedThread, scopedTurn, "turn_id 必须与 thread_id 不同")
	require.NotEqual(t, scopedThread, scopedCtx, "context_window_id 必须与 thread_id 不同")
	require.NotEqual(t, scopedInst, scopedThread)

	// 版本形态：会话/轮次/上下文窗口是 v7，installation 是 v4。
	for name, raw := range map[string]string{
		"session_id": session, "thread_id": scopedThread, "turn_id": scopedTurn,
		"root_turn_id": scopedRootTurn, "context_window_id": scopedCtx,
	} {
		parsed, parseErr := uuid.Parse(raw)
		require.NoErrorf(t, parseErr, "%s 必须是合法 UUID: %s", name, raw)
		require.Equalf(t, uuid.Version(7), parsed.Version(), "%s 必须是 UUIDv7: %s", name, raw)
	}
	parsedInst, err := uuid.Parse(scopedInst)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(4), parsedInst.Version(), "installation_id 必须是 UUIDv4")

	// 内嵌 metadata 与扁平 metadata 必须同源，不能各改一套。
	var nested map[string]any
	require.NoError(t, json.Unmarshal([]byte(value("client_metadata.x-codex-turn-metadata")), &nested))
	require.Equal(t, scopedThread, nested["session_id"])
	require.Equal(t, scopedThread, nested["thread_id"])
	require.Equal(t, scopedTurn, nested["turn_id"])
	require.Equal(t, scopedRootTurn, nested["root_turn_id"])
	require.Equal(t, scopedWindow, nested["window_id"])
	require.Equal(t, scopedCtx, nested["context_window_id"])

	// turn_id 保留原值的 v7 时间戳，仍可与 turn_started_at_unix_ms 交叉验证。
	scopedTurnUUID, err := uuid.Parse(scopedTurn)
	require.NoError(t, err)
	originalTurnUUID, err := uuid.Parse(turn)
	require.NoError(t, err)
	require.Equal(t,
		int64(binary.BigEndian.Uint16(originalTurnUUID[0:2]))<<32|int64(binary.BigEndian.Uint32(originalTurnUUID[2:6])),
		int64(binary.BigEndian.Uint16(scopedTurnUUID[0:2]))<<32|int64(binary.BigEndian.Uint32(scopedTurnUUID[2:6])),
		"改写必须保留 turn_id 的 v7 时间戳",
	)
}

// 子线程通过两个出口引用父线程：元数据 parent_thread_id 与握手头
// x-codex-parent-thread-id（openai_ws_forwarder_payload.go 逐字转发该头）。
// 只改写其中一处会让同一请求携带两个不同的父线程引用。
func TestCodexAccountIdentityKeepsParentThreadReferenceConsistent(t *testing.T) {
	const (
		parent = "01a2064b-8ab7-75c3-8517-bd927169a05a"
		child  = "01a2064b-8b01-7111-9a2b-000000000001"
		turn   = "01a2064b-8aca-7672-8207-879f6d4e26bf"
		inst   = "a3f880d7-091f-4604-9c0f-87e4a5016e99"
	)
	embedded := `{"installation_id":"` + inst + `","session_id":"` + child + `","thread_id":"` + child +
		`","turn_id":"` + turn + `","root_turn_id":"` + turn + `","parent_thread_id":"` + parent + `"}`
	body := []byte(`{"client_metadata":{"thread_id":"` + child + `","session_id":"` + child +
		`","parent_thread_id":"` + parent + `","x-codex-turn-metadata":` + strconv.Quote(embedded) + `}}`)
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11", "chatgpt_user_id": "user-11"}}

	headers := http.Header{}
	headers.Set("thread-id", child)
	headers.Set(codexParentThreadIDHeader, parent)
	applyCodexAccountIdentityHeaders(headers, account, 77)

	scoped, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 77, child)
	require.NoError(t, err)
	require.True(t, changed)

	scopedHeader := headers.Get(codexParentThreadIDHeader)
	require.NotEmpty(t, scopedHeader)
	require.NotEqual(t, parent, scopedHeader, "父线程引用必须随账号重新派生")
	require.Equal(t, gjson.GetBytes(scoped, "client_metadata.parent_thread_id").String(), scopedHeader,
		"握手头与元数据必须引用同一个父线程")

	var nested map[string]any
	require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(scoped, "client_metadata.x-codex-turn-metadata").String()), &nested))
	require.Equal(t, scopedHeader, nested["parent_thread_id"],
		"内嵌元数据同样必须引用同一个父线程")

	// 派生值仍是合法 UUIDv7，且与子线程不同。
	parsed, err := uuid.Parse(scopedHeader)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsed.Version())
	require.NotEqual(t, headers.Get("thread-id"), scopedHeader)
}
