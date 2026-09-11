// Package types defines the unified internal request/response model shared by
// every wire-format translator. Wire formats (OpenAI, Anthropic, Gemini) parse
// into this model and re-encode from it; nothing else needs to know the wire
// shapes. Passthrough paths bypass this model entirely for zero-copy speed.
package types

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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

	// SharedWall marks a 429 the provider proved shared by BEHAVIOUR
	// rather than wording: a second distinct account struck the same
	// (provider, model) inside the burst window while the error carried
	// no Retry-After, no request-count window, and no limit text to
	// match (b-ai's empty-body one-api 429s). SharedConcurrency()
	// reports it, so every consumer — Router.Execute's immediate
	// fall-through to the next combo leg, the Retry-After stamping, the
	// flap-breaker exemption — treats it exactly like the text-matched
	// model walls. Never serialized.
	SharedWall bool `json:"-"`
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
	case 520, 521, 522, 523, 524, 525, 526, 527:
		// Cloudflare 52x family: the edge could not get a usable answer
		// from the origin (520 unknown error, 521 down, 522/523 connect
		// fail, 524 origin timeout, 525/526 TLS, 527 edge fetch). commandcode
		// answers these while ITS upstream model provider flaps — verbatim
		// live evidence (2026-09-09 15:35): 520 "Upstream model provider is
		// temporarily unavailable. Please try again in a moment." (type
		// server_error) — a transient fault that must not be terminal. Treat
		// the whole family as retryable so Router.Execute retries the target
		// and combos fall through to the next one.
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

// ModelScoped reports whether the upstream refusal indicts the MODEL on
// this provider rather than the credential or load — the (provider, model)
// pair cannot serve, but healthy sibling models on the same account can:
//   - 403 refusals that NAME the model: Zhipu's per-model
//     "model_access_denied" ("Model access denied for model x", live
//     2026-09-09), the "model access" message family. The b-ai deposit
//     gate ("access_denied" + "Deposit required") is deliberately NOT
//     model-scoped: it is per-CREDENTIAL state with its own pinned
//     contract (#48) — the next request must reach the pool's healthy
//     key, and the model bench would over-lock the model for 5 minutes.
//   - 404 model-not-found shapes: OpenAI's code "model_not_found", the
//     spelled-out "model not found" message.
//
// Deliberately conservative: 429s are NEVER model-scoped (they are per-key
// walls — the account ladder owns them, byte-identical to the pre-lockout
// behavior), and a generic 404 "not found" (wrong path, dead route) is
// not a model verdict. Callers bench the model for a short window so the
// router skips the target without burning the account pool.
func (e *APIError) ModelScoped() bool {
	if e == nil {
		return false
	}
	probe := strings.ToLower(e.Type + " " + e.Code + " " + e.Message)
	switch e.Status {
	case 403:
		return strings.Contains(probe, "model_access") ||
			strings.Contains(probe, "model access")
	case 404:
		return strings.Contains(probe, "model_not_found") ||
			strings.Contains(probe, "model not found")
	}
	return false
}

// reasoningEchoRe matches the DeepSeek thinking-mode history contract:
// a replayed assistant turn must carry the reasoning the model emitted.
// Live payload (commandcode/deepseek/deepseek-v4-flash 2026-09-10, seqs
// 2455/2621/2812): 400 "The `reasoning_content` in the thinking mode must
// be passed back to the API." wrapped by the reseller's AI SDK as type
// AI_APICallError.
var reasoningEchoRe = regexp.MustCompile(`(?i)reasoning_content.{0,80}must be passed back|must be passed back.{0,80}reasoning_content`)

// ReasoningEchoRequired reports whether a 400 is the upstream's
// thinking-mode reasoning-echo refusal: the request's replayed assistant
// history does not satisfy the model's reasoning_content contract. The
// verdict indicts THIS target's contract with the client's body, not the
// credential or load — the body is deterministic, so same-target retries
// are pointless; a sibling combo target serving the same model family
// (opencode/deepseek-v4-flash, tokenharbor) is the right next hop.
// Deliberately narrow: only the exact echo-demand wording matches, so
// the GLM reasoning_effort 400 family ("use low, high or max") and
// generic invalid_request 400s keep their terminal contract.
func (e *APIError) ReasoningEchoRequired() bool {
	return e != nil && e.Status == 400 && reasoningEchoRe.MatchString(e.Message)
}

// SharedConcurrency reports whether a 429 is the upstream's model-wide
// concurrency limit (resellers fronting Tencent GLM: "The request rate
// exceeds the current model Concurrency limit 1200") rather than a
// per-key rate/quota limit. Every account hits the same wall and the
// window clears by itself in seconds, so the account ladder must NOT
// bench keys for it and retries should wait ~1s steps instead of the
// 250ms fast backoff.
//
// The same family covers every "current model X limit" wall the reseller
// fronts — live 2026-09-10 11:10-11:15 (b-ai/glm-5.3-flash): "The request
// rate exceeds the current model TPM limit 340000000." struck kisame,
// linh.mn and harvey in the SAME second while clone3 served 200s on the
// same model — the budget is the reseller's per-MODEL lane (Tencent
// APPID-wide), not any one key's. Misclassifying it as per-key benched
// healthy credentials on the 10-60s ladder and burned a same-target retry
// into the saturated lane per request; both must ride the shared-wall
// medicine instead.
//
// Also matches engine cold-prefill admission walls (new-api aggregators
// fronting z-ai/SGLang: 429 "BackendAdmissionRejected: Engine
// cold-request admission rejected … policies=prefill_pressure …", live
// 2026-09-09): the budget is the ENGINE's outstanding-uncached-prefill
// pool shared by all traffic, and the same request body succeeds seconds
// later on a different account — the credential is innocent. 503
// variants ("cache-only admission rejected a cold, unavailable, or
// overloaded request", "gateway overloaded: hard concurrency limit
// reached", "gateway overloaded: cache-aware admission is unavailable")
// carry the same medicine and are matched here too: Retryable() already
// lets 503s fall through, but the ladder and the Retry-After stamping
// must also treat them as shared-wall, not per-key load.
//
// A behaviour-proven SharedWall (provider burst detection on a
// wording-less 429) short-circuits the text matching entirely.
func (e *APIError) SharedConcurrency() bool {
	if e == nil {
		return false
	}
	if e.SharedWall {
		// No wording needed — see the SharedWall field doc.
		return true
	}
	if e.Status == 503 {
		probe := strings.ToLower(e.Code + " " + e.Message)
		return strings.Contains(probe, "cache-only admission") ||
			strings.Contains(probe, "gateway overloaded")
	}
	if e.Status != 429 {
		return false
	}
	probe := strings.ToLower(e.Code + " " + e.Message)
	return strings.Contains(probe, "concurrency limit") ||
		modelLimitRe.MatchString(e.Code+" "+e.Message) ||
		strings.Contains(probe, "backendadmissionrejected") ||
		strings.Contains(probe, "cold-request admission rejected")
}

// ModelWall reports whether the error NAMES a model-scoped upstream
// budget — the reseller's "current model <X> limit <N>" family or a bare
// "Concurrency limit <N>" — a wall shared by every key fronting the
// model, so the (provider, model) pair can be parked for one burst
// window instead of every request re-discovering the wall. Engine
// admission walls ("BackendAdmissionRejected", "cache-only admission",
// "cold-request admission", "gateway overloaded") are per-request
// prefill-shape rejections and deliberately excluded: other requests
// keep serving the same model, so parking it would skip servable
// capacity.
func (e *APIError) ModelWall() bool {
	if e == nil {
		return false
	}
	probe := strings.ToLower(e.Code + " " + e.Message)
	if strings.Contains(probe, "admission") || strings.Contains(probe, "gateway overloaded") {
		return false
	}
	return strings.Contains(probe, "concurrency limit") || modelLimitRe.MatchString(e.Code+" "+e.Message)
}

// modelLimitRe matches the Tencent reseller's model-limit wall family —
// "The request rate exceeds the current model <X> limit <N>" with X ∈
// {Concurrency, TPM, RPM, …} (b-ai/glm-5.3-flash live 2026-09-10). The
// message names the MODEL's budget, never the key, so every variant is a
// shared wall.
var modelLimitRe = regexp.MustCompile(`(?i)request rate exceeds the current model \w+ limit`)

// rateWindowRe matches the request-count window a 429 body names — the
// new-api aggregator family, live 2026-09-09 (tokenrouter/z-ai/glm-5.3-free):
// "You have reached the request limit[z-ai/glm-5.3-free]: Maximum 8 requests
// within 1 minutes." The stated window is the honest account bench and
// client Retry-After for a request-count limit: the default 10s ladder base
// re-enters the still-closed window (ring evidence: 429 at :46, ladder retry
// at :57 429s again, success only ~30-40s later).
var rateWindowRe = regexp.MustCompile(`(?i)maximum\s+\d+\s+requests?\s+within\s+(\d+)\s+(second|minute|hour)s?`)

// RateWindow reports the request-count window the upstream's rate-limit
// message names ("Maximum 8 requests within 1 minutes" → 1 minute), or 0
// when the message carries no such window. Consulted for OverQuota 429s
// when no Retry-After header exists: bench the account for the stated
// window and stamp it as the client Retry-After instead of guessing 10s.
func (e *APIError) RateWindow() time.Duration {
	if e == nil {
		return 0
	}
	m := rateWindowRe.FindStringSubmatch(e.Code + " " + e.Message)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	switch m[2] {
	case "second":
		return time.Duration(n) * time.Second
	case "minute":
		return time.Duration(n) * time.Minute
	default: // "hour"
		return time.Duration(n) * time.Hour
	}
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
