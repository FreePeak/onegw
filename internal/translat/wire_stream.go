package translat

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"onegw/internal/types"
)

// EvEnd marks clean end-of-stream; encoders write their terminator there.
const EvEnd = "end"

// ---------------------------------------------------------------------------
// Stream decoding: wire -> StreamEvent
// ---------------------------------------------------------------------------

func decodeStreamEvent(f Format, ev sseEvent) ([]StreamEvent, error) {
	switch f {
	case FmtAnthropic:
		return decodeAnthropicStreamEvent(ev)
	case FmtGemini:
		return decodeGeminiStreamEvent(ev)
	default:
		return decodeOpenAIStreamEvent(ev)
	}
}

// -- OpenAI ------------------------------------------------------------------

type oaChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			// Vendor alias (commandcode /provider/v1 and other AI-SDK
			// resellers emit reasoning, not reasoning_content — live
			// 2026-09-10). Without it the buffered pipeline (saver on → no
			// stream passthrough) silently drops their thinking.
			Reasoning string       `json:"reasoning"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *oaError `json:"error"`
}

// commandCodeDecoder decodes commandcode streams: events carry no part
// numbering, so per-stream state assigns unified part indices.
type commandCodeDecoder struct {
	from Format
	cc   ccStreamState
}

func newCommandCodeDecoder(f Format) *commandCodeDecoder {
	return &commandCodeDecoder{from: f}
}

func (d *commandCodeDecoder) decode(ev sseEvent) ([]StreamEvent, error) {
	switch d.from {
	case FmtCommandCode:
		return decodeCommandCodeStreamEvent(ev, &d.cc)
	case FmtCursor:
		return nil, fmt.Errorf("cursor wire format is a skeleton; executor not implemented")
	default:
		return decodeStreamEvent(d.from, ev)
	}
}

// errDecoder fails every event: used for formats with no decoder
// registered (skeleton kinds, unknown formats). Never nil.
type errDecoder struct{ err error }

func (d errDecoder) decode(ev sseEvent) ([]StreamEvent, error) { return nil, d.err }

func decodeOpenAIStreamEvent(ev sseEvent) ([]StreamEvent, error) {
	data := ev.Data
	if strings.TrimSpace(string(data)) == "[DONE]" || len(data) == 0 {
		return nil, nil
	}
	var c oaChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("openai chunk: %w", err)
	}
	if c.Error != nil {
		return []StreamEvent{{
			Kind: EvError,
			Err: NormalizeInStreamError(&types.APIError{Status: statusFromOAErr(c.Error.Code, c.Error.Type, c.Error.Message),
				Type: orDefault(c.Error.Type, "upstream_error"), Code: errCodeString(c.Error.Code), Message: c.Error.Message}),
		}}, nil
	}
	var out []StreamEvent
	if c.ID != "" || c.Model != "" {
		out = append(out, StreamEvent{Kind: EvStart, ID: c.ID, Model: c.Model})
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if txt := flattenOAContent(d.Content); txt != "" {
			out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartText, Text: txt})
		}
		if r := orDefault(strings.TrimSpace(d.ReasoningContent), strings.TrimSpace(d.Reasoning)); r != "" {
			out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartThinking, Thinking: r})
		}
		for _, tc := range d.ToolCalls {
			if tc.ID != "" || tc.Function.Name != "" {
				out = append(out, StreamEvent{
					Kind:     EvPartStart,
					Index:    tc.Index,
					PartType: types.PartToolUse,
					ToolID:   tc.ID,
					ToolName: tc.Function.Name,
				})
			}
			if tc.Function.Arguments != "" {
				out = append(out, StreamEvent{
					Kind:     EvDelta,
					Index:    tc.Index,
					PartType: types.PartToolUse,
					ToolArgs: tc.Function.Arguments,
				})
			}
		}
		if ch.FinishReason != "" {
			out = append(out, StreamEvent{Kind: EvStop, StopReason: mapOAStop(ch.FinishReason)})
		}
	}
	if c.Usage != nil {
		u := oaUsageToUnified(c.Usage)
		out = append(out, StreamEvent{Kind: EvStop, Usage: &u})
	}
	return out, nil
}

// -- Anthropic ---------------------------------------------------------------

type anStreamMsg struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	// message_start
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens             int64 `json:"input_tokens"`
			CacheCreationInputToken int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens    int64 `json:"cache_read_input_tokens"`
			OutputTokens            int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`

	// content_block_start
	ContentBlock *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	} `json:"content_block"`

	// content_block_delta
	// content_block_delta / message_delta share the "delta" key; decoded per
	// event type.
	DeltaRaw json.RawMessage `json:"delta,omitempty"`
	Usage    *struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAnthropicStreamEvent(ev sseEvent) ([]StreamEvent, error) {
	if len(ev.Data) == 0 {
		return nil, nil
	}
	var m anStreamMsg
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		return nil, fmt.Errorf("anthropic event: %w", err)
	}
	switch m.Type {
	case "message_start":
		out := StreamEvent{Kind: EvStart}
		if m.Message != nil {
			out.ID = m.Message.ID
			out.Model = m.Message.Model
			if m.Message.Usage != nil {
				u := m.Message.Usage
				// Normalize: fold cache read/write into InputTokens
				// (unified InputTokens is cache-inclusive; Anthropic's
				// input_tokens is exclusive).
				out.Usage = &types.Usage{
					InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputToken,
					CacheReadTokens:  u.CacheReadInputTokens,
					CacheWriteTokens: u.CacheCreationInputToken,
					OutputTokens:     u.OutputTokens,
					UpstreamFormat:   string(FmtAnthropic),
				}
			}
		}
		return []StreamEvent{out}, nil
	case "content_block_start":
		e := StreamEvent{Kind: EvPartStart, Index: m.Index}
		if m.ContentBlock != nil {
			switch m.ContentBlock.Type {
			case "tool_use":
				e.PartType = types.PartToolUse
				e.ToolID = m.ContentBlock.ID
				e.ToolName = m.ContentBlock.Name
			case "thinking", "redacted_thinking":
				e.PartType = types.PartThinking
				if m.ContentBlock.Thinking != "" {
					e.Thinking = m.ContentBlock.Thinking
				}
				if m.ContentBlock.Signature != "" {
					e.Signature = m.ContentBlock.Signature
				}
			default:
				e.PartType = types.PartText
				if m.ContentBlock.Thinking != "" {
					e.Thinking = m.ContentBlock.Thinking
				}
			}
		}
		return []StreamEvent{e}, nil
	case "content_block_delta":
		e := StreamEvent{Kind: EvDelta, Index: m.Index}
		if len(m.DeltaRaw) == 0 {
			return nil, nil
		}
		var d struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
		}
		if json.Unmarshal(m.DeltaRaw, &d) != nil {
			return nil, nil
		}
		switch d.Type {
		case "input_json_delta":
			e.PartType = types.PartToolUse
			e.ToolArgs = d.PartialJSON
		case "thinking_delta":
			e.PartType = types.PartThinking
			e.Thinking = d.Thinking
		case "signature_delta":
			e.PartType = types.PartThinking
			e.Signature = d.Signature
		default: // text_delta and friends
			e.PartType = types.PartText
			e.Text = d.Text
		}
		return []StreamEvent{e}, nil
	case "content_block_stop":
		return []StreamEvent{{Kind: EvPartStop, Index: m.Index}}, nil
	case "message_delta":
		e := StreamEvent{Kind: EvStop}
		if len(m.DeltaRaw) > 0 {
			var d struct {
				StopReason   string  `json:"stop_reason"`
				StopSequence *string `json:"stop_sequence"`
			}
			if json.Unmarshal(m.DeltaRaw, &d) == nil {
				e.StopReason = mapAnthropicStop(d.StopReason)
				if d.StopSequence != nil {
					e.StopSeq = *d.StopSequence
				}
			}
		}
		if m.Usage != nil {
			e.Usage = &types.Usage{OutputTokens: m.Usage.OutputTokens, UpstreamFormat: string(FmtAnthropic)}
		}
		return []StreamEvent{e}, nil
	case "ping":
		return []StreamEvent{{Kind: EvPing}}, nil
	case "error":
		if m.Error != nil {
			return []StreamEvent{{
				Kind: EvError,
				Err:  &types.APIError{Status: 502, Type: m.Error.Type, Message: m.Error.Message},
			}}, nil
		}
		return nil, nil
	default: // message_stop, unknown: no-op
		return nil, nil
	}
}

func mapAnthropicStop(s string) string {
	switch s {
	case "end_turn", "":
		return types.StopEndTurn
	case "max_tokens":
		return types.StopMaxTokens
	case "stop_sequence":
		return types.StopStopSequence
	case "tool_use":
		return types.StopToolUse
	case "refusal":
		return types.StopContentFilter
	default:
		return types.StopEndTurn
	}
}

func unmapAnthropicStop(s string) string {
	switch s {
	case types.StopMaxTokens:
		return "max_tokens"
	case types.StopStopSequence:
		return "stop_sequence"
	case types.StopToolUse:
		return "tool_use"
	case types.StopContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// -- Gemini ------------------------------------------------------------------

type gemChunk struct {
	Candidates []struct {
		Content *struct {
			Role  string    `json:"role"`
			Parts []gemPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	UsageMetadata *gemUsage `json:"usageMetadata"`
	ModelVersion  string    `json:"modelVersion"`
	ErrorResponse *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func decodeGeminiStreamEvent(ev sseEvent) ([]StreamEvent, error) {
	if len(ev.Data) == 0 {
		return nil, nil
	}
	var c gemChunk
	if err := json.Unmarshal(ev.Data, &c); err != nil {
		return nil, fmt.Errorf("gemini chunk: %w", err)
	}
	if c.ErrorResponse != nil {
		return []StreamEvent{{
			Kind: EvError,
			Err:  &types.APIError{Status: c.ErrorResponse.Code, Type: c.ErrorResponse.Status, Message: c.ErrorResponse.Message},
		}}, nil
	}
	var out []StreamEvent
	if c.ModelVersion != "" {
		out = append(out, StreamEvent{Kind: EvStart, Model: c.ModelVersion})
	}
	for _, cand := range c.Candidates {
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				switch {
				case p.FunctionCall != nil:
					out = append(out, StreamEvent{
						Kind:     EvPartStart,
						Index:    cand.Index,
						PartType: types.PartToolUse,
						ToolName: p.FunctionCall.Name,
					})
					out = append(out, StreamEvent{
						Kind:     EvDelta,
						Index:    cand.Index,
						PartType: types.PartToolUse,
						ToolArgs: string(gemArgsJSON(p.FunctionCall.Args)),
					})
					out = append(out, StreamEvent{Kind: EvPartStop, Index: cand.Index})
				case p.Thought:
					out = append(out, StreamEvent{
						Kind: EvDelta, Index: cand.Index,
						PartType: types.PartThinking, Thinking: p.Text,
					})
				default:
					out = append(out, StreamEvent{
						Kind: EvDelta, Index: cand.Index,
						PartType: types.PartText, Text: p.Text,
					})
				}
			}
		}
		if cand.FinishReason != "" {
			out = append(out, StreamEvent{Kind: EvStop, StopReason: mapGeminiStop(cand.FinishReason)})
		}
	}
	if c.UsageMetadata != nil {
		u := c.UsageMetadata
		out = append(out, StreamEvent{Kind: EvStop, Usage: &types.Usage{
			InputTokens:     u.PromptTokenCount,
			OutputTokens:    u.CandidatesTokenCount,
			CacheReadTokens: u.CachedContentTokenCount,
			ReasoningTokens: u.ThoughtsTokenCount,
			UpstreamFormat:  string(FmtGemini),
		}})
	}
	return out, nil
}

func gemArgsJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func mapGeminiStop(s string) string {
	switch s {
	case "STOP":
		return types.StopEndTurn
	case "MAX_TOKENS":
		return types.StopMaxTokens
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
		return types.StopContentFilter
	case "MALFORMED_FUNCTION_CALL":
		return types.StopContentFilter
	default:
		return types.StopEndTurn
	}
}

func unmapGeminiStop(s string) string {
	switch s {
	case types.StopMaxTokens:
		return "MAX_TOKENS"
	case types.StopContentFilter:
		return "SAFETY"
	default:
		return "STOP"
	}
}

// ---------------------------------------------------------------------------
// Stream encoding: StreamEvent -> wire
// ---------------------------------------------------------------------------

func newStreamEncoder(f Format, model string) streamEncoder {
	switch f {
	case FmtAnthropic:
		return &anthropicEncoder{model: model}
	case FmtGemini:
		return &geminiEncoder{model: model}
	default:
		return &openaiEncoder{model: model, id: "chatcmpl-onegw-" + randHex(8)}
	}
}

type streamEncoder interface {
	encode(w io.Writer, ev StreamEvent) error
	finish(w io.Writer) error
}

// -- OpenAI encoder -----------------------------------------------------------

type openaiEncoder struct {
	model       string
	id          string
	created     int64
	started     bool
	toolIdx     map[int]int // unified part index -> openai tool_calls index
	nextToolIdx int
	usage       types.Usage
	finished    bool
}

func (e *openaiEncoder) chunk(w io.Writer, delta any, finish string, usage *oaUsage) error {
	c := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		c["usage"] = usage
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func (e *openaiEncoder) encode(w io.Writer, ev StreamEvent) error {
	switch ev.Kind {
	case EvStart:
		if e.created == 0 {
			e.created = nowUnix()
		}
		if ev.ID != "" {
			e.id = ev.ID
		}
		if ev.Model != "" {
			e.model = ev.Model
		}
		if ev.Usage != nil {
			e.usage.Merge(*ev.Usage)
		}
		if !e.started {
			e.started = true
			return e.chunk(w, map[string]any{"role": "assistant", "content": ""}, "", nil)
		}
	case EvPartStart:
		idx := e.nextToolIdx
		e.nextToolIdx++
		if e.toolIdx == nil {
			e.toolIdx = map[int]int{}
		}
		e.toolIdx[ev.Index] = idx
		return e.chunk(w, map[string]any{
			"tool_calls": []map[string]any{{
				"index":    idx,
				"id":       orDefault(ev.ToolID, "call_"+ev.ToolName),
				"type":     "function",
				"function": map[string]any{"name": ev.ToolName, "arguments": ""},
			}},
		}, "", nil)
	case EvDelta:
		switch ev.PartType {
		case types.PartThinking:
			return e.chunk(w, map[string]any{"reasoning_content": ev.Thinking}, "", nil)
		case types.PartToolUse:
			idx, ok := e.toolIdx[ev.Index]
			if !ok {
				idx = e.nextToolIdx
				e.nextToolIdx++
				if e.toolIdx == nil {
					e.toolIdx = map[int]int{}
				}
				e.toolIdx[ev.Index] = idx
			}
			return e.chunk(w, map[string]any{
				"tool_calls": []map[string]any{{
					"index":    idx,
					"function": map[string]any{"arguments": ev.ToolArgs},
				}},
			}, "", nil)
		default:
			return e.chunk(w, map[string]any{"content": ev.Text}, "", nil)
		}
	case EvPartStop:
		return nil // openai has no block-stop concept
	case EvStop:
		if ev.Usage != nil {
			e.usage.Merge(*ev.Usage)
		}
		if ev.StopReason != "" && !e.finished {
			e.finished = true
			return e.chunk(w, map[string]any{}, unmapOAStop(ev.StopReason), nil)
		}
	case EvPing, EvError:
		return nil // errors surface via finish(); stream already failed upstream
	case EvEnd:
		return nil
	}
	return nil
}

func (e *openaiEncoder) finish(w io.Writer) error {
	if e.created == 0 {
		e.created = nowUnix()
	}
	if !e.started {
		// Degenerate stream (no content): still emit a valid minimal stream.
		if err := e.chunk(w, map[string]any{"role": "assistant", "content": ""}, "", nil); err != nil {
			return err
		}
	}
	if e.usage.InputTokens > 0 || e.usage.OutputTokens > 0 {
		u := unifiedToOAUsage(e.usage)
		if err := e.usageChunk(w, u); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

func (e *openaiEncoder) usageChunk(w io.Writer, u *oaUsage) error {
	c := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []any{},
		"usage":   u,
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func unifiedToOAUsage(u types.Usage) *oaUsage {
	out := &oaUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out.PromptTokensDetails = &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		}{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// -- Anthropic encoder ---------------------------------------------------------

type anthropicEncoder struct {
	model      string
	id         string
	started    bool
	blockMap   map[int]int // upstream index -> client block index
	nextBlock  int
	openBlocks map[int]string // upstream index -> open block part type
	usage      types.Usage
	finished   bool
	stopReason string
	stopSeq    string
}

func (e *anthropicEncoder) raw(w io.Writer, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return nil
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func (e *anthropicEncoder) ensureStart(w io.Writer, ev StreamEvent) error {
	if e.started {
		return nil
	}
	e.started = true
	if ev.ID != "" {
		e.id = ev.ID
	}
	if ev.Model != "" {
		e.model = ev.Model
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 1}
	if ev.Usage != nil {
		e.usage.Merge(*ev.Usage)
	}
	// Unified InputTokens is cache-inclusive; Anthropic's input_tokens
	// excludes cache read/write — denormalize (never below 0).
	usage["input_tokens"] = anthropicInputTokens(e.usage)
	if e.usage.CacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = e.usage.CacheReadTokens
	}
	if e.usage.CacheWriteTokens > 0 {
		usage["cache_creation_input_tokens"] = e.usage.CacheWriteTokens
	}
	return e.raw(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      orDefault(e.id, "msg_onegw"),
			"type":    "message",
			"role":    "assistant",
			"model":   e.model,
			"content": []any{},
			"usage":   usage,
		},
	})
}

func (e *anthropicEncoder) blockIndex(upstream int) int {
	if e.blockMap == nil {
		e.blockMap = map[int]int{}
	}
	if idx, ok := e.blockMap[upstream]; ok {
		return idx
	}
	idx := e.nextBlock
	e.nextBlock++
	e.blockMap[upstream] = idx
	return idx
}

func (e *anthropicEncoder) encode(w io.Writer, ev StreamEvent) error {
	switch ev.Kind {
	case EvStart:
		return e.ensureStart(w, ev)
	case EvPartStart:
		if err := e.ensureStart(w, ev); err != nil {
			return err
		}
		if err := e.closeBlock(w, ev.Index); err != nil {
			return err
		}
		e.openBlocks = openBlocksSet(e.openBlocks, ev.Index, ev.PartType)
		return e.raw(w, "content_block_start", map[string]any{
			"type": "content_block_start", "index": e.blockIndex(ev.Index), "content_block": e.startBlock(ev.PartType, ev.ToolID, ev.ToolName),
		})
	case EvDelta:
		if err := e.ensureStart(w, ev); err != nil {
			return err
		}
		// Upstreams frequently emit deltas without a part start (OpenAI
		// text/thinking, Gemini thought parts): synthesize the
		// content_block_start, and close the previous block when the part
		// type changes on the same upstream index.
		if open, ok := e.openBlocks[ev.Index]; !ok || open != ev.PartType {
			if err := e.closeBlock(w, ev.Index); err != nil {
				return err
			}
			e.openBlocks = openBlocksSet(e.openBlocks, ev.Index, ev.PartType)
			if err := e.raw(w, "content_block_start", map[string]any{
				"type": "content_block_start", "index": e.blockIndex(ev.Index), "content_block": e.startBlock(ev.PartType, "", ""),
			}); err != nil {
				return err
			}
		}
		idx := e.blockIndex(ev.Index)
		var delta map[string]any
		switch ev.PartType {
		case types.PartToolUse:
			delta = map[string]any{"type": "input_json_delta", "partial_json": ev.ToolArgs}
		case types.PartThinking:
			if ev.Signature != "" {
				delta = map[string]any{"type": "signature_delta", "signature": ev.Signature}
			} else {
				delta = map[string]any{"type": "thinking_delta", "thinking": ev.Thinking}
			}
		default:
			delta = map[string]any{"type": "text_delta", "text": ev.Text}
		}
		return e.raw(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx, "delta": delta,
		})
	case EvPartStop:
		return e.closeBlock(w, ev.Index)
	case EvStop:
		// Defer message_delta to finish(): upstreams commonly send usage
		// after finish_reason, and message_delta carries final usage.
		if ev.Usage != nil {
			e.usage.Merge(*ev.Usage)
		}
		if ev.StopReason != "" {
			e.stopReason = ev.StopReason
			e.stopSeq = ev.StopSeq
			e.finished = true
		}
	case EvError, EvPing, EvEnd:
		return nil
	}
	return nil
}

// closeBlock emits content_block_stop for the open block at upstream index up
// (if any) and retires its client index: a later block at the same upstream
// index must open at a fresh client index, because Anthropic clients key
// content blocks by index.
func (e *anthropicEncoder) closeBlock(w io.Writer, up int) error {
	if _, ok := e.openBlocks[up]; !ok {
		return nil
	}
	delete(e.openBlocks, up)
	idx := e.blockIndex(up)
	delete(e.blockMap, up)
	return e.raw(w, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": idx,
	})
}

// startBlock builds the content_block payload for a part start.
func (e *anthropicEncoder) startBlock(partType, toolID, toolName string) map[string]any {
	switch partType {
	case types.PartToolUse:
		return map[string]any{"type": "tool_use", "id": orDefault(toolID, "toolu_onegw"+randHex(6)), "name": toolName, "input": map[string]any{}}
	case types.PartThinking:
		return map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	default:
		return map[string]any{"type": "text", "text": ""}
	}
}

func openBlocksSet(m map[int]string, up int, partType string) map[int]string {
	if m == nil {
		m = map[int]string{}
	}
	m[up] = partType
	return m
}

func (e *anthropicEncoder) finish(w io.Writer) error {
	if err := e.ensureStart(w, StreamEvent{}); err != nil {
		return err
	}
	// message events.
	ups := make([]int, 0, len(e.openBlocks))
	for up := range e.openBlocks {
		ups = append(ups, up)
	}
	sort.Ints(ups)
	for _, up := range ups {
		if err := e.closeBlock(w, up); err != nil {
			return err
		}
	}
	if !e.finished {
		e.stopReason = types.StopEndTurn
	}
	if err := e.raw(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": unmapAnthropicStop(e.stopReason), "stop_sequence": stopSeqOrNull(e.stopSeq)},
		"usage": map[string]any{
			"input_tokens":                anthropicInputTokens(e.usage),
			"output_tokens":               maxI64(e.usage.OutputTokens, 1),
			"cache_read_input_tokens":     e.usage.CacheReadTokens,
			"cache_creation_input_tokens": e.usage.CacheWriteTokens,
		},
	}); err != nil {
		return err
	}
	return e.raw(w, "message_stop", map[string]any{"type": "message_stop"})
}

func stopSeqOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// -- Gemini encoder ------------------------------------------------------------

type geminiEncoder struct {
	model       string
	started     bool
	finished    bool
	pendingTool *struct {
		name string
		args strings.Builder
		idx  int
	}
	usage   types.Usage
	stopRsn string
}

func (e *geminiEncoder) chunk(w io.Writer, parts []map[string]any, finish string, usage *gemUsage) error {
	cand := map[string]any{"index": 0}
	if len(parts) > 0 {
		cand["content"] = map[string]any{"role": "model", "parts": parts}
	}
	if finish != "" {
		cand["finishReason"] = finish
	}
	c := map[string]any{"candidates": []any{cand}}
	if usage != nil {
		c["usageMetadata"] = usage
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func (e *geminiEncoder) encode(w io.Writer, ev StreamEvent) error {
	switch ev.Kind {
	case EvStart:
		if ev.Model != "" {
			e.model = ev.Model
		}
		// Anthropic upstreams report prompt usage at message_start; keep
		// it so finish() can still emit it when no later event repeats it.
		if ev.Usage != nil {
			e.usage.Merge(*ev.Usage)
		}
	case EvPartStart:
		if ev.PartType == types.PartToolUse {
			e.pendingTool = &struct {
				name string
				args strings.Builder
				idx  int
			}{name: ev.ToolName, idx: ev.Index}
		}
	case EvDelta:
		if ev.PartType == types.PartToolUse && e.pendingTool != nil {
			e.pendingTool.args.WriteString(ev.ToolArgs)
			return nil
		}
		parts := []map[string]any{}
		if ev.PartType == types.PartThinking {
			parts = append(parts, map[string]any{"text": ev.Thinking, "thought": true})
		} else if ev.Text != "" {
			parts = append(parts, map[string]any{"text": ev.Text})
		}
		if len(parts) == 0 {
			return nil
		}
		return e.chunk(w, parts, "", nil)
	case EvPartStop:
		if e.pendingTool != nil {
			args := e.pendingTool.args.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			err := e.chunk(w, []map[string]any{{
				"functionCall": map[string]any{"name": e.pendingTool.name, "args": json.RawMessage(args)},
			}}, "", nil)
			e.pendingTool = nil
			return err
		}
	case EvStop:
		if ev.Usage != nil {
			e.usage.Merge(*ev.Usage)
		}
		if ev.StopReason != "" {
			e.stopRsn = unmapGeminiStop(ev.StopReason)
		}
	case EvError, EvPing, EvEnd:
		return nil
	}
	return nil
}

func (e *geminiEncoder) finish(w io.Writer) error {
	if e.finished {
		return nil
	}
	e.finished = true
	var usage *gemUsage
	if e.usage.InputTokens > 0 || e.usage.OutputTokens > 0 {
		usage = &gemUsage{
			PromptTokenCount:        e.usage.InputTokens,
			CandidatesTokenCount:    e.usage.OutputTokens,
			TotalTokenCount:         e.usage.InputTokens + e.usage.OutputTokens,
			CachedContentTokenCount: e.usage.CacheReadTokens,
			ThoughtsTokenCount:      e.usage.ReasoningTokens,
		}
	}
	return e.chunk(w, nil, orDefault(e.stopRsn, "STOP"), usage)
}
