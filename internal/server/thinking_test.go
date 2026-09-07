package server

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/translat"
)

// assertField decodes a prepared body and checks the top-level string
// field; want "" means the key must be absent.
func assertField(t *testing.T, body []byte, key, want string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	got, _ := m[key].(string)
	if want == "" {
		if _, ok := m[key]; ok {
			t.Errorf("%s = %q, want absent (body %s)", key, got, body)
		}
		return
	}
	if got != want {
		t.Errorf("%s = %q, want %q (body %s)", key, got, want, body)
	}
}

func TestAlwaysThinkingModel(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"glm-*", "z-ai/glm-*"}}
	for model, want := range map[string]bool{
		"glm-5.3-flash":      true,
		"glm-4.5-air":        true,
		"z-ai/glm-5.3":       true,
		"openai/gpt-5.4":     false,
		"claude-opus-5":      false,
		"minimax/minimax-m3": false,
	} {
		if got := def.AlwaysThinkingModel(model); got != want {
			t.Errorf("AlwaysThinkingModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestCoerceEffort(t *testing.T) {
	for in, want := range map[string]string{
		"none": "low", "minimal": "low", "medium": "low",
		"low": "low", "high": "high", "max": "max", "xhigh": "xhigh",
	} {
		if got := coerceEffort(in); got != want {
			t.Errorf("coerceEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdaptAlwaysThinking(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"glm-*"}}

	// GLM error 1210: explicit disable knobs must be rewritten.
	body := []byte(`{"model":"m","reasoning_effort":"none","thinking":{"type":"disabled"},"enable_thinking":false,"messages":[]}`)
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := m["reasoning_effort"].(string); got != "low" {
		t.Errorf("reasoning_effort = %q, want low (body %s)", got, out)
	}
	for _, key := range []string{"thinking", "enable_thinking"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s not dropped: %s", key, out)
		}
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Errorf("messages lost: %s", out)
	}

	// Supported values pass through untouched.
	body = []byte(`{"model":"m","reasoning_effort":"high","messages":[]}`)
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	assertField(t, out, "reasoning_effort", "high")

	// No knob present → nothing added.
	body = []byte(`{"model":"m","messages":[]}`)
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	assertField(t, out, "reasoning_effort", "")

	// Non-matching model → untouched (OpenAI accepts "none").
	body = []byte(`{"model":"m","reasoning_effort":"none","messages":[]}`)
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "gpt-5.4-mini", def)
	assertField(t, out, "reasoning_effort", "none")

	// nil def (never configured) → untouched.
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", nil)
	assertField(t, out, "reasoning_effort", "none")
}

func TestAdaptAlwaysThinkingPreservesNumbers(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"*"}}
	body := []byte(`{"model":"m","temperature":0.7,"reasoning_effort":"medium","max_tokens":4096,"messages":[]}`)
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"reasoning_effort":"low"`) {
		t.Errorf("want reasoning_effort low in %s", got)
	}
	if !strings.Contains(got, "0.7") || !strings.Contains(got, "4096") {
		t.Errorf("numeric fidelity broken: %s", got)
	}
}
