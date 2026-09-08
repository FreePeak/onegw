package translat

// CommandCode wire format (https://api.commandcode.ai/alpha/generate).
//
// The upstream is a CLI agent backend, not a chat-completions API:
//   - Request: {threadId, memory, config{...}, params{model, messages,
//     stream, max_tokens, temperature, system?, tools?, top_p?}}. System
//     prompts are a top-level params.system STRING (Anthropic-style; system
//     messages are rejected inside params.messages). Message content is
//     always an array of blocks; tool calls are "tool-call" blocks with a
//     parsed input object; tool results are "tool-result" blocks.
//   - Response: AI SDK v5 NDJSON — one JSON event per line, NO "data:"
//     prefix (text-delta / reasoning-delta / tool-input-* / tool-call /
//     finish-step / finish). Streaming is forced: there is no non-streaming
//     response shape, so non-stream clients get an aggregated completion.
//   - Errors can arrive INSIDE a 200 response as a {"type":"error"} event.
//     InspectCommandCodeHead reads the stream head (bounded) so the gateway
//     can still answer a real 4xx/5xx before any bytes go to the client.

import (
	"crypto/rand"

	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"onegw/internal/types"
)

// CommandCodeVersion is the x-command-code-version the upstream expects.
const CommandCodeVersion = "0.25.7"

// ccDefaultMaxTokens mirrors 9router's DEFAULT_MAX_TOKENS: the upstream
// rejects requests without an output cap.
const ccDefaultMaxTokens = 64000

// ---------------------------------------------------------------------------
// Request: unified -> commandcode
// ---------------------------------------------------------------------------

type ccBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     *ccToolOutput   `json:"output,omitempty"`
}

type ccToolOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type ccMessage struct {
	Role    string    `json:"role"`
	Content []ccBlock `json:"content"`
}

type ccToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type ccParams struct {
	Model       string      `json:"model"`
	Messages    []ccMessage `json:"messages"`
	Stream      bool        `json:"stream"`
	MaxTokens   int         `json:"max_tokens"`
	Temperature *float64    `json:"temperature,omitempty"`
	System      string      `json:"system,omitempty"`
	Tools       []ccToolDef `json:"tools,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
}

type ccConfig struct {
	WorkingDir    string `json:"workingDir"`
	Date          string `json:"date"`
	Environment   string `json:"environment"`
	Structure     []any  `json:"structure"`
	IsGitRepo     bool   `json:"isGitRepo"`
	CurrentBranch string `json:"currentBranch"`
	MainBranch    string `json:"mainBranch"`
	GitStatus     string `json:"gitStatus"`
	RecentCommits []any  `json:"recentCommits"`
}

type ccRequest struct {
	ThreadID string   `json:"threadId"`
	Memory   string   `json:"memory"`
	Config   ccConfig `json:"config"`
	Params   ccParams `json:"params"`
}

// EncodeCommandCodeRequest renders the unified request as a CommandCode
// /alpha/generate body. Streaming is always forced on (the upstream has no
// non-streaming mode); non-stream clients are served by AggregateStream.
func EncodeCommandCodeRequest(u *types.ChatRequest) ([]byte, error) {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		wd = "/"
	}
	req := ccRequest{
		ThreadID: newUUID(),
		Memory:   "",
		Config: ccConfig{
			WorkingDir:    wd,
			Date:          time.Now().UTC().Format("2006-01-02"),
			Environment:   runtime.GOOS,
			Structure:     []any{},
			RecentCommits: []any{},
		},
		Params: ccParams{
			Model:       u.Model,
			Stream:      true,
			MaxTokens:   ccDefaultMaxTokens,
			Temperature: u.Temperature,
			TopP:        u.TopP,
		},
	}
	if u.MaxTokens > 0 {
		req.Params.MaxTokens = u.MaxTokens
	}
	var system []string
	for _, p := range u.System {
		if p.Text != "" {
			system = append(system, p.Text)
		}
	}
	req.Params.System = strings.Join(system, "\n\n")

	for _, m := range u.Messages {
		switch m.Role {
		case types.RoleSystem:
			if t := m.FlattenText(); t != "" {
				req.Params.System = strings.TrimSpace(req.Params.System + "\n\n" + t)
			}
		case types.RoleTool:
			req.Params.Messages = append(req.Params.Messages, ccMessage{
				Role: "tool",
				Content: []ccBlock{{
					Type:       "tool-result",
					ToolCallID: orDefault(m.ToolCallID, m.Name),
					ToolName:   m.Name,
					Output:     &ccToolOutput{Type: "text", Value: m.FlattenText()},
				}},
			})
		case types.RoleAssistant:
			msg := ccMessage{Role: "assistant"}
			if t := assistantText(m); t != "" {
				msg.Content = append(msg.Content, ccBlock{Type: "text", Text: t})
			}
			for _, p := range m.Content {
				if p.Type != types.PartToolUse {
					continue
				}
				input := p.Args
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				msg.Content = append(msg.Content, ccBlock{
					Type:       "tool-call",
					ToolCallID: orDefault(p.ID, "call_"+p.Name),
					ToolName:   p.Name,
					Input:      input,
				})
			}
			if len(msg.Content) == 0 {
				msg.Content = append(msg.Content, ccBlock{Type: "text", Text: ""})
			}
			req.Params.Messages = append(req.Params.Messages, msg)
		default: // user (and anything else): text blocks + split-out tool results
			var blocks []ccBlock
			var results []ccMessage
			for _, p := range m.Content {
				switch p.Type {
				case types.PartToolResult:
					results = append(results, ccMessage{
						Role: "tool",
						Content: []ccBlock{{
							Type:       "tool-result",
							ToolCallID: orDefault(p.ToolUseID, m.ToolCallID),
							ToolName:   p.Name,
							Output:     &ccToolOutput{Type: "text", Value: p.Text},
						}},
					})
				case types.PartImage:
					blocks = append(blocks, ccBlock{Type: "text", Text: "[image omitted]"})
				default:
					if p.Text != "" {
						blocks = append(blocks, ccBlock{Type: "text", Text: p.Text})
					}
				}
			}
			if len(blocks) == 0 {
				blocks = append(blocks, ccBlock{Type: "text", Text: ""})
			}
			req.Params.Messages = append(req.Params.Messages, ccMessage{Role: "user", Content: blocks})
			req.Params.Messages = append(req.Params.Messages, results...)
		}
	}

	for _, t := range u.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		req.Params.Tools = append(req.Params.Tools, ccToolDef{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return json.Marshal(req)
}

// assistantText flattens the text parts of an assistant message.
func assistantText(m types.Message) string {
	var sb strings.Builder
	for _, p := range m.Content {
		if p.Type == types.PartText && p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// NDJSON line reader
// ---------------------------------------------------------------------------

// readNDJSON yields one sseEvent per non-blank line. Unlike readSSE it does
// not understand "data:" framing — CommandCode emits bare JSON lines — but a
// stray "data:" prefix or "[DONE]" is tolerated by the decoders.
func readNDJSON(r *bufio.Reader, yield func(sseEvent) error) error {
	for {
		line, err := readLine(r)
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			if yerr := yield(sseEvent{Data: []byte(trimmed)}); yerr != nil {
				return yerr
			}
		}
		if err != nil {
			if err == io.EOF {
				return io.EOF
			}
			return err
		}
	}
}

// ccLineJSON strips an optional "data:" prefix and parses one NDJSON line.
// ok=false means the line is not a usable JSON object (skip it).
func ccLineJSON(data []byte) (map[string]any, bool) {
	s := strings.TrimSpace(string(data))
	s = strings.TrimPrefix(s, "data:")
	s = strings.TrimSpace(s)
	if s == "" || s == "[DONE]" {
		return nil, false
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(s), &ev); err != nil {
		return nil, false
	}
	return ev, true
}

func ccEventType(ev map[string]any) string {
	s, _ := ev["type"].(string)
	return s
}

func ccEventString(ev map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := ev[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func ccEventInt(ev map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := ev[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			n, _ := v.Int64()
			return n
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Stream decoding: commandcode NDJSON events -> StreamEvent
// ---------------------------------------------------------------------------

// ccStreamState carries per-stream decoder state (tool ids -> unified part
// index, finish-step carry-over). CommandCode events do not number tool
// parts, so the mapping must be built as the stream unfolds.
type ccStreamState struct {
	started      bool
	toolIdx      map[string]int
	nextTool     int
	toolStreamed map[string]bool
	finishReason string
	usage        *types.Usage
}

func decodeCommandCodeStreamEvent(ev sseEvent, st *ccStreamState) ([]StreamEvent, error) {
	obj, ok := ccLineJSON(ev.Data)
	if !ok {
		return nil, nil
	}
	var out []StreamEvent
	start := func() {
		if !st.started {
			st.started = true
			out = append(out, StreamEvent{Kind: EvStart, ID: ccEventString(obj, "messageId", "id")})
		}
	}
	switch ccEventType(obj) {
	case "text-delta":
		text := ccEventString(obj, "text", "delta")
		if text == "" {
			return nil, nil
		}
		start()
		out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartText, Text: text})
	case "reasoning-delta":
		text := ccEventString(obj, "text", "delta")
		if text == "" {
			return nil, nil
		}
		start()
		out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartThinking, Thinking: text})
	case "tool-input-start":
		id := ccEventString(obj, "id", "toolCallId")
		if id == "" {
			id = fmt.Sprintf("call_%d", st.nextTool)
		}
		idx := st.toolIndex(id)
		start()
		st.toolStreamed[id] = true
		out = append(out, StreamEvent{Kind: EvPartStart, Index: idx, PartType: types.PartToolUse, ToolID: id, ToolName: ccEventString(obj, "toolName")})
	case "tool-input-delta":
		id := ccEventString(obj, "id", "toolCallId")
		idx, known := st.toolIdx[id]
		if !known {
			idx = st.toolIndex(id)
		}
		args := ccEventString(obj, "delta", "inputTextDelta")
		if args == "" {
			return nil, nil
		}
		out = append(out, StreamEvent{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: args})
	case "tool-call":
		// Consolidated call; only emit when no tool-input-* deltas streamed it.
		id := ccEventString(obj, "toolCallId", "id")
		if st.toolStreamed[id] {
			return nil, nil
		}
		idx := st.toolIndex(id)
		start()
		args := ccEventString(obj, "input")
		if args == "" {
			if raw, err := json.Marshal(obj["input"]); err == nil {
				args = string(raw)
				if args == "null" {
					args = "{}"
				}
			}
		}
		out = append(out,
			StreamEvent{Kind: EvPartStart, Index: idx, PartType: types.PartToolUse, ToolID: id, ToolName: ccEventString(obj, "toolName")},
			StreamEvent{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: args},
			StreamEvent{Kind: EvPartStop, Index: idx, PartType: types.PartToolUse},
		)
	case "finish-step":
		if r := ccEventString(obj, "finishReason"); r != "" {
			st.finishReason = r
		}
		if u := ccUsage(obj["usage"]); u != nil {
			st.usage = u
		}
	case "finish":
		reason := st.finishReason
		if reason == "" {
			reason = ccEventString(obj, "finishReason")
		}
		u := st.usage
		if nu := ccUsage(obj["totalUsage"]); nu != nil {
			u = nu
		}
		ev := StreamEvent{Kind: EvStop, StopReason: mapCCFinish(orDefault(reason, "stop")), Usage: u}
		if !st.started {
			st.started = true
			out = append(out, StreamEvent{Kind: EvStart})
		}
		out = append(out, ev)
	case "error":
		// In-200 error. Head inspection normally intercepts this before the
		// client sees anything; mid-stream it aborts translation (the caller
		// surfaces a stream_translate_failed).
		return nil, ParseCommandCodeError(obj)
	// Ignored (no client-visible content): start, start-step, text-start,
	// text-end, reasoning-start, reasoning-end, tool-input-end,
	// provider-metadata, message-metadata, ...
	default:
	}
	return out, nil
}

func (st *ccStreamState) toolIndex(id string) int {
	if st.toolIdx == nil {
		st.toolIdx = map[string]int{}
	}
	if idx, ok := st.toolIdx[id]; ok {
		return idx
	}
	idx := st.nextTool
	st.nextTool++
	st.toolIdx[id] = idx
	if st.toolStreamed == nil {
		st.toolStreamed = map[string]bool{}
	}
	return idx
}

// ccUsage maps a commandcode usage object ({inputTokens,outputTokens,...}).
func ccUsage(raw any) *types.Usage {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	u := &types.Usage{
		InputTokens:  ccEventInt(m, "inputTokens", "input_tokens"),
		OutputTokens: ccEventInt(m, "outputTokens", "output_tokens"),
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	return u
}

func mapCCFinish(s string) string {
	switch s {
	case "length", "max-tokens":
		return types.StopMaxTokens
	case "tool-calls", "tool_use":
		return types.StopToolUse
	case "content-filter":
		return types.StopContentFilter
	default:
		return types.StopEndTurn
	}
}

// ---------------------------------------------------------------------------
// In-200 error handling
// ---------------------------------------------------------------------------

// ParseCommandCodeError converts a {"type":"error"} event into a unified
// APIError, synthesizing an HTTP status from explicit statusCode/status
// fields or from message keywords (ported from 9router's
// parseCommandCodeError). Returns an APIError with status 503 for anything
// unclassifiable.
func ParseCommandCodeError(event map[string]any) *types.APIError {
	if event == nil {
		return &types.APIError{Status: 503, Type: "server_error", Message: "CommandCode upstream error"}
	}
	var message string
	status := 0
	errType := "server_error"

	errVal := event["error"]
	if errVal == nil {
		errVal = event["message"]
	}
	switch v := errVal.(type) {
	case map[string]any:
		message = ccEventString(v, "message", "error")
		if message == "" {
			if b, err := json.Marshal(v); err == nil {
				message = string(b)
			}
		}
		if s := int(ccEventInt(v, "statusCode", "status")); s != 0 {
			status = s
		}
		if t := ccEventString(v, "type"); t != "" {
			errType = t
		}
	case string:
		message = v
	default:
		if b, err := json.Marshal(v); err == nil {
			message = string(b)
		} else {
			message = "unknown"
		}
	}
	if message == "" {
		message = "unknown"
	}
	if s := int(ccEventInt(event, "statusCode")); s != 0 {
		status = s
	}
	if status < 400 || status > 599 {
		status, errType = classifyCCError(message)
	} else if status >= 400 {
		// Explicit upstream status: keep it, but keep the classified type
		// (which may be more specific than "server_error") for the class.
		_, errType = classifyCCError(message)
	}
	return &types.APIError{Status: status, Type: errType, Message: "[CommandCode error: " + message + "]"}
}

func classifyCCError(message string) (int, string) {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"):
		return 429, "rate_limit_error"
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "invalid api key"), strings.Contains(lower, "authentication"):
		return 401, "authentication_error"
	case strings.Contains(lower, "payment required"), strings.Contains(lower, "billing"):
		return 402, "billing_error"
	case strings.Contains(lower, "quota"), strings.Contains(lower, "forbidden"), strings.Contains(lower, "permission"):
		return 403, "permission_error"
	case strings.Contains(lower, "not found"):
		return 404, "invalid_request_error"
	case strings.Contains(lower, "unavailable"), strings.Contains(lower, "overloaded"), strings.Contains(lower, "server error"):
		return 503, "server_error"
	default:
		return 503, "server_error"
	}
}

// ccHeadCap bounds the error-inspection prefix (64 KiB).
const ccHeadCap = 64 << 10

// InspectCommandCodeHead reads the head of a CommandCode NDJSON stream until
// the first terminal event (content, tool activity, finish, [DONE]) or the
// byte cap. It returns the consumed prefix (which the caller must replay
// into the translator) and a synthesized APIError when the head carries an
// in-200 {"type":"error"} event, letting the gateway answer a real status
// code before any bytes reach the client.
func InspectCommandCodeHead(body io.Reader) (head []byte, apiErr *types.APIError, err error) {
	var buf bytes.Buffer
	for {
		line, rerr := ccHeadLine(body)
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			buf.WriteString(trimmed)
			buf.WriteByte('\n')
			if obj, ok := ccLineJSON([]byte(trimmed)); ok {
				switch ccEventType(obj) {
				case "error":
					return nil, ParseCommandCodeError(obj), nil
				case "text-delta", "reasoning-delta", "tool-input-start", "tool-call",
					"finish", "finish-step":
					return buf.Bytes(), nil, nil
				}
			} else if strings.Contains(trimmed, "[DONE]") {
				return buf.Bytes(), nil, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return buf.Bytes(), nil, nil
			}
			return buf.Bytes(), nil, rerr
		}
		if buf.Len() > ccHeadCap {
			return buf.Bytes(), nil, nil
		}
	}
}

// ccHeadLine reads one \n-terminated line WITHOUT buffering: reads one byte
// at a time from the body so that bytes after the consumed line stay in the
// stream for the replaying reader. (A bufio.Reader here would pull the whole
// response into its internal buffer and drop the unconsumed tail.) The head
// is bounded by ccHeadCap, so the extra syscalls are negligible.
func ccHeadLine(r io.Reader) (string, error) {
	var sb strings.Builder
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			if b[0] == '\n' {
				return strings.TrimSuffix(sb.String(), "\r"), nil
			}
			sb.WriteByte(b[0])
			if sb.Len() > maxLine {
				return sb.String(), fmt.Errorf("ndjson line exceeds %d bytes", maxLine)
			}
		}
		if err != nil {
			if err == io.EOF {
				return sb.String(), io.EOF
			}
			return sb.String(), err
		}
	}
}

// newUUID returns a random RFC 4122 v4 UUID (crypto/rand).
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
