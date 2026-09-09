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
	// none|minimal|low|medium|high|max; xhigh is a client ladder above
	// high). Always-thinking upstreams like GLM accept only low|high|max —
	// the server coerces (see provider AlwaysThinking). Budget is
	// explicit thinking-token budget. Exactly one may be set.
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	Thinking        *ThinkingCfg `json:"thinking,omitempty"`

	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// SessionID keys usage rollups (from header or metadata). Never sent
	// upstream as model input.
	SessionID string `json:"session_id,omitempty"`

	// Metadata passthrough (user ids etc), format-specific keys preserved.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Cache-affinity knobs captured verbatim from the incoming wire
	// (issue #32): empty means the client never sent it, and encoders
	// re-emit only on wires that accept the knob — never invented.
	// PromptCacheKey is the OpenAI-dialect prompt_cache_key (sticky
	// prompt-cache routing); StickySessionID is OpenRouter's session_id
	// body knob (unified tag differs from the wire name to avoid
	// colliding with SessionID's usage-rollup tag; each decoder/encoder
	// maps the wire name explicitly). Both are deliberately separate
	// from SessionID above, which keys usage rollups and must never
	// travel upstream.
	PromptCacheKey  string `json:"prompt_cache_key,omitempty"`
	StickySessionID string `json:"session_affinity_key,omitempty"`

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

	// CacheBreakpoint marks an Anthropic-style prompt-cache breakpoint:
	// the client put cache_control {type: ephemeral} on this content
	// block, and the same marker must re-anchor at the same logical
	// block when the request re-encodes to a wire that supports it
	// (issue #32).
	CacheBreakpoint bool `json:"cache_breakpoint,omitempty"`
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

// Usage is unified token accounting. InputTokens is the TOTAL prompt size,
// cache-INCLUSIVE: CacheReadTokens and CacheWriteTokens are subsets of it
// (mirrors OpenAI's prompt_tokens semantics; issue #31). Translators
// normalize at the decode boundary: Anthropic reports input_tokens
// EXCLUDING cache reads/writes, so its decoders add cache read+write into
// InputTokens and its encoders subtract them back out (clamped at 0).
// Counters the upstream did not report stay 0; cost estimation treats
// 0 input/output with Estimated=true as char-derived approximation.
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
	// Fallbackable marks a per-model capability rejection (e.g. a learned
	// always-thinking upstream that cannot serve the request even after the
	// gateway coerced the body): Router.Execute retries the target once
	// (the attempt now coerces upfront) and then falls through to the next
	// combo target instead of surfacing the 400. Never serialized.
	Fallbackable bool `json:"-"`

	// NoSameTargetRetry marks a pre-first-byte budget exhaustion (the
	// gateway's own ResponseHeaderTimeout): the request's pre-first-byte
	// demand is fixed, so a second attempt on the SAME target burns a
	// second full budget. Router.Execute falls through to the next combo
	// target immediately; a direct route surfaces the 504. Never
	// serialized.
	NoSameTargetRetry bool `json:"-"`
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

// SharedConcurrency reports whether a 429 is the upstream's model-wide
// concurrency limit (resellers fronting Tencent GLM: "The request rate
// exceeds the current model Concurrency limit 1200") rather than a
// per-key rate/quota limit. Every account hits the same wall and the
// window clears by itself in seconds, so the account ladder must NOT
// bench keys for it and retries should wait ~1s steps instead of the
// 250ms fast backoff.
func (e *APIError) SharedConcurrency() bool {
	if e == nil || e.Status != 429 {
		return false
	}
	probe := strings.ToLower(e.Code + " " + e.Message)
	return strings.Contains(probe, "concurrency limit")
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
