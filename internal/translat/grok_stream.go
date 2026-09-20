package translat

import (
	"bytes"
	"encoding/json"

	"onegw/internal/types"
)

// responsesStreamState tracks emitted items so part stops fire once and tool
// calls can stream arguments incrementally.
type responsesStreamState struct {
	started   bool
	toolIdx   map[string]int // item_id -> unified part index
	nextTool  int
	itemTypes map[string]string // item_id -> item type (informational)
}

func decodeResponsesStreamEvent(ev sseEvent, st *responsesStreamState) ([]StreamEvent, error) {
	var obj struct {
		Type string `json:"type"`
		Item *struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Input     string `json:"input"`
			Status    string `json:"status"`
		} `json:"item"`
		ItemID      string `json:"item_id"`
		OutputIndex int    `json:"output_index"`
		Delta       string `json:"delta"`
		Text        string `json:"text"`
		Arguments   string `json:"arguments"`
		Response    *struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
			Usage *struct {
				InputTokens        int64 `json:"input_tokens"`
				OutputTokens       int64 `json:"output_tokens"`
				InputTokensDetails *struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails *struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
				// Kimi-style top-level cached subset (issue #33).
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(ev.Data, &obj); err != nil {
		return nil, nil // tolerate non-event SSE noise
	}

	start := func() {
		if !st.started {
			st.started = true
			// The Responses "model" surfaces later (response.created); the
			// encoder falls back to its own model string meanwhile.
		}
	}
	partStart := func(id, itemType string) {
		if st.itemTypes == nil {
			st.itemTypes = map[string]string{}
		}
		st.itemTypes[id] = itemType
	}

	var out []StreamEvent
	switch obj.Type {
	case "response.created":
		start()
		if obj.Response != nil && obj.Response.ID != "" {
			out = append(out, StreamEvent{Kind: EvStart, ID: obj.Response.ID, Model: obj.Response.Model})
		}
	case "response.output_text.delta":
		start()
		out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartText, Text: obj.Delta})
	case "response.reasoning_summary_text.delta":
		start()
		out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartThinking, Thinking: obj.Delta})
	case "response.output_item.added":
		if obj.Item == nil {
			return nil, nil
		}
		partStart(obj.Item.ID, obj.Item.Type)
		switch obj.Item.Type {
		case "function_call":
			idx := st.toolIndexFor(obj.Item.ID)
			start()
			out = append(out, StreamEvent{
				Kind: EvPartStart, Index: idx, PartType: types.PartToolUse,
				ToolID: orDefault(obj.Item.CallID, obj.Item.ID), ToolName: obj.Item.Name,
			})
			if obj.Item.Arguments != "" {
				out = append(out, StreamEvent{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: obj.Item.Arguments})
			}
		case "message", "reasoning":
			// Content arrives via delta events; nothing to emit here.
		}
	case "response.function_call_arguments.delta":
		idx := st.toolIndexFor(obj.ItemID)
		// The wire field is "delta"; some compatible upstreams use
		// "arguments" — accept either.
		args := orDefault(obj.Delta, obj.Arguments)
		if args == "" {
			return nil, nil
		}
		out = append(out, StreamEvent{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: args})
	case "response.output_item.done":
		if obj.Item == nil {
			return nil, nil
		}
		switch obj.Item.Type {
		case "function_call":
			idx := st.toolIndexFor(obj.Item.ID)
			out = append(out, StreamEvent{Kind: EvPartStop, Index: idx, PartType: types.PartToolUse})
		case "message", "reasoning":
			// text.done events carry the full text; skip (already streamed).
		}
	case "response.completed", "response.incomplete", "response.failed":
		var usage *types.Usage
		if obj.Response != nil {
			if u := obj.Response.Usage; u != nil {
				usage = &types.Usage{
					InputTokens:  u.InputTokens,
					OutputTokens: u.OutputTokens,
				}
				if u.InputTokensDetails != nil {
					usage.CacheReadTokens = u.InputTokensDetails.CachedTokens
				}
				if usage.CacheReadTokens == 0 {
					usage.CacheReadTokens = u.CachedTokens // Kimi top level
				}
				if u.OutputTokensDetails != nil {
					usage.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
				}
				if usage.InputTokens == 0 && usage.OutputTokens == 0 {
					usage = nil
				}
			}
		}
		stop := types.StopEndTurn
		if obj.Type == "response.incomplete" {
			stop = types.StopMaxTokens
		}
		if obj.Response != nil && obj.Response.Error != nil && obj.Response.Error.Message != "" {
			e := obj.Response.Error
			return nil, NormalizeInStreamError(&types.APIError{
				Status:  statusFromOAErr(e.Code, "", e.Message),
				Type:    "upstream_error",
				Code:    orDefault(e.Code, ""),
				Message: e.Message,
			})
		}
		if !st.started {
			st.started = true
			out = append(out, StreamEvent{Kind: EvStart})
		}
		out = append(out, StreamEvent{Kind: EvStop, StopReason: stop, Usage: usage})
	// Ignored: response.in_progress, response.output_text.done,
	// response.content_part.added/done, response.reasoning_summary_*.done,
	// response.output_text.annotation.added, ...
	default:
	}
	return out, nil
}

func (st *responsesStreamState) toolIndexFor(id string) int {
	if st.toolIdx == nil {
		st.toolIdx = map[string]int{}
	}
	if idx, ok := st.toolIdx[id]; ok {
		return idx
	}
	idx := st.nextTool
	st.nextTool++
	st.toolIdx[id] = idx
	return idx
}

// EncodeGrokCliRequest renders the unified request as a grok-cli (Grok
// Build) Responses body: stream forced true, store false. Distinct from
// EncodeResponsesRequest (opencode), which forwards the client's stream
// preference.
func EncodeGrokCliRequest(u *types.ChatRequest) ([]byte, error) {
	out, err := EncodeResponsesRequest(u)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	root["stream"] = true
	root["store"] = false
	return json.Marshal(root)
}

// DecodeGrokCliResponse aggregates a grok-cli (Grok Build) Responses SSE
// stream into one unified completion for non-streaming clients.
func DecodeGrokCliResponse(body []byte) (*types.ChatResponse, error) {
	return AggregateStream(bytes.NewReader(body), FmtOpenAIResponses, "")
}
