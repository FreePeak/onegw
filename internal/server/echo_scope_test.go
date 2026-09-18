package server

import (
	"encoding/json"
	"testing"

	"onegw/internal/provider"
)

// TestSynthesizeReasoningEchoScopesToCurrentTurn pins issue #351:
// echo synthesis must touch ONLY the assistant turn immediately
// preceding the trailing tool result. Earlier assistant turns
// carry real reasoning that must be preserved — stamping them
// with the "(context elided)" placeholder is what made DeepSeek
// V4.1 Flash collapse into placeholder-only / filler-only thinking
// (22.6% of assistant turns on 2026-09-17; 26 consecutive
// thinking-less turns).
func TestSynthesizeReasoningEchoScopesToCurrentTurn(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			// turn 1: answered with real reasoning — must survive
			map[string]any{"role": "assistant", "content": "ok",
				"reasoning_content": "read the file first"},
			map[string]any{"role": "tool", "content": "out", "tool_call_id": "c1"},
			// turn 2: the echo-less assistant immediately before tail
			map[string]any{"role": "assistant", "content": "grep it"},
			// trailing tool result — the continuation trigger
			map[string]any{"role": "tool", "content": "out2", "tool_call_id": "c2"},
		},
	})
	def := &provider.Def{Name: "oc", EchoReasoning: []string{"deepseek-*"}}
	out := synthesizeReasoningEcho(body, "deepseek-v4.1-flash", def)

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}

	turn1 := got.Messages[1]
	if s, _ := turn1["reasoning_content"].(string); s != "read the file first" {
		t.Fatalf("turn 1 reasoning was overwritten: %s\n%s", turn1["reasoning_content"], out)
	}
	// Messages[2] is the tool result for c1; Messages[3] is the
	// assistant turn immediately preceding the tail tool result.
	turnCurrent := got.Messages[3]
	if s, _ := turnCurrent["reasoning_content"].(string); s != "(context elided)" {
		t.Fatalf("current turn was not filled: %s\n%s", turnCurrent["reasoning_content"], out)
	}
	if string(out) == string(body) {
		t.Fatal("body must change: current turn needs an echo")
	}
}

// TestSynthesizeReasoningEchoAlreadyHasAliasNoRewrite pins the
// short-circuit from the opposite direction: if the current turn
// already carries an echo under either alias (reasoning_content or
// the stable "reasoning" alias xdev buildRequest uses), it is left
// alone and no earlier turn is scanned.
func TestSynthesizeReasoningEchoAlreadyHasAliasNoRewrite(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok"}, // echo-less
			map[string]any{"role": "assistant", "content": "now", "reasoning": "I already reasoned"},
			map[string]any{"role": "tool", "content": "out", "tool_call_id": "c1"},
		},
	})
	def := &provider.Def{Name: "oc", EchoReasoning: []string{"deepseek-*"}}
	out := synthesizeReasoningEcho(body, "deepseek-v4.1-flash", def)
	if string(out) != string(body) {
		t.Fatalf("already-echoed turn must pass through untouched:\n got %s\nwant %s", out, body)
	}
}
