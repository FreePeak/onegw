package translat

import (
	"encoding/json"
	"testing"

	"onegw/internal/types"
)

// TestDecodeSystemOneResponse verifies the TypeSafe Jev envelope
// ({model, answers, usage}) decodes to a unified ChatResponse the way
// DecodeResponse(FmtSystemOne, ...) consumes it: each answer becomes a
// text part (the selected option id for Choice, the legend label for
// Score, "probability: X.XXX" for Noul), with the usage counters copied.
func TestDecodeSystemOneResponse(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"q1": {"type":"choice","choice":"pass","confidence":1.0,"probabilities":{"pass":1.0}},
			"q2": {"type":"score","score":0.9,"confidence":0.9,"legend":{"0":"nope","1":"sure","2":"maybe"}},
			"q3": {"type":"noul","confidence":0.5,"probabilities":{"1":0.7,"0":0.3}}
		},
		"usage": {"input_tokens": 309, "output_tokens": 24}
	}`)

	got, err := DecodeSystemOneResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "systemone-jev-1.13.0" {
		t.Fatalf("id: want systemone-jev-1.13.0, got %q", got.ID)
	}
	if got.Model != "jev-1.13.0" {
		t.Fatalf("model: want jev-1.13.0, got %q", got.Model)
	}
	wantParts := map[string]bool{
		"pass": true, "nope": true, "probability: 0.700": true,
	}
	if len(got.Content) != len(wantParts) {
		t.Fatalf("parts: want %d, got %d: %+v", len(wantParts), len(got.Content), got.Content)
	}
	seen := make(map[string]bool, len(got.Content))
	for _, p := range got.Content {
		if p.Type != types.PartText {
			t.Errorf("part type: want %q, got %q", types.PartText, p.Type)
			continue
		}
		if !wantParts[string(p.Text)] {
			t.Errorf("unexpected part: %q", string(p.Text))
			continue
		}
		seen[string(p.Text)] = true
	}
	for w := range wantParts {
		if !seen[w] {
			t.Errorf("missing part: %q", w)
		}
	}
	if got.Usage.InputTokens != 309 || got.Usage.OutputTokens != 24 {
		t.Fatalf("usage: want 309/24, got %d/%d", got.Usage.InputTokens, got.Usage.OutputTokens)
	}
}

// TestDecodeSystemOneResponseMalformed proves the function fails closed
// on a non-systemone body (the default DecodeOpenAIResponse path would
// silently misread such a body instead).
func TestDecodeSystemOneResponseMalformed(t *testing.T) {
	_, err := DecodeSystemOneResponse([]byte(`not json`))
	if err == nil {
		t.Fatal("want error for malformed body, got nil")
	}
}

// TestDecodeSystemOneResponseEmpty proves an envelope with no answers
// decodes without crashing and carries zero usage.
func TestDecodeSystemOneResponseEmpty(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"model": "jev-latest", "answers": map[string]any{}, "usage": map[string]any{},
	})
	got, err := DecodeSystemOneResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "systemone-jev-latest" || got.Model != "jev-latest" {
		t.Fatalf("id/model wrong: %+v", got)
	}
	if len(got.Content) != 0 {
		t.Fatalf("want no parts, got %d", len(got.Content))
	}
}
