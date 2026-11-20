package openaitransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	pluginv1 "github.com/MACOS-DO/sub4api/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

const bodyChunkSize = 32 * 1024

// forwardResult 是 client.Do 的异步结果。
type forwardResult struct {
	resp *http.Response
	err  error
}

// Forward 处理一次出站请求：接收 start/body 帧，发出上游请求，回传 start/body/end。
func (p *Plugin) Forward(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse]) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return p.sendError(stream, "PLUGIN_BAD_FRAME", errNoStartFrame.Error(), false)
	}

	target, err := url.Parse(start.GetUrl())
	if err != nil || target.Scheme == "" || target.Host == "" {
		return p.sendError(stream, "PLUGIN_BAD_URL", "请求 URL 非法", false)
	}

	transport, err := p.transportFor(start.GetAccountId(), start.GetProxyUrl())
	if err != nil {
		return p.sendError(stream, "PLUGIN_BAD_PROXY", err.Error(), false)
	}
	cfg := p.currentConfig()

	bodyReader, bodyWriter := io.Pipe()
	req := &http.Request{
		Method:        strings.ToUpper(start.GetMethod()),
		URL:           target,
		Host:          start.GetHost(),
		Header:        headerFromProto(start.GetHeaders()),
		Body:          http.NoBody,
		ContentLength: start.GetContentLength(),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
	}
	if req.Method == "" {
		req.Method = http.MethodPost
	}
	if start.GetHasBody() {
		req.Body = bodyReader
		if req.ContentLength == 0 {
			req.ContentLength = -1
		}
	} else {
		_ = bodyWriter.Close()
	}

	// requestSent 一旦置位，任何错误都必须标记为「可能已发出」，宿主据此禁止重放。
	var requestSent atomic.Bool
	resultCh := make(chan forwardResult, 1)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		client := &http.Client{
			Transport: transport,
			// 不跟随重定向：上游返回 3xx 必须原样交给宿主判断。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       0,
		}
		requestSent.Store(true)
		req = req.WithContext(reqCtx)
		resp, err := client.Do(req)
		if err != nil {
			// 请求体可能仍在发送，解除写端阻塞。
			_ = bodyWriter.CloseWithError(err)
		}
		resultCh <- forwardResult{resp: resp, err: err}
	}()

	// 先把请求体全部转发出去（上游在收完 body 前不会给响应）。
	for {
		frame, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				_ = bodyWriter.Close()
				break
			}
			_ = bodyWriter.CloseWithError(recvErr)
			cancel()
			return recvErr
		}
		if chunk := frame.GetBodyChunk(); chunk != nil {
			if _, werr := bodyWriter.Write(chunk); werr != nil {
				// 上游已提前结束（例如 413/401），停止发送剩余 body。
				break
			}
			continue
		}
		if frame.GetBodyEnd() {
			_ = bodyWriter.Close()
			break
		}
		if frame.GetStart() != nil {
			_ = bodyWriter.CloseWithError(errBodyBeforeEnd)
			cancel()
			return p.sendError(stream, "PLUGIN_BAD_FRAME", errBodyBeforeEnd.Error(), requestSent.Load())
		}
	}

	result := <-resultCh
	if result.err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return p.sendError(stream, classifyDialError(result.err), result.err.Error(), requestSent.Load())
	}
	resp := result.resp
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{
		Start: &pluginv1.ForwardResponseStart{
			StatusCode:    int32(resp.StatusCode),
			Status:        resp.Status,
			Protocol:      resp.Proto,
			ProtocolMajor: int32(resp.ProtoMajor),
			ProtocolMinor: int32(resp.ProtoMinor),
			Headers:       headerToProto(resp.Header),
			ContentLength: resp.ContentLength,
		},
	}}); err != nil {
		return err
	}

	started := time.Now()
	var total int64
	buf := make([]byte, bodyChunkSize)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			// gRPC 的 SendMsg 在返回前完成序列化，不会保留切片引用，
			// 因此直接复用读缓冲，省掉每帧一次分配（高并发流下 GC 差异明显）。
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: buf[:n]}}); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return p.sendError(stream, "PLUGIN_RESPONSE_READ", readErr.Error(), true)
		}
	}

	_ = cfg
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{
		End: &pluginv1.ForwardResponseEnd{
			BytesReceived: total,
			DurationMs:    time.Since(started).Milliseconds(),
		},
	}})
}

func (p *Plugin) sendError(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse], code, message string, requestSent bool) error {
	sendErr := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{
		Error: &pluginv1.ForwardResponseError{Code: code, Message: message, RequestSent: requestSent},
	}})
	if sendErr != nil {
		return sendErr
	}
	return nil
}

// classifyDialError 把底层错误归类成稳定错误码，便于宿主做策略。
func classifyDialError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "PLUGIN_UPSTREAM_TIMEOUT"
	case strings.Contains(msg, "connect"), strings.Contains(msg, "dial"), strings.Contains(msg, "tcp"):
		return "PLUGIN_UPSTREAM_CONNECT"
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return "PLUGIN_UPSTREAM_TLS"
	default:
		return "PLUGIN_UPSTREAM_ERROR"
	}
}

func headerFromProto(in map[string]*pluginv1.HeaderValues) http.Header {
	h := make(http.Header, len(in))
	for k, v := range in {
		if v == nil || len(v.GetValues()) == 0 {
			continue
		}
		h[k] = append([]string(nil), v.GetValues()...)
	}
	return h
}

func headerToProto(in http.Header) map[string]*pluginv1.HeaderValues {
	out := make(map[string]*pluginv1.HeaderValues, len(in))
	for k, v := range in {
		out[k] = &pluginv1.HeaderValues{Values: append([]string(nil), v...)}
	}
	return out
}

var _ = bytes.MinRead
var _ = fmt.Sprintf
