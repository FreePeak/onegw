// Package types defines the unified internal request/response model shared by
// every wire-format translator. Wire formats (OpenAI, Anthropic, Gemini) parse
// into this model and re-encode from it; nothing else needs to know the wire
// shapes. Passthrough paths bypass this model entirely for zero-copy speed.
package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Role names in the unified model. System content is hoisted out of Messages
// because Anthropic and Gemini model it as a dedicated field.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Part types.
const (
	PartText       = "text"
	PartImage      = "image"
	PartToolUse    = "tool_use"
	PartToolResult = "tool_result"
	PartThinking   = "thinking"
)

// Stop reasons (unified; translators map to/from wire values).
const (
	StopEndTurn       = "end_turn"
	StopMaxTokens     = "max_tokens"
	StopStopSequence  = "stop_sequence"
	StopToolUse       = "tool_use"
	StopContentFilter = "content_filter"
)

// ChatRequest is the unified request model.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	System      []Part    `json:"system,omitempty"` // hoisted system content
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"` // nil | "none" | "auto" | "any" | ToolChoiceTool
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	TopK        *int      `json:"top_k,omitempty"`

	StopSequences []string `json:"stop_sequences,omitempty"`

	// Reasoning controls. Effort is a free-form string (OpenAI dialect:
	// none|minimal|low|medium|high|max; always-thinking upstreams like GLM
	// accept only low|high|max — see provider AlwaysThinking). Budget is
	// explicit thinking-token budget. Exactly one may be set.
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	Thinking        *ThinkingCfg `json:"thinking,omitempty"`

	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// SessionID keys usage rollups (from header or metadata). Never sent
	// upstream as model input.
	SessionID string `json:"session_id,omitempty"`

	// Metadata passthrough (user ids etc), format-specific keys preserved.
	Metadata map[string]string `json:"metadata,omitempty"`

	// ParallelToolCalls nil = unset. Upstreams that lack the knob ignore it.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

type ThinkingCfg struct {
	BudgetTokens int `json:"budget_tokens"`
}

// Message is one unified conversation turn. Content is always part-encoded
// internally; translators flatten plain strings where the wire format allows.
type Message struct {
	Role    string `json:"role"`
	Content []Part `json:"content"`

	// ToolCallID marks a tool-role message (OpenAI shape); translators merge
	// it into a tool_result part when targeting Anthropic/Gemini.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"` // tool name for tool messages
}

type Part struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// Image.
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"` // base64-decoded bytes
	URL      string `json:"url,omitempty"`

	// ToolUse.
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"` // JSON object

	// ToolUseID links a tool_result part to its tool_use part. OpenAI tool
	// messages carry it at message level; unified keeps it on the part.
	ToolUseID string `json:"tool_use_id,omitempty"`

	// ToolResult.
	IsError bool `json:"is_error,omitempty"`

	// Thinking.
	Signature string `json:"signature,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"` // JSON Schema object
}

// ToolChoiceTool requests a specific tool.
type ToolChoiceTool struct {
	Name string `json:"name"`
}

// ToolChoiceNone/Auto/Any are the non-tool selections.
type ToolChoiceMode string

const (
	ToolChoiceNone ToolChoiceMode = "none"
	ToolChoiceAuto ToolChoiceMode = "auto"
	ToolChoiceAny  ToolChoiceMode = "any" // "required" in OpenAI
)

type ResponseFormat struct {
	// Type: "" (default), "json_object", "json_schema".
	Type       string          `json:"type,omitempty"`
	Schema     json.RawMessage `json:"schema,omitempty"`
	SchemaName string          `json:"schema_name,omitempty"`
}

// ChatResponse is the unified non-streaming response.
type ChatResponse struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Content      []Part `json:"content"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
	Usage        Usage  `json:"usage"`
}

// Usage is unified token accounting. Counters the upstream did not report
// stay 0; cost estimation treats 0 input/output with Estimated=true as
// char-derived approximation.
type Usage struct {
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	Estimated        bool   `json:"estimated,omitempty"`
	UpstreamFormat   string `json:"upstream_format,omitempty"` // openai|anthropic|gemini
}

// APIError is the unified error payload. RetryAfter, when set, is written
// as the Retry-After response header at the moment this error is emitted
// to the client — never earlier, so a fallen-through attempt cannot leak
// it onto a later successful response.
type APIError struct {
	Status     int    `json:"-"`
	Type       string `json:"type,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	RetryAfter string `json:"-"` // seconds hint for 429/503-class errors
}

// Merge folds o into u keeping maxima (streams may repeat counts).
func (u *Usage) Merge(o Usage) {
	if o.InputTokens > u.InputTokens {
		u.InputTokens = o.InputTokens
	}
	if o.OutputTokens > u.OutputTokens {
		u.OutputTokens = o.OutputTokens
	}
	if o.CacheReadTokens > u.CacheReadTokens {
		u.CacheReadTokens = o.CacheReadTokens
	}
	if o.CacheWriteTokens > u.CacheWriteTokens {
		u.CacheWriteTokens = o.CacheWriteTokens
	}
	if o.ReasoningTokens > u.ReasoningTokens {
		u.ReasoningTokens = o.ReasoningTokens
	}
	if o.UpstreamFormat != "" {
		u.UpstreamFormat = o.UpstreamFormat
	}
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Type, e.Message)
}

// Retryable reports whether the error class should trigger combo fallback.
func (e *APIError) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.Status {
	case 408, 409, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

// OverQuota reports whether the error indicates quota/rate exhaustion, which
// also marks the account cooling down.
func (e *APIError) OverQuota() bool {
	if e == nil {
		return false
	}
	if e.Status == 429 || e.Status == 529 {
		return true
	}
	code := strings.ToLower(e.Code)
	return code == "rate_limit_exceeded" || code == "quota_exceeded" ||
		strings.Contains(strings.ToLower(e.Type), "rate_limit")
}

// RegionLocked reports whether the upstream refused this account's
// credential for a region/availability policy (e.g. OpenCode Go
// RegionError: the key's workspace has not opted into the China-hosted
// route). The account is unusable for the model regardless of retries on
// the same credential, so callers cool it and rotate to another key.
func (e *APIError) RegionLocked() bool {
	return e != nil && e.Status == 403 && strings.Contains(strings.ToLower(e.Type), "region")
}

// EstimateTokens gives a rough char/4 estimate for text content; used only
// when upstream usage is missing.
func (r *ChatRequest) EstimateTokens() int64 {
	var n int
	for _, p := range r.System {
		n += len(p.Text)
	}
	for _, m := range r.Messages {
		n += 8
		for _, p := range m.Content {
			n += len(p.Text) + len(p.Data)/3 + 32
		}
	}
	return int64(n / 4)
}

// FlattenText renders part content as plain text (used for formats whose
// content is a plain string).
func (m *Message) FlattenText() string {
	var b strings.Builder
	for i, p := range m.Content {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return b.String()
}
