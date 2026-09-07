package translat

import (
	"encoding/json"
	"fmt"
	"strings"

	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// OpenAI Responses API wire shapes (opencode zen/go grok + muse-spark)
// ---------------------------------------------------------------------------

// Format added to the enum in openai.go via FmtResponses.

// rsItem is one item of the Responses `input`/`output` array.
type rsItem struct {
	Type string `json:"type"`

	// type "message"
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // string | []rsContent

	// type "function_call" (assistant tool invocation)
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// type "function_call_output" (tool result)
	Output string `json:"output,omitempty"`

	// type "reasoning"
	Summary []rsSummary `json:"summary,omitempty"`

	// type "item_reference"
	ID string `json:"id,omitempty"`
}

type rsSummary struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type rsContent struct {
	Type string `json:"type"` // "input_text" | "output_text" | "input_image" | ...
	Text string `json:"text,omitempty"`
	// input_image
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type rsToolDef struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type rsToolChoiceFunc struct {
	Name string `json:"name"`
}

type rsUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokens        int64 `json:"output_tokens"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
	TotalTokens int64 `json:"total_tokens"`
	// Legacy aliases some upstreams emit instead of the canonical fields
	// (mirrors 9router's fallbacks).
	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

type rsRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Store           bool            `json:"store"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Tools           []rsToolDef     `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	ParallelTool    *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning       *rsReasoning    `json:"reasoning,omitempty"`
	Text            *rsTextFmt      `json:"text,omitempty"`
}

type rsReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type rsTextFmt struct {
	Format *struct {
		Type   string          `json:"type"`
		Name   string          `json:"name,omitempty"`
		Schema json.RawMessage `json:"schema,omitempty"`
		Strict bool            `json:"strict,omitempty"`
	} `json:"format,omitempty"`
}

// rsResponse is the non-streaming Responses reply.
type rsResponse struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"` // completed | incomplete | failed | in_progress
	IncompleteDetails *struct {
		Reason string `json:"reason"` // max_output_tokens | content_filter
	} `json:"incomplete_details,omitempty"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Output []rsItem `json:"output"`
	Usage  *rsUsage `json:"usage"`
}

// ---------------------------------------------------------------------------
// Requests: unified -> Responses wire (one direction: onegw has no
// /v1/responses client surface, so only the upstream encoder exists)
// ---------------------------------------------------------------------------

func decodeRSContent(raw json.RawMessage) []types.Part {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		if plain == "" {
			return nil
		}
		return []types.Part{{Type: types.PartText, Text: plain}}
	}
	var arr []rsContent
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	var parts []types.Part
	for _, c := range arr {
		switch c.Type {
		case "input_image":
			data, mime, ok := parseDataURL(c.ImageURL)
			if ok {
				parts = append(parts, types.Part{Type: types.PartImage, MIMEType: mime, Data: data})
			} else if c.ImageURL != "" {
				parts = append(parts, types.Part{Type: types.PartImage, URL: c.ImageURL})
			}
		default: // "input_text", "output_text", anything text-ish
			if c.Text != "" {
				parts = append(parts, types.Part{Type: types.PartText, Text: c.Text})
			}
		}
	}
	return parts
}

// EncodeResponsesRequest renders the unified request as a Responses body.
// Assistant thinking is NOT echoed back (Responses reasoning items are
// upstream-internal); tool flow uses function_call / function_call_output
// items.
func EncodeResponsesRequest(u *types.ChatRequest) ([]byte, error) {
	req := rsRequest{
		Model:        u.Model,
		Stream:       u.Stream,
		Store:        false, // never persist conversation state upstream
		Temperature:  u.Temperature,
		TopP:         u.TopP,
		ParallelTool: u.ParallelToolCalls,
	}
	if u.MaxTokens > 0 {
		mt := u.MaxTokens
		req.MaxOutputTokens = &mt
	}
	// reasoning.effort: pass the values Responses accepts verbatim
	// (minimal|low|medium|high); ""/none mean "no knob" — omit reasoning
	// entirely (knobs are never invented). Budget-derived effort only
	// fills a gap: an explicit effort is never overridden.
	if eff := u.ReasoningEffort; eff != "" && eff != "none" {
		if eff == "max" || eff == "xhigh" {
			req.Reasoning = &rsReasoning{Effort: "high", Summary: "auto"}
		} else {
			req.Reasoning = &rsReasoning{Effort: eff, Summary: "auto"}
		}
	} else if u.ReasoningEffort == "" && u.Thinking != nil && u.Thinking.BudgetTokens > 0 {
		req.Reasoning = &rsReasoning{Effort: budgetToEffort(u.Thinking.BudgetTokens), Summary: "auto"}
	}
	var sb strings.Builder
	for _, p := range u.System {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	if t := encodeRSTextFormat(u.ResponseFormat); t != nil {
		req.Text = t
	}
	if sb.Len() > 0 {
		req.Instructions = sb.String()
	}

	items := make([]rsItem, 0, len(u.Messages))
	for i := range u.Messages {
		m := &u.Messages[i]
		switch m.Role {
		case types.RoleSystem:
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(m.FlattenText())
		case types.RoleUser:
			var texts []rsContent
			for _, p := range m.Content {
				switch p.Type {
				case types.PartText:
					texts = append(texts, rsContent{Type: "input_text", Text: p.Text})
				case types.PartImage:
					if p.Data != nil {
						texts = append(texts, rsContent{Type: "input_image", ImageURL: dataURL(p), Detail: "auto"})
					} else if p.URL != "" {
						texts = append(texts, rsContent{Type: "input_image", ImageURL: p.URL, Detail: "auto"})
					}
				case types.PartToolResult:
					items = append(items, rsItem{
						Type:   "function_call_output",
						CallID: orDefault(p.ToolUseID, orDefault(m.ToolCallID, m.Name)),
						Output: p.Text,
					})
				}
			}
			if len(texts) > 0 {
				b, err := json.Marshal(texts)
				if err != nil {
					return nil, err
				}
				items = append(items, rsItem{Type: "message", Role: "user", Content: b})
			}
			if len(texts) == 0 && !hasToolResult(m) {
				b, _ := json.Marshal([]rsContent{{Type: "input_text", Text: ""}})
				items = append(items, rsItem{Type: "message", Role: "user", Content: b})
			}
		case types.RoleAssistant:
			var texts []rsContent
			for _, p := range m.Content {
				switch p.Type {
				case types.PartText:
					texts = append(texts, rsContent{Type: "output_text", Text: p.Text})
				case types.PartToolUse:
					items = append(items, rsItem{
						Type:      "function_call",
						CallID:    orDefault(p.ID, "call_"+p.Name),
						Name:      p.Name,
						Arguments: argsString(p.Args),
					})
					// PartThinking deliberately dropped: see doc comment.
				}
			}
			if len(texts) > 0 {
				b, err := json.Marshal(texts)
				if err != nil {
					return nil, err
				}
				items = append(items, rsItem{Type: "message", Role: "assistant", Content: b})
			}
		}
	}
	// System text gathered mid-loop lands in instructions.
	if sb.Len() > 0 && req.Instructions == "" {
		req.Instructions = sb.String()
	}
	req.Input = marshalRSInput(items)
	req.Tools = encodeRSTools(u.Tools)
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
		req.ToolChoice = map[string]any{"type": "function", "function": rsToolChoiceFunc{Name: tc.Name}}
	}
	return json.Marshal(req)
}

func hasToolResult(m *types.Message) bool {
	for _, p := range m.Content {
		if p.Type == types.PartToolResult {
			return true
		}
	}
	return false
}

func marshalRSInput(items []rsItem) json.RawMessage {
	if len(items) == 0 {
		b, _ := json.Marshal([]rsItem{{Type: "message", Role: "user", Content: mustJSON([]rsContent{{Type: "input_text", Text: "..."}})}})
		return b
	}
	b, err := json.Marshal(items)
	if err != nil {
		return mustJSON("...")
	}
	return b
}

func encodeRSTools(defs []types.Tool) []rsToolDef {
	if len(defs) == 0 {
		return nil
	}
	out := make([]rsToolDef, 0, len(defs))
	for _, t := range defs {
		out = append(out, rsToolDef{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		})
	}
	return out
}

func encodeRSTextFormat(rf *types.ResponseFormat) *rsTextFmt {
	if rf == nil {
		return nil
	}
	switch rf.Type {
	case "json_object":
		return &rsTextFmt{Format: &struct {
			Type   string          `json:"type"`
			Name   string          `json:"name,omitempty"`
			Schema json.RawMessage `json:"schema,omitempty"`
			Strict bool            `json:"strict,omitempty"`
		}{Type: "json_object"}}
	case "json_schema":
		return &rsTextFmt{Format: &struct {
			Type   string          `json:"type"`
			Name   string          `json:"name,omitempty"`
			Schema json.RawMessage `json:"schema,omitempty"`
			Strict bool            `json:"strict,omitempty"`
		}{Type: "json_schema", Name: orDefault(rf.SchemaName, "response"), Schema: rf.Schema, Strict: true}}
	default:
		return nil
	}
}

func budgetToEffort(budget int) string {
	switch {
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// ---------------------------------------------------------------------------
// Responses: unified -> wire (non-streaming reply)
// ---------------------------------------------------------------------------

// DecodeResponsesResponse parses a Responses reply into the unified response.
func DecodeResponsesResponse(body []byte) (*types.ChatResponse, error) {
	var r rsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("responses response: %w", err)
	}
	out := &types.ChatResponse{ID: r.ID, Model: r.Model}
	if r.Status == "failed" {
		msg := "responses request failed"
		if r.Error != nil {
			msg = r.Error.Message
		}
		return nil, fmt.Errorf("responses status %s: %s", r.Status, msg)
	}
	var texts []string
	for i := range r.Output {
		it := &r.Output[i]
		switch it.Type {
		case "message":
			for _, c := range decodeRSContent(it.Content) {
				if c.Type == types.PartText && c.Text != "" {
					texts = append(texts, c.Text)
				}
			}
		case "function_call":
			out.Content = append(out.Content, types.Part{
				Type: types.PartToolUse,
				ID:   it.CallID,
				Name: it.Name,
				Args: json.RawMessage(orDefault(it.Arguments, "{}")),
			})
		case "reasoning":
			for _, s := range it.Summary {
				if s.Text != "" {
					out.Content = append(out.Content, types.Part{Type: types.PartThinking, Text: s.Text})
				}
			}
		}
	}
	if len(texts) > 0 {
		out.Content = append(out.Content, types.Part{Type: types.PartText, Text: strings.Join(texts, "\n")})
	}
	out.StopReason = mapRSStop(r.Status, r.IncompleteDetails)
	if r.Usage != nil {
		out.Usage = rsUsageToUnified(r.Usage)
	}
	return out, nil
}

func mapRSStop(status string, inc *struct {
	Reason string `json:"reason"`
}) string {
	if status == "incomplete" && inc != nil {
		switch inc.Reason {
		case "max_output_tokens":
			return types.StopMaxTokens
		case "content_filter":
			return types.StopContentFilter
		}
	}
	return types.StopEndTurn
}

func rsUsageToUnified(u *rsUsage) types.Usage {
	in := u.InputTokens
	if in == 0 {
		in = u.PromptTokens // legacy alias
	}
	out := u.OutputTokens
	if out == 0 {
		out = u.CompletionTokens // legacy alias
	}
	unified := types.Usage{InputTokens: in, OutputTokens: out, UpstreamFormat: string(FmtResponses)}
	if u.InputTokensDetails != nil {
		unified.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	if unified.CacheReadTokens == 0 {
		unified.CacheReadTokens = u.CacheReadInputTokens // legacy alias
	}
	if u.OutputTokensDetails != nil {
		unified.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return unified
}
