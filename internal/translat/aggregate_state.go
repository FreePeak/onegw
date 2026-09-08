package translat

import (
	"encoding/json"
	"fmt"
	"strings"

	"onegw/internal/types"
)

// AggregateState accumulates unified stream events into a ChatResponse. Both
// stream translators feed it for clients that asked for a non-streaming
// response from a stream-only upstream.
// AggregateCap bounds the memory a forced-stream aggregation may buffer.
const AggregateCap = 16 << 20 // 16 MiB

type AggregateState struct {
	resp     types.ChatResponse
	texts    strings.Builder
	think    strings.Builder
	args     strings.Builder
	toolID   string
	toolName string
	inTool   bool
	buf      int64 // total accumulated bytes (texts, thinking, tool args)
	overflow bool
}

func (a *AggregateState) Aggregate(ev StreamEvent) bool {
	if a.overflow {
		return false
	}
	switch ev.Kind {
	case EvStart:
		a.resp.ID = orDefault(ev.ID, a.resp.ID)
		a.resp.Model = orDefault(ev.Model, a.resp.Model)
	case EvDelta:
		switch ev.PartType {
		case types.PartText:
			a.texts.WriteString(ev.Text)
			a.buf += int64(len(ev.Text))
		case types.PartThinking:
			a.think.WriteString(ev.Thinking)
			a.buf += int64(len(ev.Thinking))
		case types.PartToolUse:
			if !a.inTool {
				a.inTool = true
				a.toolID = fmt.Sprintf("call_%d", len(a.resp.Content))
			}
			a.args.WriteString(ev.ToolArgs)
			a.buf += int64(len(ev.ToolArgs))
		}
	case EvPartStart:
		a.flushTool()
		a.inTool = true
		a.toolID = orDefault(ev.ToolID, "call_"+ev.ToolName)
		a.toolName = ev.ToolName
		a.args.Reset()
	case EvPartStop:
		a.flushTool()
	case EvStop:
		if ev.Usage != nil {
			a.resp.Usage.Merge(*ev.Usage)
		}
		if ev.StopReason != "" {
			a.resp.StopReason = ev.StopReason
		}
	}
	if a.buf > AggregateCap {
		a.overflow = true
		return false
	}
	return true
}

func (a *AggregateState) flushTool() {
	if !a.inTool {
		return
	}
	a.inTool = false
	a.resp.Content = append(a.resp.Content, types.Part{
		Type: types.PartToolUse,
		ID:   a.toolID,
		Name: a.toolName,
		Args: normalizeArgs(json.RawMessage(a.args.String())),
	})
	a.toolID, a.toolName = "", ""
	a.args.Reset()
}

func (a *AggregateState) Result() *types.ChatResponse {
	a.flushTool()
	if t := a.texts.String(); t != "" {
		a.resp.Content = append([]types.Part{{Type: types.PartText, Text: t}}, a.resp.Content...)
	}
	if t := a.think.String(); t != "" {
		a.resp.Content = append([]types.Part{{Type: types.PartThinking, Text: t}}, a.resp.Content...)
	}
	if a.resp.StopReason == "" {
		a.resp.StopReason = types.StopEndTurn
	}
	return &a.resp
}
