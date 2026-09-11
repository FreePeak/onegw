package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onegw/internal/provider"
)

// The tests here pin the runtime-learning deltas layered on top of the
// config-gated synthesis (reasoning_echo_synth_test.go): the first echo
// refusal TEACHES the gateway the (provider, model) contract, the one
// bounded retry it grants carries the newly synthesized body, and every
// later request — buffered or streamed — fills upfront with no wasted
// upstream attempt.

// TestReasoningEchoLearnServesFirstRequest pins the seq-198 flow end to
// end: request 1 misses → learns → the bounded same-target retry carries
// the filled body and serves; request 2 fills upfront (one upstream hit).
func TestReasoningEchoLearnServesFirstRequest(t *testing.T) {
	st := newOpencodeEchoStub()
	defer st.srv.Close()
	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "th", up: st.srv.URL, model: "deepseek-v4.1-flash:free"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// Request 1: refusal (learn) → filled retry → 200. No config globs:
	// the contract comes entirely from the runtime learn.
	w := do(t, h, toolLoopReq(t, "th/deepseek-v4.1-flash:free"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("learn-and-fill must serve request 1, got code=%d body=%s", w.Code, w.Body.String())
	}
	bods := st.bodies()
	if len(bods) != 2 {
		t.Fatalf("upstream hits=%d, want 2 (refusal then filled retry)", len(bods))
	}
	if strings.Contains(string(bods[0]), `"reasoning_content":"(context elided)"`) {
		t.Fatalf("first attempt must replay the client body unfilled, got %s", bods[0])
	}
	if !strings.Contains(string(bods[1]), `"reasoning_content":"(context elided)"`) {
		t.Fatalf("retry must carry the synthesized echo, got %s", bods[1])
	}

	// Request 2: the contract is learned — the FIRST attempt serves.
	w = do(t, h, toolLoopReq(t, "th/deepseek-v4.1-flash:free"))
	if w.Code != 200 {
		t.Fatalf("learned model must fill upfront, got code=%d body=%s", w.Code, w.Body.String())
	}
	bods = st.bodies()
	if len(bods) != 3 {
		t.Fatalf("upstream hits=%d, want 3 (no wasted attempt after learning)", len(bods))
	}
	if !strings.Contains(string(bods[2]), `"reasoning_content":"(context elided)"`) {
		t.Fatalf("upfront fill missing, got %s", bods[2])
	}
}

// TestReasoningEchoLearnContainsRepeatRefusals pins the containment: a
// stubborn echo refusal costs exactly ONE bounded retry (the learn made the
// retry body different), and a repeat refusal — byte-identical body —
// advances after ONE attempt instead of burning MaxAttempts per request.
func TestReasoningEchoLearnContainsRepeatRefusals(t *testing.T) {
	hits := 0
	th := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(reasoningEchoErr))
	}))
	defer th.Close()
	other, otherCap := captureStub()
	defer other.Close()

	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "th", up: th.URL, model: "deepseek-v4.1-flash:free"},
		providerSpec{name: "oc", up: other.URL, model: "mimo-v2.5"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	w := do(t, h, toolLoopReq(t, "pair"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("combo must fall through to the next target, got code=%d body=%s", w.Code, w.Body.String())
	}
	if hits != 2 { // refusal (fresh learn) + one filled retry, then fall through
		t.Fatalf("th hits=%d, want 2 (one learn-driven retry, no more)", hits)
	}
	if b, _, _ := otherCap.snapshot(); len(b) == 0 {
		t.Fatal("next combo leg never hit")
	}

	// Request 2: the learned refusal replays byte-identically — the echo
	// break must fire after ONE attempt, no re-burned retry.
	before := hits
	w = do(t, h, toolLoopReq(t, "pair"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("combo must still fall through on repeat refusals, got code=%d", w.Code)
	}
	if hits != before+1 {
		t.Fatalf("learned target burned %d attempts on repeat refusal, want 1", hits-before)
	}
}

// TestStreamRelayLearnedEchoBuffered pins the streaming gate: the FIRST
// echo refusal on the single-shot relay teaches the model (the honest 400
// is the one the client retries), and every later streaming request rides
// the buffered pipeline where synthesizeReasoningEcho fills the gap.
func TestStreamRelayLearnedEchoBuffered(t *testing.T) {
	st := newOpencodeEchoStub()
	defer st.srv.Close()
	cfg := streamCfg(t, false, nil, providerSpec{name: "th", up: st.srv.URL, model: "deepseek-v4.1-flash:free"})
	_, h := newStreamServer(t, cfg)

	// Attempt 1 rides the fast path, replays verbatim, is refused, and
	// teaches the contract.
	w := do(t, h, toolLoopStreamReq(t, "th/deepseek-v4.1-flash:free"))
	if w.Code != 400 {
		t.Fatalf("first stream attempt must surface the honest refusal, got code=%d", w.Code)
	}
	// Attempt 2 is rerouted buffered and serves with the filled body.
	w = do(t, h, toolLoopStreamReq(t, "th/deepseek-v4.1-flash:free"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("learned stream model must serve buffered, got code=%d body=%s", w.Code, w.Body.String())
	}
	bods := st.bodies()
	if len(bods) != 2 || !strings.Contains(string(bods[1]), `"reasoning_content":"(context elided)"`) {
		t.Fatalf("second attempt must carry the synthesized echo, bodies=%d", len(bods))
	}
}

// TestLearnReasoningEchoGatesRetry pins the retry-grant predicate at the
// Def level: a fresh learn is new, a repeat learn is not — the difference
// between "the retry body changed" and "byte-identical burn".
func TestLearnReasoningEchoGatesRetry(t *testing.T) {
	def := &provider.Def{}
	if !def.LearnReasoningEcho("deepseek-v4.1-flash") {
		t.Fatal("first learn must report fresh")
	}
	if def.LearnReasoningEcho("deepseek-v4.1-flash") {
		t.Fatal("repeat learn must not report fresh")
	}
	if !def.ReasoningEchoModel("deepseek-v4.1-flash") {
		t.Fatal("learned model must be reported as echo-required")
	}
	if def.ReasoningEchoModel("mimo-v2.5") {
		t.Fatal("unrelated model must not be reported as echo-required")
	}
}

// toolLoopStreamReq is toolLoopReq with stream=true: the single-shot relay
// is eligible for it until the model is learned.
func toolLoopStreamReq(t *testing.T, model string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "read it"},
			map[string]any{"role": "assistant", "content": "Now grep:",
				"tool_calls": []any{map[string]any{"id": "c2", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "content": "out2", "tool_call_id": "c2"},
		},
		"stream": true,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-test-key")
	return r
}
