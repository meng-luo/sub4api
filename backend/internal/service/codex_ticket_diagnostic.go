package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type codexTicketDiagnosticTargetKey struct{}

func WithCodexTicketDiagnostic(ctx context.Context, accountID int64) context.Context {
	return context.WithValue(ctx, codexTicketDiagnosticTargetKey{}, accountID)
}

func isCodexTicketDiagnostic(ctx context.Context) bool {
	accountID, _ := ctx.Value(codexTicketDiagnosticTargetKey{}).(int64)
	return accountID > 0
}

// CodexNativeDiagnostic 是一次"号原生"降智探测的结果：请求由目标账号自身的
// 上游凭据发出，不经过公开网关，也不消耗任何用户的 API Key。
type CodexNativeDiagnostic struct {
	Output     string
	HTTPStatus int
	// Reason 非空表示本次探测没有拿到可评分的输出。
	Reason string
}

// RunCodexNativeDiagnostic 用账号自己的凭据打一发 ModelTrace 探测。
// challenge 必须由调用方先生成，保证请求正文与最终判定用同一次抽签。
func (s *OpenAIGatewayService) RunCodexNativeDiagnostic(ctx context.Context, accountID int64, model string, challenge ModelTraceChallenge, timeout time.Duration) CodexNativeDiagnostic {
	var result CodexNativeDiagnostic
	if s == nil || s.accountRepo == nil {
		result.Reason = "unavailable"
		return result
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		result.Reason = "identity_resolution_failed"
		return result
	}
	ctx = WithCodexTicketDiagnostic(ctx, accountID)
	probeCtx, err := s.PrepareCodexProbeContext(ctx)
	if err != nil {
		result.Reason = "template_unavailable"
		if errors.Is(err, ErrCodexProbeTemplateInvalid) {
			result.Reason = "template_invalid"
		}
		return result
	}
	if s.httpUpstream == nil {
		result.Reason = "upstream_unavailable"
		return result
	}
	token, _, tokenErr := s.GetAccessToken(probeCtx, account)
	if tokenErr != nil || strings.TrimSpace(token) == "" {
		result.Reason = "credentials"
		return result
	}
	// Follow the configured ticket egress source; an empty pool falls back to direct.
	proxyURL := ""
	if selected, _, proxyErr := s.chooseCodexTicketProxy(probeCtx, account, model); proxyErr == nil {
		proxyURL = selected
	} else if !errors.Is(proxyErr, ErrCodexTicketNoProxy) {
		result.Reason = "proxy_unavailable"
		return result
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	output, _, _, status, probeErr := s.fireOpenAICodexTicketProbe(probeCtx, account, token, model, proxyURL, challenge, timeout)
	result.HTTPStatus = status
	if probeErr != nil {
		result.Reason = "request"
		if errors.Is(probeErr, ErrCodexProbeTemplateInvalid) {
			result.Reason = "template_invalid"
		}
		if errors.Is(probeErr, ErrCodexProbeIdentity) {
			result.Reason = "identity_resolution_failed"
		}
		if errors.Is(probeErr, context.DeadlineExceeded) {
			result.Reason = "timeout"
		}
		return result
	}
	if status != http.StatusOK {
		result.Reason = "http_status"
		return result
	}
	if strings.TrimSpace(output) == "" {
		result.Reason = "response_incomplete"
		return result
	}
	result.Output = output
	return result
}

// CodexDiagnosticVerdict 是一次降智判定。
// Status: normal(正常) / degraded(疑似降智) / uncertain(不确定) / failed(请求失败)。
type CodexDiagnosticVerdict struct {
	Status         string
	Reason         string
	PredictedModel string
	Probability    float64
	ParsedCount    int
}

// EvaluateCodexDiagnostic 把探测输出与请求模型比对。
//
// Models without an independent class can pass a configured shared fingerprint.
// Preserve the predicted class so a group match does not imply exact identity.
func EvaluateCodexDiagnostic(model, output string, challenge ModelTraceChallenge) CodexDiagnosticVerdict {
	prediction, snapshot, err := modelTracePredictSnapshot(output, challenge.ExpectedCount)
	verdict := CodexDiagnosticVerdict{Status: "failed", Reason: "insufficient_numbers", ParsedCount: prediction.ParsedCount}
	if err != nil {
		return verdict
	}
	verdict.PredictedModel = prediction.Model
	verdict.Probability = prediction.Probability
	if snapshot.bank.fingerprintMatches(model, prediction.Model) {
		verdict.Status = "normal"
		verdict.Reason = ""
		if prediction.Model != model {
			verdict.Reason = "shared_fingerprint_match"
		}
		return verdict
	}
	verdict.Status = "degraded"
	verdict.Reason = "fingerprint_mismatch"
	return verdict
}
