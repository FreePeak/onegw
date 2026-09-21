package translat

import (
	"encoding/json"
	"fmt"

	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// TypeSafe System One wire shapes
//
// The TypeSafe Jev endpoint (verified live 2026-09-21 at
// https://api.typesafe.ai/v1/systemone) speaks a structured
// evaluation envelope on both sides of onegw's POST /v1/systemone
// surface. The server forwards the client request body verbatim
// to upstream, and upstream answers {model, answers, usage}; the
// shapes below decode the upstream envelope into onegw's unified
// ChatResponse when a cross-format relay needs it (OpenAI client
// on a systemone target). Choice, Score, and Noul are the three
// answer primitives.
// ---------------------------------------------------------------------------

// systemOneResponse is the native TypeSafe evaluation answer.
type systemOneResponse struct {
	Model   string                   `json:"model"`
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   systemOneUsage             `json:"usage"`
}

type systemOneAnswer struct {
	Type          string                `json:"type"` // "choice" | "score" | "noul"
	Choice        string                `json:"choice,omitempty"`
	Score         float64               `json:"score,omitempty"`
	Probabilities map[string]float64    `json:"probabilities,omitempty"`
	Confidence    float64               `json:"confidence"`
	Legend        map[string]string     `json:"legend,omitempty"`
}

type systemOneUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// DecodeSystemOneResponse converts a TypeSafe evaluation envelope
// into the unified ChatResponse. Jev returns structured answers
// (Choice / Score / Noul), not prose, so the best text approximation
// for an OpenAI client is the selected option IDs joined with ", ".
func DecodeSystemOneResponse(body []byte) (*types.ChatResponse, error) {
	var resp systemOneResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode systemone response: %w", err)
	}
	out := &types.ChatResponse{
		ID:    "systemone-" + resp.Model,
		Model: resp.Model,
	}
	for _, ans := range resp.Answers {
		out.Content = append(out.Content, types.Part{Type: types.PartText, Text: ToOpenAIChoiceText(ans)})
	}
	out.Usage = types.Usage{
		InputTokens:  int64(resp.Usage.InputTokens),
		OutputTokens: int64(resp.Usage.OutputTokens),
	}
	return out, nil
}

// ToOpenAIChoiceText extracts a human-readable answer from one
// TypeSafe answer. Jev returns structured decisions, not prose,
// so this is the best text approximation for an OpenAI client:
//
//   - Choice: the selected option ID.
//   - Score:  the level label from the legend, or the raw score.
//   - Noul:   "probability: X.XXX" (P(true)).
func ToOpenAIChoiceText(ans systemOneAnswer) string {
	switch ans.Type {
	case "choice":
		return ans.Choice
	case "score":
		if ans.Legend != nil {
			if label, ok := ans.Legend[fmt.Sprintf("%d", int(ans.Score))]; ok {
				return label
			}
		}
		return fmt.Sprintf("score: %.3f", ans.Score)
	case "noul":
		return fmt.Sprintf("probability: %.3f", ans.Probabilities["1"])
	default:
		return ans.Type
	}
}
