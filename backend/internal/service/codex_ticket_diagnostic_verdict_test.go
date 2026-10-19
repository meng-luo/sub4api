package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func referenceSampleText(t *testing.T, model string) (string, int) {
	t.Helper()
	raw, err := os.ReadFile("testdata/modeltrace_gpt_reference.json")
	require.NoError(t, err)
	var samples []struct {
		Model         string `json:"model"`
		ExpectedCount int    `json:"expected_count"`
		Text          string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(raw, &samples))
	for _, sample := range samples {
		if sample.Model == model {
			return sample.Text, sample.ExpectedCount
		}
	}
	t.Fatalf("no reference sample for %s", model)
	return "", 0
}

func TestModelTraceDiagnosticModelsAddFingerprintlessCatalogModel(t *testing.T) {
	bankModels, err := ModelTraceGPTModels()
	require.NoError(t, err)
	require.NotContains(t, bankModels, "gpt-6.1-sol", "the bundled bank has no 6.1-sol class")

	diagnostic, err := ModelTraceDiagnosticModels()
	require.NoError(t, err)
	require.Contains(t, diagnostic, "gpt-6.1-sol")
	require.Len(t, diagnostic, len(bankModels)+1, "extra models must not duplicate bank classes")
	for _, model := range bankModels {
		require.Contains(t, diagnostic, model)
	}
	require.Empty(t, duplicateValues(diagnostic))
}

func duplicateValues(values []string) []string {
	seen := make(map[string]bool, len(values))
	var duplicates []string
	for _, value := range values {
		if seen[value] {
			duplicates = append(duplicates, value)
		}
		seen[value] = true
	}
	return duplicates
}

func TestEvaluateCodexDiagnosticVerdicts(t *testing.T) {
	text, expected := referenceSampleText(t, "gpt-6-sol")
	challenge := ModelTraceChallenge{Prompt: "reference", ExpectedCount: expected}

	t.Run("exact class match is normal", func(t *testing.T) {
		verdict := EvaluateCodexDiagnostic("gpt-6-sol", text, challenge)
		require.Equal(t, "normal", verdict.Status)
		require.Empty(t, verdict.Reason)
		require.Equal(t, "gpt-6-sol", verdict.PredictedModel)
		require.Greater(t, verdict.Probability, 0.0)
	})

	t.Run("shared astra fingerprint passes without changing predicted class", func(t *testing.T) {
		astraText, astraExpected := referenceSampleText(t, "gpt-6-astra")
		verdict := EvaluateCodexDiagnostic("gpt-6.1-sol", astraText, ModelTraceChallenge{ExpectedCount: astraExpected})
		require.Equal(t, "normal", verdict.Status)
		require.Equal(t, "shared_fingerprint_match", verdict.Reason)
		require.Equal(t, "gpt-6-astra", verdict.PredictedModel)
		require.Greater(t, verdict.Probability, 0.0)
	})

	t.Run("previous sol reference is outside the shared group", func(t *testing.T) {
		verdict := EvaluateCodexDiagnostic("gpt-6.1-sol", text, challenge)
		require.Equal(t, "degraded", verdict.Status)
		require.Equal(t, "fingerprint_mismatch", verdict.Reason)
		require.Equal(t, "gpt-6-sol", verdict.PredictedModel)
	})

	t.Run("any other class is degraded", func(t *testing.T) {
		verdict := EvaluateCodexDiagnostic("gpt-5.6-luna", text, challenge)
		require.Equal(t, "degraded", verdict.Status)
		require.Equal(t, "fingerprint_mismatch", verdict.Reason)
		require.Equal(t, "gpt-6-sol", verdict.PredictedModel)
	})

	t.Run("truncated output fails instead of degrading", func(t *testing.T) {
		verdict := EvaluateCodexDiagnostic("gpt-6-sol", "1 2 3", challenge)
		require.Equal(t, "failed", verdict.Status)
		require.Equal(t, "insufficient_numbers", verdict.Reason)
		require.Equal(t, 3, verdict.ParsedCount)
		require.Empty(t, verdict.PredictedModel)
	})
}

func TestModelTraceSharedFingerprintPrefersIndependentClassAfterRefresh(t *testing.T) {
	bank, err := parseModelTraceBank(embeddedModelTraceBank)
	require.NoError(t, err)
	for index, model := range bank.Models {
		if model.ID == "gpt-6-sol" {
			bank.Models[index].ID = "gpt-6.1-sol"
			bank.Robust.ModelOrder[index] = "gpt-6.1-sol"
		}
	}
	previous := modelTraceActive.Load()
	t.Cleanup(func() { modelTraceActive.Store(previous) })
	modelTraceActive.Store(&modelTraceVersion{bank: bank, commit: "independent-class-test"})

	text, expected := referenceSampleText(t, "gpt-6-astra")
	verdict := EvaluateCodexDiagnostic("gpt-6.1-sol", text, ModelTraceChallenge{ExpectedCount: expected})
	require.Equal(t, "degraded", verdict.Status, "an independent class must disable the shared fallback")
	require.Equal(t, "gpt-6-astra", verdict.PredictedModel)

	text, expected = referenceSampleText(t, "gpt-6-sol")
	verdict = EvaluateCodexDiagnostic("gpt-6.1-sol", text, ModelTraceChallenge{ExpectedCount: expected})
	require.Equal(t, "normal", verdict.Status)
	require.Empty(t, verdict.Reason)
	require.Equal(t, "gpt-6.1-sol", verdict.PredictedModel)

	for _, list := range []func() ([]string, error){ModelTraceDiagnosticModels, ModelTraceTicketModels} {
		models, err := list()
		require.NoError(t, err)
		require.Contains(t, models, "gpt-6.1-sol")
		require.Empty(t, duplicateValues(models))
	}
}
