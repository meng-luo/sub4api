package admin

import (
	"context"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

// codexDiagnosticProbe 是降智检测需要的最小探测能力：号原生、无用户 Key。
type codexDiagnosticProbe interface {
	RunCodexNativeDiagnostic(ctx context.Context, accountID int64, model string, challenge service.ModelTraceChallenge, timeout time.Duration) service.CodexNativeDiagnostic
}

type codexDiagnosticItem struct {
	Model             string  `json:"model"`
	Status            string  `json:"status"`
	Reason            string  `json:"reason,omitempty"`
	PredictedModel    string  `json:"predicted_model,omitempty"`
	Probability       float64 `json:"probability,omitempty"`
	ParsedCount       int     `json:"parsed_number_count,omitempty"`
	HTTPStatus        int     `json:"http_status,omitempty"`
	FingerprintCommit string  `json:"fingerprint_commit,omitempty"`
}

func (h *AccountHandler) DiagnoseCodexModels(c *gin.Context) {
	h.diagnoseCodexModels(c, service.NewModelTraceChallenge)
}

// diagnoseCodexModels 用目标账号自身的上游凭据逐个模型打探测，不再要求用户
// 选择计费 Key，也不再回环进公开网关。
func (h *AccountHandler) diagnoseCodexModels(c *gin.Context, generateChallenge func() (service.ModelTraceChallenge, error)) {
	accountID, ok := codexTicketAccountID(c)
	if !ok {
		return
	}
	if h.codexDiagnostic == nil {
		codexTicketError(c, service.ErrCodexTicketUnavailable)
		return
	}
	var input struct {
		Models []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || len(input.Models) == 0 || len(input.Models) > 32 {
		response.BadRequest(c, "Select up to 32 models")
		return
	}
	allowed, err := service.ModelTraceDiagnosticModels()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	modelSet := make(map[string]bool, len(allowed))
	for _, model := range allowed {
		modelSet[model] = true
	}
	seen := make(map[string]bool, len(input.Models))
	for _, model := range input.Models {
		if !modelSet[model] || seen[model] {
			response.BadRequest(c, "Invalid or duplicate GPT model")
			return
		}
		seen[model] = true
	}
	results := make([]codexDiagnosticItem, 0, len(input.Models))
	for _, model := range input.Models {
		if c.Request.Context().Err() != nil {
			break
		}
		item := codexDiagnosticItem{Model: model, Status: "failed"}
		challenge, challengeErr := generateChallenge()
		if challengeErr != nil {
			item.Reason = "challenge_generation_failed"
			results = append(results, item)
			continue
		}
		probe := h.codexDiagnostic.RunCodexNativeDiagnostic(c.Request.Context(), accountID, model, challenge, 120*time.Second)
		item.HTTPStatus = probe.HTTPStatus
		if probe.Reason != "" {
			item.Reason = probe.Reason
			results = append(results, item)
			continue
		}
		verdict := service.EvaluateCodexDiagnostic(model, probe.Output, challenge)
		item.Status = verdict.Status
		item.Reason = verdict.Reason
		item.PredictedModel = verdict.PredictedModel
		item.Probability = verdict.Probability
		item.ParsedCount = verdict.ParsedCount
		item.FingerprintCommit = verdict.FingerprintCommit
		results = append(results, item)
	}
	response.Success(c, gin.H{"items": results, "canceled": c.Request.Context().Err() != nil})
}
