package provider

import (
	"encoding/json"
	"testing"
)

func TestStripReasoningContent_RemovesFromAssistant(t *testing.T) {
	input := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello","reasoning_content":"thinking here"},{"role":"user","content":"why?"}]}`
	got := stripReasoningContent([]byte(input))
	if got == nil {
		t.Fatal("expected strip, got nil")
	}
	var req struct {
		Messages []struct {
			Role             string          `json:"role"`
			ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
			Content          any             `json:"content,omitempty"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(req.Messages))
	}
	// User messages untouched
	if req.Messages[0].Role != "user" {
		t.Errorf("msg[0] role: want user, got %s", req.Messages[0].Role)
	}
	// Assistant message: reasoning_content stripped
	if len(req.Messages[1].ReasoningContent) != 0 {
		t.Errorf("msg[1] reasoning_content should be empty, got %s", req.Messages[1].ReasoningContent)
	}
	if req.Messages[1].Role != "assistant" {
		t.Errorf("msg[1] role: want assistant, got %s", req.Messages[1].Role)
	}
}

func TestStripReasoningContent_NoOpWhenAbsent(t *testing.T) {
	input := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`
	got := stripReasoningContent([]byte(input))
	if got != nil {
		t.Errorf("expected nil (no change), got %s", got)
	}
}

func TestStripReasoningContent_FastPathNoField(t *testing.T) {
	// Body doesn't contain "reasoning_content" at all — fast path returns nil
	input := `{"model":"mistral-large","messages":[{"role":"user","content":"hello"}]}`
	got := stripReasoningContent([]byte(input))
	if got != nil {
		t.Errorf("expected nil (fast path), got %s", got)
	}
}

func TestStripReasoningContent_InvalidJSON(t *testing.T) {
	got := stripReasoningContent([]byte(`{not json`))
	if got != nil {
		t.Errorf("expected nil on invalid JSON, got %s", got)
	}
}

func TestStripReasoningContent_EmptyBody(t *testing.T) {
	got := stripReasoningContent([]byte{})
	if got != nil {
		t.Errorf("expected nil on empty body, got %s", got)
	}
}

func TestStripReasoningContent_PreservesUserReasoning(t *testing.T) {
	// Only assistant messages should be stripped; user messages with
	// reasoning_content (rare but valid) must pass through.
	input := `{"messages":[{"role":"user","content":"hi","reasoning_content":"user thinking"}]}`
	got := stripReasoningContent([]byte(input))
	if got != nil {
		t.Errorf("expected nil (user msg not stripped), got %s", got)
	}
}

func TestStripReasoningContent_MultipleAssistantTurns(t *testing.T) {
	input := `{"messages":[
		{"role":"assistant","content":"a1","reasoning_content":"r1"},
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a2","reasoning_content":"r2"}
	]}`
	got := stripReasoningContent([]byte(input))
	if got == nil {
		t.Fatal("expected strip, got nil")
	}
	var req struct {
		Messages []struct {
			Role             string          `json:"role"`
			ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, m := range req.Messages {
		if m.Role == "assistant" && len(m.ReasoningContent) != 0 {
			t.Errorf("msg[%d] reasoning_content should be empty, got %s", i, m.ReasoningContent)
		}
	}
}

func TestKindMistral_Format(t *testing.T) {
	if got := KindMistral.Format(); got != "openai" {
		t.Errorf("KindMistral.Format() = %q, want %q", got, "openai")
	}
}

func TestKindMistral_DefaultBaseURL(t *testing.T) {
	got := KindMistral.DefaultBaseURL()
	want := "https://api.mistral.ai/v1"
	if got != want {
		t.Errorf("KindMistral.DefaultBaseURL() = %q, want %q", got, want)
	}
}
