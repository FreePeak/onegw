// Package translat converts between the unified model (internal/types) and
// the three supported wire formats: OpenAI Chat Completions, Anthropic
// Messages, and Gemini generateContent. Request/response bodies translate via
// full parse; streams translate event-by-event (see stream.go) so memory is
// O(event), never O(conversation).
package translat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"onegw/internal/types"
)

// Format names a wire protocol.
type Format string

const (
	FmtOpenAI    Format = "openai"
	FmtAnthropic Format = "anthropic"
	FmtGemini    Format = "gemini"
	// FmtResponses is the OpenAI Responses API (/v1/responses) — the wire
	// format grok and muse-spark models speak on the OpenCode Zen gateways.
	FmtResponses Format = "openai-responses"
	// Custom wire formats (issue #12).
	FmtCommandCode     Format = "commandcode"           // CommandCode /alpha/generate NDJSON
	FmtOpenAIResponses Format = "openai-responses-grok" // Grok CLI Responses API (distinct from opencode FmtResponses)
	FmtCursor          Format = "cursor"                // Cursor protobuf (skeleton)
)

// ---------------------------------------------------------------------------
// OpenAI Chat Completions wire shapes
// ---------------------------------------------------------------------------
type oaContentText struct {
	Type string `json:"type"` // "text" | "image_url" | ...
	Text string `json:"text,omitempty"`
	// CacheControl preserves an Anthropic-style breakpoint riding on this
	// content part (OpenRouter dialect); nil = absent (issue #32).
	CacheControl *anCacheControl `json:"cache_control,omitempty"`
	// image_url
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
}

type oaToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"` // "function"
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"` // string | []oaContentText | null
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []oaToolCall    `json:"tool_calls,omitempty"`
	// Assistant reasoning echo, vendor shapes (9router extractReasoningText
	// is the shape authority): reasoning_content (DeepSeek/GLM/Qwen/Kimi),
	// reasoning (commandcode /provider/v1 and other AI-SDK resellers — live
	// 2026-09-10: their responses carry BOTH "reasoning" and
	// "reasoning_details"), reasoning_text, and the structured
	// reasoning_details[] array ([{type:"reasoning.text",text:"..."}, ...])
	// pi-ai replays verbatim from those responses. The native
	// reasoning_content wins; aliases are consulted in order and flattened
	// into the unified PartThinking so cross-format paths stop silently
	// dropping the echo the upstream demands back on replay.
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningText    string          `json:"reasoning_text,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

type oaToolDef struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Strict      bool            `json:"strict,omitempty"`
	} `json:"function"`
}

type oaRequest struct {
	Model    string      `json:"model"`
	Messages []oaMessage `json:"messages"`
	Stream   bool        `json:"stream,omitempty"`

	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"` // string | []string
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Tools             []oaToolDef    `json:"tools,omitempty"`
	ToolChoice        any            `json:"tool_choice,omitempty"` // string | {"type","function":{name}}
	ResponseFormat    *oaRespFmt     `json:"response_format,omitempty"`
	ReasoningEffort   string         `json:"reasoning_effort,omitempty"`
	Seed              *int64         `json:"seed,omitempty"`
	User              string         `json:"user,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	FrequencyPenalty  *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64       `json:"presence_penalty,omitempty"`
	LogitBias         map[string]int `json:"logit_bias,omitempty"`
	Logprobs          *bool          `json:"logprobs,omitempty"`
	TopLogprobs       *int           `json:"top_logprobs,omitempty"`
	ServiceTier       string         `json:"service_tier,omitempty"`
	Store             *bool          `json:"store,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	// Cache-affinity knobs (issue #32). Tolerated by the live fleet: b-ai
	// and glm ignore them (200); kilocode/OpenRouter uses them for sticky
	// prompt-cache routing. Forwarded only when the client sent one.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
}

type oaRespFmt struct {
	Type       string `json:"type"` // "json_object" | "json_schema" | "text"
	JSONSchema *struct {
		Name        string          `json:"name"`
		Schema      json.RawMessage `json:"schema"`
		Strict      bool            `json:"strict,omitempty"`
		Description string          `json:"description,omitempty"`
	} `json:"json_schema,omitempty"`
}

// oaUsage mirrors the OpenAI usage object plus common extensions. DeepSeek
// speaks OpenAI chat-completions but reports cache hits as top-level
// prompt_cache_hit_tokens / prompt_cache_miss_tokens (prompt_tokens = hit +
// miss) instead of prompt_tokens_details.cached_tokens (issue #33).
type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
	// DeepSeek cache shape.
	PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
	// Kimi/Moonshot report the cached subset at the top level.
	CachedTokens int64 `json:"cached_tokens,omitempty"`
}

// DecodeOpenAIRequest parses an OpenAI chat-completions body into the unified
// request. Unknown fields are dropped; known passthrough-only fields are kept
// in Metadata for forwarders that care.
func DecodeOpenAIRequest(body []byte) (*types.ChatRequest, error) {
	var req oaRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	u := &types.ChatRequest{
		Model:             req.Model,
		Stream:            req.Stream,
		ReasoningEffort:   req.ReasoningEffort,
		ParallelToolCalls: req.ParallelToolCalls,
	}
	if req.MaxCompletionTokens != nil {
		u.MaxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		u.MaxTokens = *req.MaxTokens
	}
	u.Temperature = req.Temperature
	u.TopP = req.TopP
	switch tc := req.ToolChoice.(type) {
	case string:
		switch tc {
		case "none":
			u.ToolChoice = types.ToolChoiceNone
		case "required":
			u.ToolChoice = types.ToolChoiceAny
		case "auto", "":
			u.ToolChoice = types.ToolChoiceAuto
		}
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				u.ToolChoice = &types.ToolChoiceTool{Name: name}
			}
		}
	}
	if req.ResponseFormat != nil {
		rf := &types.ResponseFormat{}
		switch req.ResponseFormat.Type {
		case "json_object":
			rf.Type = "json_object"
		case "json_schema":
			rf.Type = "json_object" // normalize; schema travels along
			if req.ResponseFormat.JSONSchema != nil {
				rf.Schema = req.ResponseFormat.JSONSchema.Schema
				rf.SchemaName = req.ResponseFormat.JSONSchema.Name
			}
		}
		if rf.Type != "" {
			u.ResponseFormat = rf
		}
	}
	if len(req.Stop) > 0 {
		var stops []string
		if s := string(req.Stop); strings.HasPrefix(s, `"`) {
			var one string
			if err := json.Unmarshal(req.Stop, &one); err == nil {
				stops = []string{one}
			}
		} else if err := json.Unmarshal(req.Stop, &stops); err != nil {
			stops = nil
		}
		u.StopSequences = stops
	}
	if req.User != "" {
		u.SessionID = sessionKey(req.User)
	}
	// Cache-affinity knobs ride through verbatim (issue #32): empty means
	// the client sent none; encoders never invent one.
	u.PromptCacheKey = req.PromptCacheKey
	u.StickySessionID = req.SessionID
	for k, v := range req.Metadata {
		if s, ok := v.(string); ok {
			setMeta(u, k, s)
		}
	}

	tools := make([]types.Tool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, types.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Schema:      t.Function.Parameters,
		})
	}
	if len(tools) > 0 {
		u.Tools = tools
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			u.System = append(u.System, types.Part{Type: types.PartText, Text: flattenOAContent(m.Content)})
		case "user", "function":
			msg := types.Message{Role: types.RoleUser, Name: m.Name}
			msg.Content = decodeOAUserContent(m.Content)
			u.Messages = append(u.Messages, msg)
		case "assistant":
			msg := types.Message{Role: types.RoleAssistant}
			if txt := flattenOAContent(m.Content); txt != "" {
				msg.Content = append(msg.Content, types.Part{Type: types.PartText, Text: txt})
			}
			if r := reasoningEcho(m); r != "" {
				msg.Content = append(msg.Content, types.Part{Type: types.PartThinking, Text: r})
			}
			for _, tc := range m.ToolCalls {
				msg.Content = append(msg.Content, types.Part{
					Type: types.PartToolUse,
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: normalizeArgs(json.RawMessage(tc.Function.Arguments)),
				})
			}
			u.Messages = append(u.Messages, msg)
		case "tool":
			msg := types.Message{
				Role: types.RoleUser,
				Name: m.Name,
			}
			msg.Content = []types.Part{{
				Type:      types.PartToolResult,
				ToolUseID: m.ToolCallID,
				Text:      flattenOAContent(m.Content),
			}}
			u.Messages = append(u.Messages, msg)
		}
	}
	return u, nil
}

func decodeOAUserContent(raw json.RawMessage) []types.Part {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []types.Part{{Type: types.PartText, Text: s}}
	}
	var arr []oaContentText
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	var parts []types.Part
	for _, c := range arr {
		switch c.Type {
		case "text":
			parts = append(parts, types.Part{
				Type:            types.PartText,
				Text:            c.Text,
				CacheBreakpoint: c.CacheControl != nil && c.CacheControl.Type == "ephemeral",
			})
		case "image_url":
			if c.ImageURL == nil {
				continue
			}
			p := types.Part{Type: types.PartImage, URL: c.ImageURL.URL}
			if d, mime, ok := parseDataURL(c.ImageURL.URL); ok {
				p.Data = d
				p.MIMEType = mime
				p.URL = ""
			}
			parts = append(parts, p)
		}
	}
	return parts
}

// flattenOAContent renders string-or-parts OpenAI content as plain text.
func flattenOAContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []oaContentText
	if err := json.Unmarshal(raw, &arr); err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range arr {
		if c.Type == "text" && c.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// reasoningEcho flattens an assistant message's reasoning fields into the
// text the upstream demands back on replay (DeepSeek thinking mode:
// "The `reasoning_content` in the thinking mode must be passed back to the
// API.", commandcode 2026-09-10). reasoning_content is native; reasoning
// and reasoning_text are string aliases; reasoning_details[] is the
// structured array AI SDK clients replay verbatim (9router
// extractReasoningText is the shape authority: {text|content} entries,
// joined). A blank string means the turn carried no reasoning.
func reasoningEcho(m oaMessage) string {
	if r := strings.TrimSpace(m.ReasoningContent); r != "" {
		return r
	}
	if r := strings.TrimSpace(m.Reasoning); r != "" {
		return r
	}
	if r := strings.TrimSpace(m.ReasoningText); r != "" {
		return r
	}
	var details []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Content string `json:"content"`
		Summary string `json:"summary"`
	}
	if len(m.ReasoningDetails) > 0 {
		if err := json.Unmarshal(m.ReasoningDetails, &details); err != nil {
			return ""
		}
		var sb strings.Builder
		for _, d := range details {
			t := d.Text
			if t == "" {
				t = d.Content
			}
			if t == "" && d.Type == "reasoning.summary" {
				t = d.Summary
			}
			if t != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

func parseDataURL(url string) ([]byte, string, bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil, "", false
	}
	rest := url[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil, "", false
	}
	mime := strings.TrimSuffix(rest[:comma], ";base64")
	b, err := base64.StdEncoding.DecodeString(rest[comma+1:])
	if err != nil {
		return nil, "", false
	}
	return b, mime, true
}

func dataURL(p types.Part) string {
	return "data:" + p.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}

// normalizeArgs ensures tool arguments are a JSON object (OpenAI sends a
// string; Anthropic sends an object).
func normalizeArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return json.RawMessage(trimmed)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return json.RawMessage("{}")
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return json.RawMessage("{}")
	}
	if !strings.HasPrefix(s, "{") {
		s = "{}"
	}
	return json.RawMessage(s)
}

func argsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func setMeta(u *types.ChatRequest, k, v string) {
	if u.Metadata == nil {
		u.Metadata = map[string]string{}
	}
	u.Metadata[k] = v
}

// sessionKey reduces arbitrary client user ids to a bounded session key.
func sessionKey(s string) string {
	if len(s) <= 64 {
		return s
	}
	return s[:64]
}

// EncodeOpenAIRequest renders the unified request as an OpenAI
// chat-completions body.
func EncodeOpenAIRequest(u *types.ChatRequest) ([]byte, error) {
	req := oaRequest{
		Model:             u.Model,
		Stream:            u.Stream,
		ReasoningEffort:   u.ReasoningEffort,
		Temperature:       u.Temperature,
		TopP:              u.TopP,
		ParallelToolCalls: u.ParallelToolCalls,
		// Cache-affinity knobs: re-emit only what the client sent (issue
		// #32); omitempty drops absent fields, never invented.
		PromptCacheKey: u.PromptCacheKey,
		SessionID:      u.StickySessionID,
	}
	if u.MaxTokens > 0 {
		mt := u.MaxTokens
		req.MaxCompletionTokens = &mt
	}
	switch tc := u.ToolChoice.(type) {
	case types.ToolChoiceMode:
		switch tc {
		case types.ToolChoiceNone:
			req.ToolChoice = "none"
		case types.ToolChoiceAny:
			req.ToolChoice = "required"
		default:
			req.ToolChoice = "auto"
		}
	case *types.ToolChoiceTool:
		req.ToolChoice = map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}
	}
	if u.ResponseFormat != nil {
		switch u.ResponseFormat.Type {
		case "json_object":
			req.ResponseFormat = &oaRespFmt{Type: "json_object"}
		case "json_schema":
			req.ResponseFormat = &oaRespFmt{
				Type: "json_schema",
				JSONSchema: &struct {
					Name        string          `json:"name"`
					Schema      json.RawMessage `json:"schema"`
					Strict      bool            `json:"strict,omitempty"`
					Description string          `json:"description,omitempty"`
				}{Name: orDefault(u.ResponseFormat.SchemaName, "response"), Schema: u.ResponseFormat.Schema},
			}
		}
	}
	if len(u.StopSequences) == 1 {
		req.Stop, _ = json.Marshal(u.StopSequences[0])
	} else if len(u.StopSequences) > 1 {
		req.Stop, _ = json.Marshal(u.StopSequences)
	}
	for _, p := range u.System {
		if p.CacheBreakpoint {
			// A marked system block must keep its part shape so the
			// cache_control marker has something to ride on (issue #32).
			req.Messages = append(req.Messages, oaMessage{
				Role:    "system",
				Content: mustJSON([]oaContentText{{Type: "text", Text: p.Text, CacheControl: cacheControlOf(p)}}),
			})
			continue
		}
		req.Messages = append(req.Messages, oaMessage{Role: "system", Content: mustJSON(p.Text)})
	}
	for _, m := range u.Messages {
		switch m.Role {
		case types.RoleSystem:
			req.Messages = append(req.Messages, oaMessage{Role: "system", Content: mustJSON(m.FlattenText())})
		case types.RoleUser:
			// OpenAI models a tool result as its own role:"tool" message;
			// split mixed user turns so tool_result parts become tool msgs.
			var toolResults []types.Part
			var plain []types.Part
			for _, p := range m.Content {
				if p.Type == types.PartToolResult {
					toolResults = append(toolResults, p)
				} else {
					plain = append(plain, p)
				}
			}
			if len(plain) > 0 {
				oc := oaMessage{Role: "user", Name: m.Name}
				if content := encodeOAUserContent(plain); content != nil {
					oc.Content = content
				} else {
					oc.Content = mustJSON("")
				}
				req.Messages = append(req.Messages, oc)
			}
			for _, tr := range toolResults {
				req.Messages = append(req.Messages, oaMessage{
					Role:       "tool",
					ToolCallID: orDefault(tr.ToolUseID, orDefault(m.ToolCallID, m.Name)),
					Content:    mustJSON(tr.Text),
				})
			}
			if len(plain) == 0 && len(toolResults) == 0 {
				req.Messages = append(req.Messages, oaMessage{Role: "user", Name: m.Name, Content: mustJSON("")})
			}
		case types.RoleAssistant:
			om := oaMessage{Role: "assistant"}
			var texts []string
			var reasoning string
			for _, p := range m.Content {
				switch p.Type {
				case types.PartText:
					texts = append(texts, p.Text)
				case types.PartThinking:
					if reasoning != "" {
						reasoning += "\n"
					}
					reasoning += p.Text
				case types.PartToolUse:
					om.ToolCalls = append(om.ToolCalls, oaToolCall{
						ID:   orDefault(p.ID, "call_"+p.Name),
						Type: "function",
						Function: struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						}{Name: p.Name, Arguments: argsString(p.Args)},
					})
				}
			}
			if reasoning != "" {
				om.ReasoningContent = reasoning
			}
			if len(texts) == 1 {
				om.Content = mustJSON(texts[0])
			} else if len(texts) > 1 {
				om.Content = mustJSON(strings.Join(texts, "\n"))
			}
			req.Messages = append(req.Messages, om)
		}
	}
	if len(u.Tools) > 0 {
		for _, t := range u.Tools {
			td := oaToolDef{Type: "function"}
			td.Function.Name = t.Name
			td.Function.Description = t.Description
			td.Function.Parameters = t.Schema
			req.Tools = append(req.Tools, td)
		}
		if req.ToolChoice == nil {
			req.ToolChoice = "auto"
		}
	}
	if u.Stream {
		req.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	return json.Marshal(req)
}

func encodeOAUserContent(parts []types.Part) json.RawMessage {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 && parts[0].Type == types.PartText && !parts[0].CacheBreakpoint {
		return mustJSON(parts[0].Text)
	}
	arr := make([]oaContentText, 0, len(parts))
	texts := true
	for _, p := range parts {
		switch p.Type {
		case types.PartText:
			arr = append(arr, oaContentText{
				Type:         "text",
				Text:         p.Text,
				CacheControl: cacheControlOf(p),
			})
		case types.PartImage:
			texts = false
			c := oaContentText{Type: "image_url"}
			c.ImageURL = &struct {
				URL    string `json:"url"`
				Detail string `json:"detail,omitempty"`
			}{URL: p.URL}
			if p.URL == "" && len(p.Data) > 0 {
				c.ImageURL.URL = dataURL(p)
			}
			arr = append(arr, c)
		default:
			texts = false
		}
	}
	if !texts && len(arr) == 0 {
		return nil
	}
	return mustJSON(arr)
}

func isToolResultMsg(m *types.Message) bool {
	for _, p := range m.Content {
		if p.Type == types.PartToolResult {
			return true
		}
	}
	return false
}

// cacheControlOf converts a unified part's breakpoint flag into the wire
// marker for dialects that accept Anthropic-style cache_control on content
// parts (OpenRouter); nil = absent, never invented (issue #32).
func cacheControlOf(p types.Part) *anCacheControl {
	if p.CacheBreakpoint {
		return &anCacheControl{Type: "ephemeral"}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------------------
// OpenAI responses
// ---------------------------------------------------------------------------

type oaResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int       `json:"index"`
		FinishReason string    `json:"finish_reason"`
		Message      oaMessage `json:"message"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *oaError `json:"error,omitempty"`
}

type oaError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// DecodeOpenAIResponse converts an OpenAI completion response to unified.
func DecodeOpenAIResponse(body []byte) (*types.ChatResponse, error) {
	var r oaResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai response: %w", err)
	}
	if r.Error != nil {
		return nil, NormalizeInStreamError(&types.APIError{
			Status:  statusFromOAErr(r.Error.Code, r.Error.Type, r.Error.Message),
			Type:    orDefault(r.Error.Type, "upstream_error"),
			Code:    errCodeString(r.Error.Code),
			Message: r.Error.Message,
		})
	}
	out := &types.ChatResponse{ID: r.ID, Model: r.Model}
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		out.StopReason = mapOAStop(c.FinishReason)
		m := c.Message
		if txt := flattenOAContent(m.Content); txt != "" {
			out.Content = append(out.Content, types.Part{Type: types.PartText, Text: txt})
		}
		if m.ReasoningContent != "" {
			out.Content = append(out.Content, types.Part{Type: types.PartThinking, Text: m.ReasoningContent})
		}
		for _, tc := range m.ToolCalls {
			out.Content = append(out.Content, types.Part{
				Type: types.PartToolUse,
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: normalizeArgs(json.RawMessage(tc.Function.Arguments)),
			})
		}
	}
	if r.Usage != nil {
		out.Usage = oaUsageToUnified(r.Usage)
	}
	return out, nil
}

func errCodeString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func mapOAStop(s string) string {
	switch s {
	case "stop", "":
		return types.StopEndTurn
	case "length":
		return types.StopMaxTokens
	case "tool_calls", "function_call":
		return types.StopToolUse
	case "content_filter":
		return types.StopContentFilter
	default:
		return types.StopEndTurn
	}
}

func unmapOAStop(s string) string {
	switch s {
	case types.StopMaxTokens:
		return "length"
	case types.StopToolUse:
		return "tool_calls"
	case types.StopContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

func oaUsageToUnified(u *oaUsage) types.Usage {
	out := types.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, UpstreamFormat: string(FmtOpenAI)}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	// DeepSeek shape: prompt_tokens = hit + miss; the hit is the cached
	// subset (unified InputTokens is already inclusive upstream).
	if out.CacheReadTokens == 0 && u.PromptCacheHitTokens > 0 {
		out.CacheReadTokens = u.PromptCacheHitTokens
	}
	if out.CacheReadTokens == 0 {
		out.CacheReadTokens = u.CachedTokens // Kimi top level
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// EncodeOpenAIResponse renders unified response as OpenAI completion JSON.
func EncodeOpenAIResponse(r *types.ChatResponse) ([]byte, error) {
	resp := oaResponse{
		ID:      orDefault(r.ID, "chatcmpl-onegw"),
		Object:  "chat.completion",
		Created: nowUnix(),
		Model:   r.Model,
	}
	msg := oaMessage{Role: "assistant"}
	var texts []string
	for _, p := range r.Content {
		switch p.Type {
		case types.PartText:
			texts = append(texts, p.Text)
		case types.PartThinking:
			msg.ReasoningContent += p.Text
		case types.PartToolUse:
			msg.ToolCalls = append(msg.ToolCalls, oaToolCall{
				ID:   orDefault(p.ID, "call_"+p.Name),
				Type: "function",
				Function: struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Name: p.Name, Arguments: argsString(p.Args)},
			})
		}
	}
	if len(texts) > 0 {
		msg.Content = mustJSON(strings.Join(texts, "\n"))
	}
	finish := "stop"
	switch r.StopReason {
	case types.StopMaxTokens:
		finish = "length"
	case types.StopToolUse:
		finish = "tool_calls"
	case types.StopContentFilter:
		finish = "content_filter"
	}
	resp.Choices = append(resp.Choices, struct {
		Index        int       `json:"index"`
		FinishReason string    `json:"finish_reason"`
		Message      oaMessage `json:"message"`
	}{Index: 0, FinishReason: finish, Message: msg})
	resp.Usage = &oaUsage{
		PromptTokens:     r.Usage.InputTokens,
		CompletionTokens: r.Usage.OutputTokens,
		TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
	}
	if r.Usage.CacheReadTokens > 0 {
		resp.Usage.PromptTokensDetails = &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{CachedTokens: r.Usage.CacheReadTokens}
	}
	if r.Usage.ReasoningTokens > 0 {
		resp.Usage.CompletionTokensDetails = &struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		}{ReasoningTokens: r.Usage.ReasoningTokens}
	}
	return json.Marshal(resp)
}

// DecodeOpenAIError parses an OpenAI error payload.
func DecodeOpenAIError(body []byte, status int) *types.APIError {
	var e struct {
		Error oaError `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Message == "" {
		return &types.APIError{Status: status, Type: "upstream_error", Message: strings.TrimSpace(string(body))}
	}
	return &types.APIError{Status: status, Type: orDefault(e.Error.Type, "upstream_error"), Code: errCodeString(e.Error.Code), Message: e.Error.Message}
}

// EncodeError renders a unified error in the wire format expected by the
// client surface.
func EncodeError(f Format, e *types.APIError) []byte {
	if e == nil {
		e = &types.APIError{Status: 500, Type: "internal", Message: "unknown error"}
	}
	switch f {
	case FmtAnthropic:
		b, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": mapAnthropicErrType(e), "message": e.Message},
		})
		return b
	case FmtGemini:
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"code": e.Status, "message": e.Message, "status": mapGeminiErrStatus(e)},
		})
		return b
	default:
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": e.Message,
				"type":    orDefault(e.Type, "onegw_error"),
				"code":    orDefault(e.Code, strconv.Itoa(e.Status)),
			},
		})
		return b
	}
}

func mapAnthropicErrType(e *types.APIError) string {
	if e.OverQuota() {
		return "rate_limit_error"
	}
	switch e.Status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 408, 504:
		return "timeout_error"
	case 500, 502, 503, 529:
		return "api_error"
	default:
		return "api_error"
	}
}

func mapGeminiErrStatus(e *types.APIError) string {
	switch e.Status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401, 403:
		return "UNAUTHENTICATED"
	case 404:
		return "NOT_FOUND"
	case 408, 504:
		return "DEADLINE_EXCEEDED"
	case 429, 529:
		return "RESOURCE_EXHAUSTED"
	case 500:
		return "INTERNAL"
	case 502, 503:
		return "UNAVAILABLE"
	default:
		return "UNKNOWN"
	}
}
