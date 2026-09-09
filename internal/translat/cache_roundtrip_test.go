package translat

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/types"
)

// TestAnthropicCacheControlRoundTrip pins issue #32's proven-broken
// scenario: cache_control {type: ephemeral} anchored on the last system
// block and the last message content block must survive
// DecodeAnthropicRequest → EncodeAnthropicRequest at the same logical
// positions.
func TestAnthropicCacheControlRoundTrip(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 256,
		"system": [
			{"type": "text", "text": "first, no marker"},
			{"type": "text", "text": "second, marked", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{"role": "user", "content": "earlier turn"},
			{"role": "user", "content": [
				{"type": "text", "text": "earlier part, no marker"},
				{"type": "text", "text": "final part, marked", "cache_control": {"type": "ephemeral"}}
			]}
		]
	}`
	u, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	// Breakpoints captured at decode.
	if len(u.System) != 2 || !u.System[1].CacheBreakpoint || u.System[0].CacheBreakpoint {
		t.Fatalf("system breakpoints misdecoded: %+v", u.System)
	}
	msgs := u.Messages
	if len(msgs) != 2 || len(msgs[1].Content) != 2 {
		t.Fatalf("messages misdecoded: %+v", msgs)
	}
	if msgs[1].Content[0].CacheBreakpoint || !msgs[1].Content[1].CacheBreakpoint {
		t.Fatalf("message breakpoints misdecoded: %+v", msgs[1].Content)
	}

	out, err := EncodeAnthropicRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var rt struct {
		System []struct {
			Text         string `json:"text"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Content []struct {
				Text         string `json:"text"`
				CacheControl *struct {
					Type string `json:"type"`
				} `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &rt); err != nil {
		t.Fatalf("re-encoded body unparseable: %v (%s)", err, out)
	}
	if len(rt.System) != 2 || rt.System[1].CacheControl == nil || rt.System[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("system breakpoint not re-anchored: %s", out)
	}
	if rt.System[0].CacheControl != nil {
		t.Fatalf("unmarked system block grew a marker: %s", out)
	}
	if len(rt.Messages) != 2 || len(rt.Messages[1].Content) != 2 {
		t.Fatalf("message shape changed: %s", out)
	}
	last := rt.Messages[1].Content[1]
	if last.CacheControl == nil || last.CacheControl.Type != "ephemeral" {
		t.Fatalf("message breakpoint not re-anchored: %s", out)
	}
	if rt.Messages[1].Content[0].CacheControl != nil {
		t.Fatalf("unmarked message block grew a marker: %s", out)
	}
}

// TestOpenAICacheKnobRoundTrip pins OpenAI-dialect knob preservation:
// prompt_cache_key and session_id survive decode → encode; a request
// without them encodes without them (never invented).
func TestOpenAICacheKnobRoundTrip(t *testing.T) {
	body := `{
		"model": "glm-5.3-flash",
		"max_tokens": 64,
		"prompt_cache_key": "conv-abc123",
		"session_id": "sess-xyz789",
		"messages": [{"role": "user", "content": "hi"}]
	}`
	u, err := DecodeOpenAIRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u.PromptCacheKey != "conv-abc123" || u.StickySessionID != "sess-xyz789" {
		t.Fatalf("cache knobs not captured: %+v", u)
	}
	out, err := EncodeOpenAIRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	assertKnob := func(b []byte, key, want string) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got, _ := m[key].(string); got != want {
			t.Fatalf("%s = %q, want %q (body %s)", key, got, want, b)
		}
	}
	assertKnob(out, "prompt_cache_key", "conv-abc123")
	assertKnob(out, "session_id", "sess-xyz789")

	// Absent knobs must not be invented by either direction.
	u2, err := DecodeOpenAIRequest([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out2, err := EncodeOpenAIRequest(u2)
	if err != nil {
		t.Fatal(err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(out2, &m2); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"prompt_cache_key", "session_id"} {
		if _, ok := m2[key]; ok {
			t.Fatalf("%s invented for request without one: %s", key, out2)
		}
	}
}

// TestAnthropicBreakpointToOpenAIMarker pins the cross-format money path:
// an Anthropic-surface request with cache_control markers re-encodes onto
// the OpenAI wire with cache_control markers on the same content parts
// (OpenRouter forwards them), and a plain prompt_cache_key passthrough
// works when the Anthropic client sent one (Claude Code → glm).
func TestAnthropicBreakpointToOpenAIMarker(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 256,
		"prompt_cache_key": "cc-session-1",
		"system": [
			{"type": "text", "text": "preamble"},
			{"type": "text", "text": "long tools doc", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "turn tail", "cache_control": {"type": "ephemeral"}}
			]}
		]
	}`
	u, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeOpenAIRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"prompt_cache_key":"cc-session-1"`) {
		t.Fatalf("prompt_cache_key not forwarded cross-format: %s", s)
	}
	// Both breakpoints must land as cache_control markers on OpenAI wire
	// content parts, and the system block must become a parts array (not
	// a flattened string) to carry the marker.
	if !strings.Contains(s, `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("cache_control marker lost cross-format: %s", s)
	}
	if got := strings.Count(s, `"cache_control":{"type":"ephemeral"}`); got != 2 {
		t.Fatalf("want 2 re-anchored markers, got %d: %s", got, s)
	}
}

// TestGeminiCacheKnobsLeftAlone documents the deliberate scope decision:
// Gemini wire has no equivalent knobs, so nothing is invented on decode or
// encode.
func TestGeminiCacheKnobsLeftAlone(t *testing.T) {
	u := &types.ChatRequest{
		Model:     "gemini",
		MaxTokens: 64,
		System:    []types.Part{{Type: types.PartText, Text: "sys", CacheBreakpoint: true}},
		Messages:  []types.Message{{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "hi", CacheBreakpoint: true}}}},
	}
	out, err := EncodeGeminiRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "cache") || strings.Contains(s, "prompt_cache_key") {
		t.Fatalf("gemini encoder invented cache fields: %s", s)
	}
}
