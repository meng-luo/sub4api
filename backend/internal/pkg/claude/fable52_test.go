package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFable52CatalogAndCapabilities(t *testing.T) {
	t.Parallel()

	count := 0
	for _, model := range DefaultModels {
		if model.ID == "claude-fable-5-2" {
			count++
			require.Equal(t, "model", model.Type)
			require.Equal(t, "Claude Fable 5.2", model.DisplayName)
			require.Equal(t, "2026-11-09T00:00:00Z", model.CreatedAt)
		}
	}
	require.Equal(t, 1, count)
	for _, model := range []string{
		"claude-fable-5-2",
		"anthropic/claude-fable-5-2",
		"models/claude-fable-5-2",
		"global.anthropic.claude-fable-5-2",
		"claude-fable-5-2-thinking",
		"claude-fable-5-2-20261109",
		" CLAUDE-FABLE-5-2 ",
	} {
		require.True(t, IsFable52(model), model)
		require.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, EffortLevelsForModel(model), model)
	}
	for _, model := range []string{"claude-fable-5", "claude-fable-5-1", "claude-fable-5-20", "claude-fable-5-2-preview", "claude-mythos-5-2"} {
		require.False(t, IsFable52(model), model)
	}
}

func TestCLICurrentVersionSatisfiesFable52Gate(t *testing.T) {
	require.GreaterOrEqual(t, compareSemver(t, CLICurrentVersion, "2.1.321"), 0)
}
