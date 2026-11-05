package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSHTTPHandoffHistoryPreservesContext(t *testing.T) {
	first := []byte(`{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"first"}]}`)
	answer := json.RawMessage(`{"id":"msg_first","type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}`)
	call := json.RawMessage(`{"id":"fc_first","type":"function_call","call_id":"call_first","name":"shell","arguments":"{}"}`)
	next := []byte(`{"type":"response.create","previous_response_id":"resp_first","input":[{"type":"function_call_output","call_id":"call_first","output":"ok"}]}`)
	history := newOpenAIWSHTTPHandoffHistory(nil)
	history.record(first, &OpenAIForwardResult{
		RequestID: "resp_first", wsAccountFailoverReplayInput: []json.RawMessage{answer, call},
	})
	payload, safe, err := history.prepare(next)
	require.NoError(t, err)
	require.True(t, safe)
	require.False(t, gjson.GetBytes(payload, "previous_response_id").Exists())
	input := gjson.GetBytes(payload, "input").Array()
	require.Len(t, input, 4)
	require.Equal(t, "first", input[0].Get("content").String())
	require.Equal(t, "first answer", input[1].Get("content.0.text").String())
	require.Equal(t, "call_first", input[2].Get("call_id").String())
	require.Equal(t, "ok", input[3].Get("output").String())
	require.Equal(t, "resp_first", gjson.GetBytes(next, "previous_response_id").String(), "borrowed bodies must stay immutable")

	t.Run("unobserved continuation remains WS", func(t *testing.T) {
		unobserved := newOpenAIWSHTTPHandoffHistory(nil)
		_, safe, err := unobserved.prepare(next)
		require.NoError(t, err)
		require.False(t, safe)
		mismatch := []byte(strings.ReplaceAll(string(next), "resp_first", "resp_elsewhere"))
		_, safe, err = history.prepare(mismatch)
		require.NoError(t, err)
		require.False(t, safe)
	})
	t.Run("root history is not duplicated", func(t *testing.T) {
		root := []byte(`{"type":"response.create","input":[{"role":"user","content":"full history"}]}`)
		prepared, safe, err := history.prepare(root)
		require.NoError(t, err)
		require.True(t, safe)
		require.Equal(t, root, prepared)
	})
	t.Run("failed turn invalidates continuation", func(t *testing.T) {
		h := newOpenAIWSHTTPHandoffHistory(nil)
		h.record(first, &OpenAIForwardResult{
			RequestID: "resp_first", OpenAIWSMode: true, UpstreamTerminalEvent: "response.failed",
		})
		_, safe, err := h.prepare(next)
		require.NoError(t, err)
		require.False(t, safe)
	})
	t.Run("retained history is bounded and can recover on a new root", func(t *testing.T) {
		h := newOpenAIWSHTTPHandoffHistory(nil)
		h.limit = int64(len(first) + len(answer) - 1)
		h.record(first, &OpenAIForwardResult{RequestID: "resp_first", wsAccountFailoverReplayInput: []json.RawMessage{answer}})
		require.False(t, h.complete)
		require.Empty(t, h.input)
		h.record(first, &OpenAIForwardResult{RequestID: "resp_new"})
		require.True(t, h.complete)
		_, safe, err := h.prepare([]byte(`{"previous_response_id":"resp_new","input":"next"}`))
		require.NoError(t, err)
		require.True(t, safe)
	})
	t.Run("state outside response.create prevents handoff", func(t *testing.T) {
		h := newOpenAIWSHTTPHandoffHistory(nil)
		h.disableForSessionEvent()
		_, safe, err := h.prepare(first)
		require.NoError(t, err)
		require.False(t, safe)
	})
	t.Run("prewarm and unresolved references remain WS", func(t *testing.T) {
		for _, payload := range []string{
			`{"type":"response.create","generate":false,"input":"prewarm"}`,
			`{"type":"response.create","input":[{"type":"item_reference","id":"msg_remote"}]}`,
		} {
			_, safe, err := history.prepare([]byte(payload))
			require.NoError(t, err)
			require.False(t, safe)
		}
	})
	t.Run("oversized observed output discards retained history", func(t *testing.T) {
		h := newOpenAIWSHTTPHandoffHistory(nil)
		h.limit = 256
		h.begin(first)
		h.observe([]byte(`{"type":"response.completed","response":{"id":"resp_large","output":[{"type":"message","content":"` + strings.Repeat("x", 512) + `"}]}}`))
		require.False(t, h.complete)
		require.Empty(t, h.input)
		require.Empty(t, h.pending)
		require.Empty(t, h.output.allItems)
	})
}

func handoffTestSSE(id, answer string) *http.Response {
	event := fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"gpt-5.1","status":"completed","output":[{"id":%q,"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":7,"output_tokens":2}}}`, id, "msg_"+id, answer)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: " + event + "\n\n")),
	}
}

func TestOpenAIWSHTTPHandoffLaterImageTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, continuation := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/previous_id_%v", mode, continuation), func(t *testing.T) {
				cfg := passthroughLifecycleConfig()
				cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 4096
				imageData := strings.Repeat("AQID", 2048)
				if continuation {
					cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 15 * 1024 * 1024
					imageData = strings.Repeat("AQID", 17*1024*1024/4)
				}
				cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
				cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
				account := passthroughLifecycleAccount()
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				firstInput := `{"role":"user","content":"first question"}`
				firstOutput := `{"id":"msg_first","type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}`
				call := `{"id":"fc_first","type":"function_call","call_id":"call_first","name":"shell","arguments":"{}"}`
				terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","status":"completed","output":[%s,%s],"usage":{"input_tokens":1,"output_tokens":1}}}`, firstOutput, call)
				captureConn := &openAIWSCaptureConn{events: [][]byte{[]byte(terminal)}}
				captureDialer := &openAIWSCaptureDialer{conn: captureConn}
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(captureDialer)
				defer pool.Close()
				staged := newStagedPassthroughConn()
				staged.Send(terminal)
				upstream := &httpUpstreamRecorder{responses: []*http.Response{
					handoffTestSSE("resp_second", "second answer"),
					handoffTestSSE("resp_third", "third answer"),
				}}
				svc := newPassthroughLifecycleService(cfg, staged)
				svc.openaiWSPool = pool
				svc.httpUpstream = upstream

				var hooksMu sync.Mutex
				var admitted, requested, settled []int
				var results []*OpenAIForwardResult
				hooks := &OpenAIWSIngressHooks{
					BeforeRequest: func(turn int, _ []byte, _ string) error {
						hooksMu.Lock()
						defer hooksMu.Unlock()
						requested = append(requested, turn)
						return nil
					},
					BeforeTurn: func(turn int) error {
						hooksMu.Lock()
						defer hooksMu.Unlock()
						admitted = append(admitted, turn)
						return nil
					},
					AfterTurn: func(turn int, result *OpenAIForwardResult, err error) {
						hooksMu.Lock()
						defer hooksMu.Unlock()
						require.NoError(t, err)
						settled = append(settled, turn)
						results = append(results, result)
					},
				}
				serverErr := make(chan error, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						serverErr <- err
						return
					}
					defer func() { _ = conn.CloseNow() }()
					conn.SetReadLimit(ResolveOpenAIWSClientReadLimitBytes(cfg))
					ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
					defer cancel()
					_, first, err := conn.Read(ctx)
					if err != nil {
						serverErr <- err
						return
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = r.Clone(ctx)
					serverErr <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "sk-test", first, hooks)
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				require.NoError(t, err)
				defer func() { _ = client.CloseNow() }()
				exchange := func(payload, expectedID string) {
					t.Helper()
					require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(payload)))
					_, event, err := client.Read(ctx)
					require.NoError(t, err)
					require.Equal(t, expectedID, gjson.GetBytes(event, "response.id").String())
				}
				exchange(`{"type":"response.create","model":"gpt-5.1","input":[`+firstInput+`]}`, "resp_first")
				imageURL := "data:image/png;base64," + imageData
				images := `{"role":"user","content":[{"type":"input_image","image_url":"` + imageURL + `"}]}`
				toolOutput := `{"type":"function_call_output","call_id":"call_first","output":"tool result"}`
				input := firstInput + "," + firstOutput + "," + call + "," + toolOutput + "," + images
				previous := ""
				if continuation {
					input = toolOutput + "," + images
					previous = `"previous_response_id":"resp_first",`
				}
				exchange(`{"type":"response.create",`+previous+`"input":[`+input+`]}`, "resp_second")
				exchange(`{"type":"response.create","previous_response_id":"resp_second","input":[{"role":"user","content":"third question"}]}`, "resp_third")
				require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
				select {
				case err := <-serverErr:
					require.NoError(t, err)
				case <-ctx.Done():
					t.Fatal("HTTP handoff did not finish")
				}
				require.Len(t, upstream.bodies, 2, "only the second and third turns use HTTP")
				second := gjson.GetBytes(upstream.bodies[0], "input").Array()
				require.Len(t, second, 5)
				require.Equal(t, "first question", second[0].Get("content").String())
				require.Equal(t, "first answer", second[1].Get("content.0.text").String())
				require.Equal(t, "call_first", second[2].Get("call_id").String())
				require.Equal(t, "tool result", second[3].Get("output").String())
				require.Equal(t, imageURL, second[4].Get("content.0.image_url").String())
				third := gjson.GetBytes(upstream.bodies[1], "input").Array()
				require.Len(t, third, 7)
				require.Equal(t, "second answer", third[5].Get("content.0.text").String())
				require.Equal(t, "third question", third[6].Get("content").String())
				for _, body := range upstream.bodies {
					require.False(t, gjson.GetBytes(body, "previous_response_id").Exists())
					require.False(t, gjson.GetBytes(body, "type").Exists())
				}
				hooksMu.Lock()
				require.Equal(t, []int{2, 3}, requested)
				if mode == OpenAIWSIngressModeCtxPool {
					require.Equal(t, []int{1, 2, 3}, admitted)
				} else {
					require.Equal(t, []int{2, 3}, admitted)
				}
				require.Equal(t, []int{1, 2, 3}, settled)
				require.Len(t, results, 3)
				require.Equal(t, 7, results[1].Usage.InputTokens)
				hooksMu.Unlock()
				if mode == OpenAIWSIngressModeCtxPool {
					require.Equal(t, 1, captureDialer.DialCount())
					require.Len(t, captureConn.writes, 1, "oversized turn must never reach WS")
					require.True(t, captureConn.closed, "old lease must be closed on handoff")
				} else {
					require.Len(t, staged.writes, 1, "oversized turn must never reach WS")
					select {
					case <-staged.closed:
					default:
						t.Fatal("passthrough upstream was not released")
					}
				}
			})
		}
	}
}

func TestOpenAIHTTPIngressLargeImageBodyAvoidsWS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 4096
	account := &Account{
		ID: 907, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
		Extra:       map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool},
	}
	upstream := &httpUpstreamRecorder{resp: handoffTestSSE("resp_http_image", "done")}
	svc := &OpenAIGatewayService{
		cfg: cfg, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector(),
	}
	imageURL := "data:image/png;base64," + strings.Repeat("AQID", 2048)
	body := []byte(`{"model":"gpt-5.1","stream":true,"instructions":"inspect the images","input":[{"role":"user","content":[{"type":"input_image","image_url":"` + imageURL + `"}]}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "request_body_above_ws_threshold", c.GetString("openai_ws_transport_reason"))
	require.Equal(t, imageURL, gjson.GetBytes(upstream.lastBody, "input.0.content.0.image_url").String())
}

func TestOpenAIWSHTTPHandoffFailureDoesNotReplayCompletedTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 4096
	staged := newStagedPassthroughConn()
	staged.Send(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	svc := newPassthroughLifecycleService(cfg, staged)
	httpUpstream := &httpUpstreamRecorder{err: errors.New("synthetic HTTP transport failure")}
	svc.httpUpstream = httpUpstream
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var turns []int
	var turnErrors []error
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, passthroughLifecycleAccount(), func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{
			AfterTurn: func(turn int, _ *OpenAIForwardResult, err error) {
				turns = append(turns, turn)
				turnErrors = append(turnErrors, err)
			},
		}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.1","input":"first request"}`)
	defer func() { _ = client.CloseNow() }()
	_, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	payload := []byte(`{"type":"response.create","model":"gpt-5.1","input":"second request ` + strings.Repeat("x", 8192) + `"}`)
	require.NoError(t, client.Write(ctx, coderws.MessageText, payload))
	_, failure, readErr := client.Read(ctx)
	require.NoError(t, readErr)
	require.Equal(t, "error", gjson.GetBytes(failure, "type").String())
	require.Equal(t, int64(http.StatusBadGateway), gjson.GetBytes(failure, "status").Int())
	select {
	case err := <-serverErr:
		require.ErrorContains(t, err, "upstream http bridge request failed")
		// A later-turn transport failure has an ambiguous upstream outcome.
		// Report it to the client without authorizing an automatic replay.
		retry, retryable := OpenAIWSCurrentTurnRetryPayload(err)
		require.False(t, retryable)
		require.Empty(t, retry)
	case <-ctx.Done():
		t.Fatal("HTTP handoff failure did not finish")
	}
	require.Equal(t, []int{1, 2}, turns)
	require.NoError(t, turnErrors[0])
	require.Error(t, turnErrors[1])
	require.Len(t, httpUpstream.requests, 1)
	require.NotContains(t, string(httpUpstream.lastBody), "first request")
	require.Len(t, staged.writes, 1)
}

func BenchmarkOpenAIWSHTTPHandoffHistory(b *testing.B) {
	for _, itemBytes := range []int{1024, 16 * 1024, 64 * 1024} {
		b.Run(fmt.Sprintf("item_%d_KiB", itemBytes/1024), func(b *testing.B) {
			const turns = 32
			payloads := make([][]byte, turns)
			for i := range payloads {
				payloads[i] = buildReplayTurnPayload(i+1, itemBytes)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h := newOpenAIWSHTTPHandoffHistory(nil)
				for turn, payload := range payloads {
					h.record(payload, &OpenAIForwardResult{RequestID: fmt.Sprintf("resp_%d", turn)})
				}
				_, safe, err := h.prepare(payloads[turns-1])
				if err != nil || !safe {
					b.Fatalf("could not prepare complete history: %v", err)
				}
			}
		})
	}
}
