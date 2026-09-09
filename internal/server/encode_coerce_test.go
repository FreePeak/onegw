package server

import (
	"encoding/json"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/translat"
)

// TestCoerceAlwaysThinkingCrossFormat pins issue #50: an Anthropic-surface
// request routed to an always-thinking upstream via the cross-format branch
// must be coerced BEFORE encoding — reasoning_effort none|medium become
// low, and no thinking-disable knob may reach the wire. The same-format
// path is pinned by TestAdaptAlwaysThinking.
func TestCoerceAlwaysThinkingCrossFormat(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"glm-*"}}

	// Anthropic surface (Claude Code) → OpenAI upstream, effort "medium".
	body := []byte(`{"model":"claude-x","max_tokens":64,"reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`)
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	assertField(t, out, "reasoning_effort", "low")

	// Effort "none" is likewise rewritten to low.
	body = []byte(`{"model":"claude-x","max_tokens":64,"reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`)
	out, err = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	assertField(t, out, "reasoning_effort", "low")

	// A thinking:{type:disabled} disable request must not surface any
	// disable knob on the OpenAI wire (and must not invent one).
	body = []byte(`{"model":"claude-x","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	out, err = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"thinking", "enable_thinking"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s invented on cross-format path: %s", key, out)
		}
	}

	// Supported effort ("high") passes through untouched.
	body = []byte(`{"model":"claude-x","max_tokens":64,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	out, err = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	assertField(t, out, "reasoning_effort", "high")
}

// TestCoerceAlwaysThinkingNonListedPassthrough pins the no-invention side
// of issue #50: non-listed models and nil defs must pass through the
// cross-format branch with the client's knob byte-identical in meaning.
func TestCoerceAlwaysThinkingNonListedPassthrough(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"glm-*"}}

	// Non-listed model keeps "none" — OpenAI-dialect upstreams accept it.
	body := []byte(`{"model":"m","max_tokens":64,"reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`)
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, body, "gpt-5.4-mini", def)
	if err != nil {
		t.Fatal(err)
	}
	assertField(t, out, "reasoning_effort", "none")

	// nil def (never configured) → untouched even for a glm model.
	out, err = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, body, "glm-5.3-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertField(t, out, "reasoning_effort", "none")

	// OpenAI surface → Anthropic upstream, non-listed model: effort
	// survives translation unmangled.
	body = []byte(`{"model":"m","max_tokens":64,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	out, err = prepareUpstreamBody(translat.FmtAnthropic, translat.FmtOpenAI, body, "gpt-5.4-mini", def)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort invented on Anthropic wire: %s", out)
	}
}
