package service

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/tidwall/gjson"
)

const openAIWSHTTPHandoffMaxItems = 16384

// A handoff is raised before admission or forwarding of the new turn. Completed
// turns have already been settled and must never be replayed as new requests.
type openAIWSHTTPHandoff struct {
	payload []byte
	turn    int
}

func (*openAIWSHTTPHandoff) Error() string { return "switch oversized websocket turn to HTTP" }

// History belongs to one client connection, never a global/session-key cache.
// Bodies are immutable; the byte limit also counts the backing request buffers
// retained by zero-copy input items.
type openAIWSHTTPHandoffHistory struct {
	mu            sync.Mutex
	limit         int64
	input         []json.RawMessage
	responseID    string
	retainedBytes int64
	complete      bool
	sessionEvents bool
	pending       []byte
	output        openAIWSToolCallReplayCollector
	outputBytes   int64
}

func newOpenAIWSHTTPHandoffHistory(cfg *config.Config) *openAIWSHTTPHandoffHistory {
	limit := min(ResolveOpenAIWSClientReadLimitBytes(cfg), openAIWSClientReadLimitBytesDefault)
	return &openAIWSHTTPHandoffHistory{limit: limit}
}

func (h *openAIWSHTTPHandoffHistory) inputForTurn(payload []byte) ([]json.RawMessage, bool, int64) {
	if int64(len(payload)) > h.limit || h.sessionEvents {
		return nil, false, 0
	}
	items, exists, err := openAIWSExtractNormalizedInputSequence(payload)
	if err != nil || !exists || len(items) > openAIWSHTTPHandoffMaxItems {
		return nil, false, 0
	}
	for _, item := range items {
		if gjson.GetBytes(item, "type").String() == "item_reference" {
			return nil, false, 0
		}
	}
	previousID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	coverage := AnalyzeToolCallOutputContextCoverageBytes(payload)
	needsHistory := previousID != "" || (coverage.HasFunctionCallOutput && !coverage.ContextCoversAllCallIDs)
	retained := int64(len(payload))
	if needsHistory {
		if !h.complete || (previousID != "" && previousID != h.responseID) {
			return nil, false, 0
		}
		merged, _ := buildOpenAIWSReplayInputSequenceFromItems(h.input, true, items, true, true)
		if !openAIWSRawItemsHasPrefix(items, merged) {
			retained += h.retainedBytes
		}
		items = merged
	}
	if retained > h.limit || len(items) > openAIWSHTTPHandoffMaxItems ||
		(openAIWSRawItemsHasFunctionCallOutput(items) && !openAIWSRawItemsHaveToolCallContextForOutputs(items)) {
		return nil, false, 0
	}
	return items, true, retained
}

func (h *openAIWSHTTPHandoffHistory) prepare(payload []byte) ([]byte, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if generate := gjson.GetBytes(payload, "generate"); generate.Type == gjson.False {
		return nil, false, nil
	}
	items, safe, _ := h.inputForTurn(payload)
	if !safe {
		return nil, false, nil
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) == "" {
		coverage := AnalyzeToolCallOutputContextCoverageBytes(payload)
		if !coverage.HasFunctionCallOutput || coverage.ContextCoversAllCallIDs {
			return payload, true, nil
		}
	}
	return buildOpenAIWSCurrentTurnRetryPayload(payload, items, true, "")
}

func (h *openAIWSHTTPHandoffHistory) recordLocked(payload []byte, output []json.RawMessage, responseID string, success bool) {
	items, safe, retained := h.inputForTurn(payload)
	for _, item := range output {
		retained += int64(len(item))
	}
	h.complete = success && safe && responseID != "" && retained <= h.limit && len(items)+len(output) <= openAIWSHTTPHandoffMaxItems
	h.input = nil
	h.retainedBytes = 0
	h.responseID = responseID
	if h.complete {
		h.input = combineOpenAIWSReplayItems(items, output)
		h.retainedBytes = retained
	}
}

func (h *openAIWSHTTPHandoffHistory) record(payload []byte, result *OpenAIForwardResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if result == nil {
		h.complete = false
		h.input = nil
		h.retainedBytes = 0
		return
	}
	h.recordLocked(payload, result.wsAccountFailoverReplayInput, result.RequestID, result.SucceededForScheduling())
}

func (h *openAIWSHTTPHandoffHistory) begin(payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending = nil
	if int64(len(payload)) <= h.limit {
		h.pending = payload
	}
	h.output = openAIWSToolCallReplayCollector{}
	h.outputBytes = 0
}

func (h *openAIWSHTTPHandoffHistory) observe(payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		return
	}
	eventType := gjson.GetBytes(payload, "type").String()
	switch eventType {
	case "response.output_item.done", "response.completed", "response.done":
		h.outputBytes += int64(len(payload))
		if h.outputBytes > h.limit || len(h.output.allItems) >= openAIWSHTTPHandoffMaxItems ||
			gjson.GetBytes(payload, "response.output.#").Int() > openAIWSHTTPHandoffMaxItems {
			h.pending = nil
			h.output = openAIWSToolCallReplayCollector{}
			h.complete = false
			h.input = nil
			h.retainedBytes = 0
			return
		}
		h.output.AddEvent(eventType, payload)
	}
	if isOpenAIWSTerminalEvent(eventType) {
		success := eventType == "response.completed" || eventType == "response.done"
		h.recordLocked(h.pending, h.output.AllItems(), gjson.GetBytes(payload, "response.id").String(), success)
		h.pending = nil
		h.output = openAIWSToolCallReplayCollector{}
	}
}

func (h *openAIWSHTTPHandoffHistory) disableForSessionEvent() {
	h.mu.Lock()
	defer h.mu.Unlock()
	// session.update/conversation events may hold context outside response.create.
	// An HTTP request cannot reproduce that state safely.
	h.sessionEvents = true
	h.complete = false
	h.input = nil
	h.pending = nil
	h.output = openAIWSToolCallReplayCollector{}
	h.retainedBytes = 0
}

func (h *openAIWSHTTPHandoffHistory) stripInvalidEncryptedContent(invalid map[string]struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.input, _ = stripOpenAIInvalidEncryptedContentFromReplayItems(h.input, invalid)
}

func (s *OpenAIGatewayService) openAIWSHTTPHandoffSizeReached(payloadBytes int) bool {
	return s.openAIWSHTTPBridgeEnabled() && int64(payloadBytes) >= s.openAIWSHTTPBridgeThresholdBytes()
}
