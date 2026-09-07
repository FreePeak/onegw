package translat

import (
	"encoding/json"
	"fmt"

	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// Responses SSE decoding (upstream -> unified StreamEvents)
//
// Event vocabulary and edge cases mirror the live-verified reference in
// 9router (open-sse/translator/response/openai-responses.js):
//   - event name comes from the SSE `event:` line or the payload "type"
//   - tool calls are correlated by the server item id at
//     response.output_item.added time, so parallel calls whose deltas
//     interleave never merge into one argument stream
//   - response.output_item.done carries full arguments when no deltas did
//   - terminal events: response.completed / response.done /
//     response.failed / error; a stream that closes without one is an
//     error, not a clean finish
// ---------------------------------------------------------------------------

type rsStreamEvent struct {
	Type  string `json:"type"`
	Event string `json:"event"`

	// response.created / completed / failed carry the full response object
	Response *struct {
		ID     string    `json:"id"`
		Model  string    `json:"model"`
		Status string    `json:"status"`
		Usage  *rsUsage  `json:"usage"`
		Error  *rsErrObj `json:"error"`
	} `json:"response"`

	// deltas
	Delta     string `json:"delta"`
	Text      string `json:"text"`
	ItemID    string `json:"item_id"`
	OutputIdx int    `json:"output_index"`

	// item lifecycle
	Item *struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Content   json.RawMessage `json:"content"`
	} `json:"item"`

	Error *rsErrObj `json:"error"`
}

type rsErrObj struct {
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Message string `json:"message"`
}

// responsesDecoder is stateful per stream: it tracks tool-call correlation
// and terminal-event arrival.
type responsesDecoder struct {
	toolIdx      map[string]int // server item id -> unified part index
	nextToolIdx  int
	argsEmitted  map[int]bool
	terminalSeen bool
}

func newResponsesDecoder() *responsesDecoder {
	return &responsesDecoder{
		toolIdx:     map[string]int{},
		argsEmitted: map[int]bool{},
	}
}

func (d *responsesDecoder) name(ev sseEvent, c *rsStreamEvent) string {
	if ev.Name != "" {
		return ev.Name
	}
	if c.Type != "" {
		return c.Type
	}
	return c.Event
}

func (d *responsesDecoder) decode(ev sseEvent) ([]StreamEvent, error) {
	if len(ev.Data) == 0 {
		return nil, nil
	}
	var c rsStreamEvent
	if err := json.Unmarshal(ev.Data, &c); err != nil {
		return nil, fmt.Errorf("responses event: %w", err)
	}
	switch d.name(ev, &c) {
	case "response.created":
		if c.Response != nil {
			return []StreamEvent{{Kind: EvStart, ID: c.Response.ID, Model: c.Response.Model}}, nil
		}
		return nil, nil

	case "response.output_item.added":
		if c.Item == nil || c.Item.Type != "function_call" {
			return nil, nil
		}
		key := c.Item.ID
		if key == "" {
			key = c.ItemID
		}
		idx, ok := d.toolIdx[key]
		if !ok {
			idx = d.nextToolIdx
			d.nextToolIdx++
			if key != "" {
				d.toolIdx[key] = idx
			}
		}
		return []StreamEvent{{
			Kind: EvPartStart, Index: idx, PartType: types.PartToolUse,
			ToolID: c.Item.CallID, ToolName: c.Item.Name,
		}}, nil

	case "response.output_text.delta":
		if c.Delta == "" {
			return nil, nil
		}
		return []StreamEvent{{Kind: EvDelta, PartType: types.PartText, Text: c.Delta}}, nil

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if c.Delta == "" {
			return nil, nil
		}
		return []StreamEvent{{Kind: EvDelta, PartType: types.PartThinking, Thinking: c.Delta}}, nil

	case "response.function_call_arguments.delta":
		if c.Delta == "" {
			return nil, nil
		}
		idx := d.resolveToolIdx(&c)
		d.argsEmitted[idx] = true
		return []StreamEvent{{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: c.Delta}}, nil

	case "response.output_item.done":
		// Some upstreams send complete arguments only here.
		if c.Item == nil || c.Item.Type != "function_call" || c.Item.Arguments == "" {
			return nil, nil
		}
		idx := d.resolveToolIdx(&c)
		if d.argsEmitted[idx] {
			return nil, nil
		}
		d.argsEmitted[idx] = true
		return []StreamEvent{{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: c.Item.Arguments}}, nil

	case "response.completed", "response.done":
		d.terminalSeen = true
		stop := StreamEvent{Kind: EvStop, StopReason: types.StopEndTurn}
		if c.Response != nil {
			if c.Response.Status == "incomplete" {
				stop.StopReason = types.StopMaxTokens
			}
			if c.Response.Usage != nil {
				u := rsUsageToUnified(c.Response.Usage)
				stop.Usage = &u
			}
		}
		return []StreamEvent{stop}, nil

	case "response.failed", "error":
		d.terminalSeen = true
		e := c.Error
		if e == nil && c.Response != nil {
			e = c.Response.Error
		}
		msg := "responses stream failed"
		typ := "upstream_error"
		code := ""
		if e != nil {
			if e.Message != "" {
				msg = e.Message
			}
			if e.Type != "" {
				typ = e.Type
			}
			code = errCodeString(e.Code)
		}
		return []StreamEvent{{Kind: EvError, Err: &types.APIError{Status: 502, Type: typ, Code: code, Message: msg}}}, nil

	default:
		// in_progress, content_part.*, output_text.done, reasoning part
		// boundaries, annotations: lifecycle noise, ignored.
		return nil, nil
	}
}

func (d *responsesDecoder) resolveToolIdx(c *rsStreamEvent) int {
	key := c.ItemID
	if c.Item != nil && c.Item.ID != "" {
		key = c.Item.ID
	}
	if idx, ok := d.toolIdx[key]; ok {
		return idx
	}
	idx := d.nextToolIdx
	d.nextToolIdx++
	if key != "" {
		d.toolIdx[key] = idx
	}
	return idx
}

// finish enforces terminal semantics: a stream that ends without a terminal
// event is a disconnect, not a clean completion.
func (d *responsesDecoder) finish() error {
	if !d.terminalSeen {
		return fmt.Errorf("responses stream closed before response.completed")
	}
	return nil
}

// streamDecoder is the per-stream decode seam. Formats without per-stream
// state wrap the stateless decodeStreamEvent dispatcher.
type streamDecoder interface {
	decode(ev sseEvent) ([]StreamEvent, error)
}

type statelessDecoder struct {
	f  Format
	fn func(Format, sseEvent) ([]StreamEvent, error)
}

func (s statelessDecoder) decode(ev sseEvent) ([]StreamEvent, error) { return s.fn(s.f, ev) }

// newStreamDecoder returns the decoder for a format plus an optional
// terminal check run at clean stream end (nil = no check).
func newStreamDecoder(f Format) (streamDecoder, func() error) {
	if f == FmtResponses {
		d := newResponsesDecoder()
		return d, d.finish
	}
	return statelessDecoder{f: f, fn: decodeStreamEvent}, nil
}
