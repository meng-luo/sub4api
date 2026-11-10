package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFable52AdaptiveThinking(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max", "none", "minimal", "invalid"} {
		t.Run("effort="+effort, func(t *testing.T) {
			for _, chat := range []bool{false, true} {
				req := &ResponsesRequest{Model: "claude-fable-5-2", Input: json.RawMessage(`"hello"`)}
				if effort != "" {
					req.Reasoning = &ResponsesReasoning{Effort: effort}
				}
				if chat {
					var err error
					req, err = ChatCompletionsToResponses(&ChatCompletionsRequest{
						Model: "claude-fable-5-2", ReasoningEffort: effort,
						Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}})
					require.NoError(t, err)
				}
				out, err := ResponsesToAnthropicRequest(req)
				if effort == "none" || effort == "minimal" || effort == "invalid" {
					require.ErrorContains(t, err, "reasoning effort")
					continue
				}
				require.NoError(t, err)
				require.NotNil(t, out.Thinking)
				require.Equal(t, "adaptive", out.Thinking.Type)
				require.Zero(t, out.Thinking.BudgetTokens)
				want := effort
				if want == "" {
					want = "high"
				}
				require.Equal(t, want, out.OutputConfig.Effort)
			}
		})
	}
}

func TestFable52ToolChoiceAndSampling(t *testing.T) {
	for _, choice := range []string{`"required"`, `{"type":"function","name":"lookup"}`, `"auto"`, `"none"`} {
		_, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-fable-5-2", Input: json.RawMessage(`"hello"`), ToolChoice: json.RawMessage(choice)})
		if choice == `"auto"` || choice == `"none"` {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "forced tool_choice")
		}
	}
	for _, tc := range []struct {
		temperature *float64
		topP        *float64
		wantErr     string
	}{
		{temperature: fable52Float(1)},
		{topP: fable52Float(0.99)},
		{temperature: fable52Float(0.5), wantErr: "temperature"},
		{topP: fable52Float(1), wantErr: "top_p"},
		{temperature: fable52Float(1), topP: fable52Float(0.99), wantErr: "together"},
	} {
		_, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-fable-5-2", Input: json.RawMessage(`"hello"`), Temperature: tc.temperature, TopP: tc.topP})
		if tc.wantErr == "" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, tc.wantErr)
		}
	}
}

func fable52Float(value float64) *float64 { return &value }

func TestFable52SignedThinkingRoundTrip(t *testing.T) {
	for _, block := range []AnthropicContentBlock{
		{Type: "thinking", Thinking: "", Signature: "upstream-signed-block"},
		{Type: "redacted_thinking", Data: "upstream-redacted-block"},
	} {
		t.Run(block.Type, func(t *testing.T) {
			response := AnthropicToResponsesResponse(&AnthropicResponse{Model: "claude-fable-5-2", Content: []AnthropicContentBlock{
				block, {Type: "text", Text: "progress"}, {Type: "tool_use", ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)},
			}})
			require.Len(t, response.Output, 3)
			raw, err := json.Marshal(response.Output)
			require.NoError(t, err)
			var items []ResponsesInputItem
			require.NoError(t, json.Unmarshal(raw, &items))
			items = append(items, ResponsesInputItem{Type: "function_call_output", CallID: response.Output[2].CallID, Output: "ok"})
			raw, err = json.Marshal(items)
			require.NoError(t, err)
			converted, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-fable-5-2", Input: raw})
			require.NoError(t, err)
			require.Len(t, converted.Messages, 2)
			var blocks []AnthropicContentBlock
			require.NoError(t, json.Unmarshal(converted.Messages[0].Content, &blocks))
			require.Len(t, blocks, 3)
			require.Equal(t, block, blocks[0])
			require.Equal(t, "text", blocks[1].Type)
			require.Equal(t, "tool_use", blocks[2].Type)
		})
	}
}

func TestFable52StreamPreservesThinking(t *testing.T) {
	state := NewAnthropicEventToResponsesState()
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_start", Message: &AnthropicResponse{Model: "claude-fable-5-2"}}, state)
	require.True(t, state.PreserveThinkingSignatures)
	index := 0
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_start", Index: &index, ContentBlock: &AnthropicContentBlock{Type: "thinking"}}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_delta", Index: &index, Delta: &AnthropicDelta{Type: "signature_delta", Signature: "signed-stream"}}, state)
	events := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_stop", Index: &index}, state)
	found := false
	for _, event := range events {
		if event.Type == "response.output_item.done" && event.Item != nil {
			require.Contains(t, event.Item.EncryptedContent, anthropicThinkingEnvelopePrefix)
			found = true
		}
	}
	require.True(t, found)
}
