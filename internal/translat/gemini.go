package translat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// Gemini generateContent wire shapes
// ---------------------------------------------------------------------------

type gemPart struct {
	Text string `json:"text,omitempty"`
	// inlineData for images.
	InlineData *struct {
		MimeType string `json:"mimeType,omitempty"`
		Data     string `json:"data,omitempty"`
	} `json:"inlineData,omitempty"`
	FunctionCall *struct {
		Name string          `json:"name,omitempty"`
		Args json.RawMessage `json:"args,omitempty"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		Name     string          `json:"name,omitempty"`
		Response json.RawMessage `json:"response,omitempty"`
	} `json:"functionResponse,omitempty"`
	Thought     bool   `json:"thought,omitempty"`
	ThoughtSign string `json:"thoughtSignature,omitempty"`
}

type gemContent struct {
	Role  string    `json:"role,omitempty"` // user | model
	Parts []gemPart `json:"parts"`
}

type gemToolDef struct {
	FunctionDeclarations []struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"functionDeclarations,omitempty"`
}

type gemToolConfig struct {
	FunctionCallingConfig *struct {
		Mode                 string   `json:"mode,omitempty"` // AUTO | ANY | NONE
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig,omitempty"`
}

type gemRequest struct {
	Contents          []gemContent   `json:"contents"`
	SystemInstruction *gemContent    `json:"systemInstruction,omitempty"`
	Tools             []gemToolDef   `json:"tools,omitempty"`
	ToolConfig        *gemToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *struct {
		MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
		Temperature      *float64        `json:"temperature,omitempty"`
		TopP             *float64        `json:"topP,omitempty"`
		TopK             *float64        `json:"topK,omitempty"`
		StopSequences    []string        `json:"stopSequences,omitempty"`
		ResponseMimeType string          `json:"responseMimeType,omitempty"`
		ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	} `json:"generationConfig,omitempty"`
}

// DecodeGeminiRequest parses a Gemini generateContent body into unified form.
func DecodeGeminiRequest(body []byte) (*types.ChatRequest, error) {
	var req gemRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	u := &types.ChatRequest{Model: "gemini", Stream: false}
	if req.SystemInstruction != nil {
		for _, p := range req.SystemInstruction.Parts {
			u.System = append(u.System, types.Part{Type: types.PartText, Text: p.Text})
		}
	}
	if req.GenerationConfig != nil {
		g := req.GenerationConfig
		u.MaxTokens = g.MaxOutputTokens
		u.Temperature = g.Temperature
		u.TopP = g.TopP
		if g.TopK != nil {
			k := int(*g.TopK)
			u.TopK = &k
		}
		u.StopSequences = g.StopSequences
		if g.ResponseMimeType == "application/json" {
			rf := &types.ResponseFormat{Type: "json_object"}
			if len(g.ResponseSchema) > 0 {
				rf.Type = "json_schema"
				rf.Schema = g.ResponseSchema
			}
			u.ResponseFormat = rf
		}
	}
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil {
		switch req.ToolConfig.FunctionCallingConfig.Mode {
		case "NONE":
			u.ToolChoice = types.ToolChoiceNone
		case "ANY":
			u.ToolChoice = types.ToolChoiceAny
		case "AUTO":
			u.ToolChoice = types.ToolChoiceAuto
		}
		if names := req.ToolConfig.FunctionCallingConfig.AllowedFunctionNames; len(names) == 1 {
			u.ToolChoice = &types.ToolChoiceTool{Name: names[0]}
		}
	}
	for _, td := range req.Tools {
		for _, fd := range td.FunctionDeclarations {
			u.Tools = append(u.Tools, types.Tool{Name: fd.Name, Description: fd.Description, Schema: fd.Parameters})
		}
	}
	for _, c := range req.Contents {
		role := types.RoleUser
		if c.Role == "model" {
			role = types.RoleAssistant
		}
		msg := types.Message{Role: role}
		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				msg.Content = append(msg.Content, types.Part{
					Type: types.PartToolUse,
					ID:   "fc_" + p.FunctionCall.Name,
					Name: p.FunctionCall.Name,
					Args: gemArgsOrEmpty(p.FunctionCall.Args),
				})
			case p.FunctionResponse != nil:
				msg.Content = append(msg.Content, types.Part{
					Type:      types.PartToolResult,
					Name:      p.FunctionResponse.Name,
					ToolUseID: "fc_" + p.FunctionResponse.Name,
					Text:      string(p.FunctionResponse.Response),
				})
			case p.InlineData != nil:
				data, _ := base64.StdEncoding.DecodeString(p.InlineData.Data)
				msg.Content = append(msg.Content, types.Part{
					Type: types.PartImage, MIMEType: p.InlineData.MimeType, Data: data,
				})
			case p.Thought:
				msg.Content = append(msg.Content, types.Part{
					Type: types.PartThinking, Text: p.Text, Signature: p.ThoughtSign,
				})
			default:
				msg.Content = append(msg.Content, types.Part{Type: types.PartText, Text: p.Text})
			}
		}
		u.Messages = append(u.Messages, msg)
	}
	return u, nil
}

func gemArgsOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" {
		return json.RawMessage("{}")
	}
	return raw
}

// EncodeGeminiRequest renders the unified request as a Gemini
// generateContent body.
func EncodeGeminiRequest(u *types.ChatRequest) ([]byte, error) {
	req := gemRequest{}
	g := &struct {
		MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
		Temperature      *float64        `json:"temperature,omitempty"`
		TopP             *float64        `json:"topP,omitempty"`
		TopK             *float64        `json:"topK,omitempty"`
		StopSequences    []string        `json:"stopSequences,omitempty"`
		ResponseMimeType string          `json:"responseMimeType,omitempty"`
		ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	}{}
	if u.MaxTokens > 0 {
		g.MaxOutputTokens = u.MaxTokens
	}
	g.Temperature = u.Temperature
	g.TopP = u.TopP
	if u.TopK != nil {
		k := float64(*u.TopK)
		g.TopK = &k
	}
	g.StopSequences = u.StopSequences
	if u.ResponseFormat != nil {
		g.ResponseMimeType = "application/json"
		if len(u.ResponseFormat.Schema) > 0 {
			g.ResponseSchema = u.ResponseFormat.Schema
		}
	}
	req.GenerationConfig = g

	for _, p := range u.System {
		req.SystemInstruction = appendPart(req.SystemInstruction, gemPart{Text: p.Text})
	}
	switch tc := u.ToolChoice.(type) {
	case types.ToolChoiceMode:
		mode := "AUTO"
		switch tc {
		case types.ToolChoiceNone:
			mode = "NONE"
		case types.ToolChoiceAny:
			mode = "ANY"
		}
		req.ToolConfig = &gemToolConfig{FunctionCallingConfig: &struct {
			Mode                 string   `json:"mode,omitempty"`
			AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
		}{Mode: mode}}
	case *types.ToolChoiceTool:
		req.ToolConfig = &gemToolConfig{FunctionCallingConfig: &struct {
			Mode                 string   `json:"mode,omitempty"`
			AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
		}{Mode: "ANY", AllowedFunctionNames: []string{tc.Name}}}
	}
	if len(u.Tools) > 0 {
		td := gemToolDef{}
		for _, t := range u.Tools {
			td.FunctionDeclarations = append(td.FunctionDeclarations, struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}{Name: t.Name, Description: t.Description, Parameters: orJSON(t.Schema, `{"type":"object"}`)})
		}
		req.Tools = []gemToolDef{td}
	}
	for _, m := range u.Messages {
		role := "user"
		if m.Role == types.RoleAssistant {
			role = "model"
		}
		c := gemContent{Role: role}
		for _, p := range m.Content {
			switch p.Type {
			case types.PartText:
				if p.Text != "" {
					c.Parts = append(c.Parts, gemPart{Text: p.Text})
				}
			case types.PartImage:
				ip := gemPart{}
				ip.InlineData = &struct {
					MimeType string `json:"mimeType,omitempty"`
					Data     string `json:"data,omitempty"`
				}{MimeType: orDefault(p.MIMEType, "image/png"), Data: base64.StdEncoding.EncodeToString(p.Data)}
				c.Parts = append(c.Parts, ip)
			case types.PartToolUse:
				c.Parts = append(c.Parts, gemPart{FunctionCall: &struct {
					Name string          `json:"name,omitempty"`
					Args json.RawMessage `json:"args,omitempty"`
				}{Name: p.Name, Args: gemArgsOrEmpty(p.Args)}})
			case types.PartToolResult:
				resp := p.Text
				if strings.TrimSpace(resp) == "" {
					resp = "{}"
				}
				// Response must be a JSON object; wrap raw text if needed.
				if !strings.HasPrefix(strings.TrimSpace(resp), "{") {
					b, _ := json.Marshal(map[string]string{"result": resp})
					resp = string(b)
				}
				c.Parts = append(c.Parts, gemPart{FunctionResponse: &struct {
					Name     string          `json:"name,omitempty"`
					Response json.RawMessage `json:"response,omitempty"`
				}{Name: toolNameForMsgs(u.Messages, p), Response: json.RawMessage(resp)}})
			case types.PartThinking:
				c.Parts = append(c.Parts, gemPart{Text: p.Text, Thought: true, ThoughtSign: p.Signature})
			}
		}
		if len(c.Parts) > 0 {
			req.Contents = append(req.Contents, c)
		}
	}
	return json.Marshal(req)
}

func toolNameForMsgs(msgs []types.Message, p types.Part) string {
	if p.Name != "" {
		return p.Name
	}
	for _, m := range msgs {
		for _, q := range m.Content {
			if q.Type == types.PartToolUse && q.ID == p.ToolUseID {
				return q.Name
			}
		}
	}
	return "unknown_tool"
}

func appendPart(c *gemContent, p gemPart) *gemContent {
	if c == nil {
		c = &gemContent{Role: "user"}
	}
	c.Parts = append(c.Parts, p)
	return c
}

// ---------------------------------------------------------------------------
// Gemini response
// ---------------------------------------------------------------------------

type gemResponse struct {
	Candidates []struct {
		Content *struct {
			Role  string    `json:"role"`
			Parts []gemPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *gemUsage `json:"usageMetadata"`
	ModelVersion  string    `json:"modelVersion"`
}

type gemUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
}

// DecodeGeminiResponse converts a Gemini response to unified.
func DecodeGeminiResponse(body []byte) (*types.ChatResponse, error) {
	var r gemResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("gemini response: %w", err)
	}
	out := &types.ChatResponse{ID: "gemini-" + randHex(8), Model: r.ModelVersion}
	if len(r.Candidates) > 0 {
		cand := r.Candidates[0]
		out.StopReason = mapGeminiStop(cand.FinishReason)
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				switch {
				case p.FunctionCall != nil:
					out.Content = append(out.Content, types.Part{
						Type: types.PartToolUse,
						ID:   "fc_" + p.FunctionCall.Name,
						Name: p.FunctionCall.Name,
						Args: gemArgsOrEmpty(p.FunctionCall.Args),
					})
				case p.Thought:
					out.Content = append(out.Content, types.Part{
						Type: types.PartThinking, Text: p.Text, Signature: p.ThoughtSign,
					})
				default:
					out.Content = append(out.Content, types.Part{Type: types.PartText, Text: p.Text})
				}
			}
		}
	}
	if r.UsageMetadata != nil {
		um := r.UsageMetadata
		out.Usage = types.Usage{
			InputTokens:     um.PromptTokenCount,
			OutputTokens:    um.CandidatesTokenCount,
			CacheReadTokens: um.CachedContentTokenCount,
			ReasoningTokens: um.ThoughtsTokenCount,
			UpstreamFormat:  string(FmtGemini),
		}
	}
	return out, nil
}

// EncodeGeminiResponse renders unified response as Gemini JSON.
func EncodeGeminiResponse(r *types.ChatResponse) ([]byte, error) {
	resp := gemResponse{ModelVersion: r.Model}
	cand := struct {
		Content *struct {
			Role  string    `json:"role"`
			Parts []gemPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	}{}
	content := &struct {
		Role  string    `json:"role"`
		Parts []gemPart `json:"parts"`
	}{Role: "model"}
	for _, p := range r.Content {
		switch p.Type {
		case types.PartText:
			content.Parts = append(content.Parts, gemPart{Text: p.Text})
		case types.PartToolUse:
			content.Parts = append(content.Parts, gemPart{FunctionCall: &struct {
				Name string          `json:"name,omitempty"`
				Args json.RawMessage `json:"args,omitempty"`
			}{Name: p.Name, Args: gemArgsOrEmpty(p.Args)}})
		case types.PartThinking:
			content.Parts = append(content.Parts, gemPart{Text: p.Text, Thought: true, ThoughtSign: p.Signature})
		}
	}
	if len(content.Parts) == 0 {
		content.Parts = []gemPart{{Text: ""}}
	}
	cand.Content = content
	cand.FinishReason = unmapGeminiStop(r.StopReason)
	resp.Candidates = []struct {
		Content *struct {
			Role  string    `json:"role"`
			Parts []gemPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	}{cand}
	if r.Usage.InputTokens > 0 || r.Usage.OutputTokens > 0 {
		resp.UsageMetadata = &gemUsage{
			PromptTokenCount:        r.Usage.InputTokens,
			CandidatesTokenCount:    r.Usage.OutputTokens,
			TotalTokenCount:         r.Usage.InputTokens + r.Usage.OutputTokens,
			CachedContentTokenCount: r.Usage.CacheReadTokens,
			ThoughtsTokenCount:      r.Usage.ReasoningTokens,
		}
	}
	return json.Marshal(resp)
}

// DecodeGeminiError parses a Gemini error payload.
func DecodeGeminiError(body []byte, status int) *types.APIError {
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Message == "" {
		return &types.APIError{Status: status, Type: "upstream_error", Message: strings.TrimSpace(string(body))}
	}
	st := status
	if e.Error.Code > 0 {
		st = e.Error.Code
	}
	return &types.APIError{Status: st, Type: orDefault(e.Error.Status, "upstream_error"), Message: e.Error.Message}
}
