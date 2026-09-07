package translat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// Anthropic Messages wire shapes
// ---------------------------------------------------------------------------

type anBlock struct {
	Type string `json:"type"` // text | image | tool_use | tool_result | thinking

	Text string `json:"text,omitempty"`

	Source *struct {
		Type      string `json:"type"` // base64 | url
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string | []anBlock
	IsError   bool            `json:"is_error,omitempty"`

	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type anMessage struct {
	Role    string          `json:"role"`    // user | assistant
	Content json.RawMessage `json:"content"` // string | []anBlock
}

type anRequest struct {
	Model     string          `json:"model"`
	Messages  []anMessage     `json:"messages"`
	System    json.RawMessage `json:"system,omitempty"` // string | []anBlock
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream,omitempty"`

	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`

	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools,omitempty"`
	ToolChoice *struct {
		Type string `json:"type"` // auto | any | tool | none
		Name string `json:"name,omitempty"`
	} `json:"tool_choice,omitempty"`

	Thinking *struct {
		Type         string `json:"type"` // enabled | disabled
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking,omitempty"`

	Metadata *struct {
		UserID string `json:"user_id,omitempty"`
	} `json:"metadata,omitempty"`
}

// DecodeAnthropicRequest parses an Anthropic Messages body into unified form.
func DecodeAnthropicRequest(body []byte) (*types.ChatRequest, error) {
	var req anRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	u := &types.ChatRequest{
		Model:         req.Model,
		Stream:        req.Stream,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.StopSequences,
	}
	if req.System != nil && string(req.System) != "null" {
		var blocks []anBlock
		if err := json.Unmarshal(req.System, &blocks); err != nil {
			var s string
			if err2 := json.Unmarshal(req.System, &s); err2 == nil {
				u.System = []types.Part{{Type: types.PartText, Text: s}}
			}
		} else {
			for _, b := range blocks {
				if b.Type == "text" {
					u.System = append(u.System, types.Part{Type: types.PartText, Text: b.Text})
				}
			}
		}
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "none":
			u.ToolChoice = types.ToolChoiceNone
		case "any":
			u.ToolChoice = types.ToolChoiceAny
		case "tool":
			u.ToolChoice = &types.ToolChoiceTool{Name: req.ToolChoice.Name}
		default:
			u.ToolChoice = types.ToolChoiceAuto
		}
	}
	if req.Thinking != nil && req.Thinking.Type == "enabled" && req.Thinking.BudgetTokens > 0 {
		u.Thinking = &types.ThinkingCfg{BudgetTokens: req.Thinking.BudgetTokens}
	}
	if req.Metadata != nil && req.Metadata.UserID != "" {
		u.SessionID = sessionKey(req.Metadata.UserID)
	}
	for _, t := range req.Tools {
		u.Tools = append(u.Tools, types.Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	for _, m := range req.Messages {
		var blocks []anBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				continue
			}
			blocks = []anBlock{{Type: "text", Text: s}}
		}
		msg := types.Message{Role: m.Role}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				msg.Content = append(msg.Content, types.Part{Type: types.PartText, Text: b.Text})
			case "image":
				p := types.Part{Type: types.PartImage}
				if b.Source != nil {
					if b.Source.Type == "base64" {
						p.MIMEType = b.Source.MediaType
						p.Data = decodeBase64Loose(b.Source.Data)
					} else {
						p.URL = b.Source.URL
					}
				}
				msg.Content = append(msg.Content, p)
			case "tool_use":
				msg.Content = append(msg.Content, types.Part{
					Type: types.PartToolUse, ID: b.ID, Name: b.Name, Args: normalizeArgs(b.Input),
				})
			case "tool_result":
				msg.Content = append(msg.Content, decodeToolResultBlock(b)...)
			case "thinking", "redacted_thinking":
				msg.Content = append(msg.Content, types.Part{
					Type: types.PartThinking, Text: b.Thinking, Signature: b.Signature,
				})
			}
		}
		if msg.Role == "assistant" {
			msg.Role = types.RoleAssistant
		} else {
			msg.Role = types.RoleUser
		}
		u.Messages = append(u.Messages, msg)
	}
	return u, nil
}

// decodeToolResultBlock flattens a tool_result block (whose content may nest
// more blocks) into tool_result parts.
func decodeToolResultBlock(b anBlock) []types.Part {
	p := types.Part{
		Type:      types.PartToolResult,
		ID:        "", // carried by message pairing, not needed upstream
		ToolUseID: b.ToolUseID,
		IsError:   b.IsError,
	}
	var texts []string
	var raw = b.Content
	if len(raw) == 0 || string(raw) == "null" {
		texts = nil
	} else if err := json.Unmarshal(raw, &texts); err == nil {
		// []string
	} else {
		var inner []anBlock
		if err := json.Unmarshal(raw, &inner); err == nil {
			for _, ib := range inner {
				if ib.Type == "text" {
					texts = append(texts, ib.Text)
				} else if ib.Type == "image" && ib.Source != nil {
					// Images inside tool results: keep text only for
					// cross-format safety; providers that support it can
					// re-attach later.
				}
			}
		} else {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				texts = []string{s}
			}
		}
	}
	p.Text = strings.Join(texts, "\n")
	return []types.Part{p}
}

func decodeBase64Loose(s string) []byte {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' {
			return -1
		}
		return r
	}, s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// EncodeAnthropicRequest renders the unified request as an Anthropic
// Messages body.
func EncodeAnthropicRequest(u *types.ChatRequest) ([]byte, error) {
	req := anRequest{
		Model:         u.Model,
		Stream:        u.Stream,
		MaxTokens:     orInt(u.MaxTokens, 8192),
		Temperature:   u.Temperature,
		TopP:          u.TopP,
		TopK:          u.TopK,
		StopSequences: u.StopSequences,
	}
	for _, p := range u.System {
		req.System = appendJSON(req.System, anBlock{Type: "text", Text: p.Text})
	}
	switch tc := u.ToolChoice.(type) {
	case types.ToolChoiceMode:
		switch tc {
		case types.ToolChoiceNone:
			req.ToolChoice = &struct {
				Type string `json:"type"`
				Name string `json:"name,omitempty"`
			}{Type: "none"}
		case types.ToolChoiceAny:
			req.ToolChoice = &struct {
				Type string `json:"type"`
				Name string `json:"name,omitempty"`
			}{Type: "any"}
		default:
			req.ToolChoice = &struct {
				Type string `json:"type"`
				Name string `json:"name,omitempty"`
			}{Type: "auto"}
		}
	case *types.ToolChoiceTool:
		req.ToolChoice = &struct {
			Type string `json:"type"`
			Name string `json:"name,omitempty"`
		}{Type: "tool", Name: tc.Name}
	}
	if u.Thinking != nil && u.Thinking.BudgetTokens > 0 {
		req.Thinking = &struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}{Type: "enabled", BudgetTokens: u.Thinking.BudgetTokens}
	}
	for _, t := range u.Tools {
		req.Tools = append(req.Tools, struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			InputSchema json.RawMessage `json:"input_schema"`
		}{Name: t.Name, Description: t.Description, InputSchema: orJSON(t.Schema, `{"type":"object"}`)})
	}

	// Pair tool_use IDs with following tool_results across messages.
	for _, m := range u.Messages {
		var blocks []anBlock
		switch m.Role {
		case types.RoleUser:
			for _, p := range m.Content {
				switch p.Type {
				case types.PartToolResult:
					blocks = append(blocks, anBlock{
						Type:      "tool_result",
						ToolUseID: orDefault(p.ToolUseID, m.ToolCallID),
						Content:   mustJSON(p.Text),
						IsError:   p.IsError,
					})
				case types.PartImage:
					blocks = append(blocks, imageBlock(p))
				case types.PartText:
					if p.Text != "" {
						blocks = append(blocks, anBlock{Type: "text", Text: p.Text})
					}
				}
			}
			// Merge consecutive tool_result-only messages is unnecessary;
			// Anthropic allows multiple tool_result blocks per user turn.
			if len(blocks) == 0 {
				continue
			}
			req.Messages = append(req.Messages, anMessage{Role: "user", Content: mustJSON(blocks)})
		case types.RoleAssistant:
			for _, p := range m.Content {
				switch p.Type {
				case types.PartText:
					if p.Text != "" {
						blocks = append(blocks, anBlock{Type: "text", Text: p.Text})
					}
				case types.PartToolUse:
					blocks = append(blocks, anBlock{
						Type:  "tool_use",
						ID:    orDefault(p.ID, "toolu_"+p.Name),
						Name:  p.Name,
						Input: orJSON(p.Args, `{}`),
					})
				case types.PartThinking:
					blocks = append(blocks, anBlock{
						Type:      "thinking",
						Thinking:  p.Text,
						Signature: p.Signature,
					})
				}
			}
			if len(blocks) == 0 {
				blocks = []anBlock{{Type: "text", Text: ""}}
			}
			req.Messages = append(req.Messages, anMessage{Role: "assistant", Content: mustJSON(blocks)})
		}
	}
	return json.Marshal(req)
}

func imageBlock(p types.Part) anBlock {
	b := anBlock{Type: "image"}
	b.Source = &struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	}{}
	if len(p.Data) > 0 {
		b.Source.Type = "base64"
		b.Source.MediaType = orDefault(p.MIMEType, "image/png")
		b.Source.Data = base64Encode(p.Data)
	} else {
		b.Source.Type = "url"
		b.Source.URL = p.URL
	}
	return b
}

func appendJSON(arr json.RawMessage, v any) json.RawMessage {
	b, _ := json.Marshal(v)
	if len(arr) == 0 {
		return b
	}
	trimmed := strings.TrimRight(strings.TrimSpace(string(arr)), "]")
	trimmed = strings.TrimRight(trimmed, " \t\n")
	out := make([]byte, 0, len(trimmed)+len(b)+2)
	if strings.HasSuffix(trimmed, "[") {
		out = append(out, []byte(trimmed)...)
		out = append(out, b...)
	} else {
		out = append(out, []byte(trimmed)...)
		out = append(out, ',')
		out = append(out, b...)
	}
	return append(out, ']')
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orJSON(raw json.RawMessage, def string) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" || strings.TrimSpace(string(raw)) == "" {
		return json.RawMessage(def)
	}
	return raw
}

// -- Anthropic response --------------------------------------------------------

type anResponse struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Content      []anBlock `json:"content"`
	StopReason   string    `json:"stop_reason,omitempty"`
	StopSequence string    `json:"stop_sequence,omitempty"`
	Usage        *struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// DecodeAnthropicResponse converts an Anthropic Messages response to unified.
func DecodeAnthropicResponse(body []byte) (*types.ChatResponse, error) {
	var r anResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic response: %w", err)
	}
	if r.Error != nil {
		return nil, &types.APIError{Status: 502, Type: r.Error.Type, Message: r.Error.Message}
	}
	out := &types.ChatResponse{ID: r.ID, Model: r.Model, StopReason: mapAnthropicStop(r.StopReason)}
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			out.Content = append(out.Content, types.Part{Type: types.PartText, Text: b.Text})
		case "tool_use":
			out.Content = append(out.Content, types.Part{
				Type: types.PartToolUse, ID: b.ID, Name: b.Name, Args: normalizeArgs(b.Input),
			})
		case "thinking", "redacted_thinking":
			out.Content = append(out.Content, types.Part{Type: types.PartThinking, Text: b.Thinking, Signature: b.Signature})
		}
	}
	if r.Usage != nil {
		out.Usage = types.Usage{
			InputTokens:      r.Usage.InputTokens,
			OutputTokens:     r.Usage.OutputTokens,
			CacheReadTokens:  r.Usage.CacheReadInputTokens,
			CacheWriteTokens: r.Usage.CacheCreationInputTokens,
			UpstreamFormat:   string(FmtAnthropic),
		}
	}
	return out, nil
}

// EncodeAnthropicResponse renders unified response as Anthropic Messages JSON.
func EncodeAnthropicResponse(r *types.ChatResponse) ([]byte, error) {
	resp := anResponse{
		ID:         orDefault(r.ID, "msg_onegw"),
		Type:       "message",
		Role:       "assistant",
		Model:      r.Model,
		StopReason: unmapAnthropicStop(r.StopReason),
	}
	for _, p := range r.Content {
		switch p.Type {
		case types.PartText:
			resp.Content = append(resp.Content, anBlock{Type: "text", Text: p.Text})
		case types.PartToolUse:
			resp.Content = append(resp.Content, anBlock{
				Type:  "tool_use",
				ID:    orDefault(p.ID, "toolu_"+p.Name),
				Name:  p.Name,
				Input: orJSON(p.Args, `{}`),
			})
		case types.PartThinking:
			resp.Content = append(resp.Content, anBlock{Type: "thinking", Thinking: p.Text, Signature: p.Signature})
		}
	}
	if len(resp.Content) == 0 {
		resp.Content = []anBlock{{Type: "text", Text: ""}}
	}
	resp.Usage = &struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	}{
		InputTokens:              r.Usage.InputTokens,
		OutputTokens:             maxI64(r.Usage.OutputTokens, 1),
		CacheReadInputTokens:     r.Usage.CacheReadTokens,
		CacheCreationInputTokens: r.Usage.CacheWriteTokens,
	}
	return json.Marshal(resp)
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// DecodeAnthropicError parses an Anthropic error payload.
func DecodeAnthropicError(body []byte, status int) *types.APIError {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Message == "" {
		return &types.APIError{Status: status, Type: "upstream_error", Message: strings.TrimSpace(string(body))}
	}
	return &types.APIError{Status: status, Type: e.Error.Type, Message: e.Error.Message}
}
