package saver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/translat"
)

// ---------------------------------------------------------------------------
// Injection: mode prompts, glob matching, idempotency, surgical edits
// ---------------------------------------------------------------------------

func TestInjectPromptHonesty(t *testing.T) {
	// Shipped prompts must not lie (no false persona claims) and must
	// demand conciseness. The custom prompt ships verbatim.
	if strings.Contains(cavemanPrompt, "You are") || strings.Contains(tersePrompt, "You are") ||
		strings.Contains(ponytailPrompt, "You are") {
		t.Fatalf("shipped prompt makes a false persona claim: %q / %q", cavemanPrompt, tersePrompt)
	}
	if !strings.Contains(cavemanPrompt, "concise") && !strings.Contains(cavemanPrompt, "terse") {
		t.Fatalf("caveman prompt does not demand terseness")
	}
	custom := InjectCfg{Mode: "custom", Text: "reply in exactly three words"}.prompt()
	if !strings.Contains(custom, "reply in exactly three words") {
		t.Fatalf("custom text not shipped verbatim: %q", custom)
	}
	for _, p := range []string{cavemanPrompt, tersePrompt, ponytailPrompt} {
		if !strings.Contains(p, injectMarker) {
			t.Fatalf("prompt missing idempotency marker")
		}
	}
}

func TestInjectModelGlobMatching(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{
		{Mode: "terse", Models: []string{"gpt-5*"}},
		{Mode: "caveman", Models: []string{"claude-*"}},
	}})
	oa := func(model string) []byte {
		return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
	}
	out := s.InjectRaw(translat.FmtOpenAI, oa("gpt-5.5"), "gpt-5.5")
	if !strings.Contains(string(out), tersePrompt) {
		t.Fatalf("gpt-5.5 not matched by gpt-5* glob")
	}
	out = s.InjectRaw(translat.FmtOpenAI, oa("claude-sonnet-4-5"), "claude-sonnet-4-5")
	if !strings.Contains(string(out), cavemanPrompt) {
		t.Fatalf("claude not matched by claude-* glob")
	}
	out = s.InjectRaw(translat.FmtOpenAI, oa("gemini-3-pro"), "gemini-3-pro")
	if strings.Contains(string(out), injectMarker) {
		t.Fatalf("unmatched model was injected")
	}
	// First matching rule wins.
	first := New(Config{Inject: []InjectCfg{{Mode: "caveman"}, {Mode: "terse"}}})
	out = first.InjectRaw(translat.FmtOpenAI, oa("any"), "any")
	if !strings.Contains(string(out), cavemanPrompt) || strings.Contains(string(out), tersePrompt) {
		t.Fatalf("first-match-wins violated")
	}
}

func TestInjectIdempotentAcrossRetries(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{{Mode: "terse"}}})
	orig := []byte(`{"model":"m","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`)
	once := s.InjectRaw(translat.FmtOpenAI, orig, "m")
	if !strings.Contains(string(once), tersePrompt) {
		t.Fatalf("first injection missing")
	}
	twice := s.InjectRaw(translat.FmtOpenAI, once, "m")
	if string(twice) != string(once) {
		t.Fatalf("double injection: directive added again")
	}
	// Retries after a hot reload that swaps the mode must also not inject
	// a second directive into an already-marked body.
	s.SetEnabled(false)
	s.SetEnabled(true)
	reloaded := New(Config{Inject: []InjectCfg{{Mode: "caveman"}}})
	out := reloaded.InjectRaw(translat.FmtOpenAI, once, "m")
	if string(out) != string(once) {
		t.Fatalf("mode swap re-injected into marked body")
	}
}

func TestInjectSameFormatPreservesOtherBytes(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{{Mode: "terse"}}})
	raw := []byte(`{"model":"m","temperature":0.123456789,"stream":true,"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}],"metadata":{"user_id":"u1"}}`)
	out := s.InjectRaw(translat.FmtOpenAI, raw, "m")
	var probe struct {
		Temperature json.Number `json:"temperature"`
		Stream      bool        `json:"stream"`
		Metadata    struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("injected body invalid: %v", err)
	}
	if probe.Temperature.String() != "0.123456789" {
		t.Fatalf("numeric fidelity lost: %s", probe.Temperature)
	}
	if !probe.Stream || probe.Metadata.UserID != "u1" {
		t.Fatalf("sibling fields lost: %+v", probe)
	}
	if len(probe.Messages) != 3 || probe.Messages[0].Role != "system" {
		t.Fatalf("directive not prepended: %d messages", len(probe.Messages))
	}
	if !strings.Contains(string(probe.Messages[0].Content), tersePrompt) {
		t.Fatalf("prepended message is not the directive")
	}
	if !strings.Contains(string(probe.Messages[1].Content), "be nice") {
		t.Fatalf("existing system message lost")
	}
	// The directive goes in front; the client system text survives intact.
}

func TestInjectAnthropicSurface(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{{Mode: "terse"}}})
	// string system
	out := s.InjectRaw(translat.FmtAnthropic, []byte(`{"model":"m","system":"be nice","max_tokens":8,"messages":[]}`), "m")
	var probe struct {
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if !strings.HasPrefix(probe.System, tersePrompt) || !strings.Contains(probe.System, "be nice") {
		t.Fatalf("anthropic string system not merged: %q", probe.System)
	}
	if probe.MaxTokens != 8 {
		t.Fatalf("max_tokens lost")
	}
	// block-array system
	out = s.InjectRaw(translat.FmtAnthropic, []byte(`{"model":"m","system":[{"type":"text","text":"be nice"}],"messages":[]}`), "m")
	var probe2 struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(out, &probe2); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if len(probe2.System) != 2 || !strings.Contains(probe2.System[0].Text, tersePrompt) || probe2.System[1].Text != "be nice" {
		t.Fatalf("anthropic block system not prepended: %+v", probe2.System)
	}
	// idempotency via block array
	again := s.InjectRaw(translat.FmtAnthropic, out, "m")
	if string(again) != string(out) {
		t.Fatalf("anthropic re-injection")
	}
}

func TestInjectGeminiSurface(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{{Mode: "terse"}}})
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	out := s.InjectRaw(translat.FmtGemini, raw, "gemini-3-pro")
	var probe struct {
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Contents []any `json:"contents"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if probe.SystemInstruction == nil || len(probe.SystemInstruction.Parts) != 1 ||
		!strings.Contains(probe.SystemInstruction.Parts[0].Text, tersePrompt) {
		t.Fatalf("gemini systemInstruction not created: %+v", probe.SystemInstruction)
	}
	if len(probe.Contents) != 1 {
		t.Fatalf("contents disturbed")
	}
	// idempotency
	if again := s.InjectRaw(translat.FmtGemini, out, "m"); string(again) != string(out) {
		t.Fatalf("gemini re-injection")
	}
}

func TestInjectNoMessagesLeavesBodyUntouched(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{{Mode: "terse"}}})
	raw := []byte(`{"model":"m"}`) // no messages array: don't invent structure
	if out := s.InjectRaw(translat.FmtOpenAI, raw, "m"); string(out) != string(raw) {
		t.Fatalf("invented messages array: %s", out)
	}
	if out := s.InjectRaw(translat.FmtOpenAI, []byte("not json"), "m"); string(out) != "not json" {
		t.Fatalf("non-object body must pass through verbatim")
	}
}

func TestInjectPonytailMode(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{{Mode: "ponytail"}}})
	orig := []byte(`{"model":"m","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`)
	out := s.InjectRaw(translat.FmtOpenAI, orig, "m")
	// Assert on a quote-free prefix: the full prompt contains quotes that
	// JSON-escape, so it never appears verbatim in the marshalled body.
	if !strings.Contains(string(out), injectMarker+" (ponytail; adapted from DietrichGebert/ponytail, MIT): ") ||
		!strings.Contains(string(out), "the best code is the code never written") {
		t.Fatalf("ponytail ladder not injected")
	}
	if !strings.Contains(string(out), "YAGNI") || !strings.Contains(string(out), "minimum code that works") {
		t.Fatalf("ponytail ladder incomplete: rungs missing")
	}
	if !strings.Contains(string(out), "be nice") {
		t.Fatalf("client system prompt dropped by injection")
	}
	twice := s.InjectRaw(translat.FmtOpenAI, out, "m")
	if string(twice) != string(out) {
		t.Fatalf("ponytail ladder injected twice")
	}
	// The prompt must not contain its own skip signature ("lazy senior
	// dev"): a future edit adding the phrase would make the gateway
	// treat its own directive as a client-side plugin and refuse to
	// re-inject after a hot-reload mode swap.
	if strings.Contains(ponytailPrompt, ponytailClientSig) {
		t.Fatalf("ponytail prompt self-matches the client-plugin skip signature")
	}
}

func TestInjectPonytailSkipsClientPlugin(t *testing.T) {
	s := New(Config{Inject: []InjectCfg{{Mode: "ponytail"}}})
	// A client that already runs the ponytail plugin carries its
	// ruleset tagline in the prompt; the gateway must not stack the
	// ladder on top of it.
	orig := []byte(`{"model":"m","messages":[{"role":"system","content":"Ponytail, lazy senior dev mode. 1. YAGNI. 2. Reuse."},{"role":"user","content":"hi"}]}`)
	if out := s.InjectRaw(translat.FmtOpenAI, orig, "m"); string(out) != string(orig) {
		t.Fatalf("injected on top of a client-side ponytail ruleset: %s", out)
	}
	// The skip is ponytail-specific: terse mode still injects.
	terse := New(Config{Inject: []InjectCfg{{Mode: "terse"}}})
	if out := terse.InjectRaw(translat.FmtOpenAI, orig, "m"); !strings.Contains(string(out), tersePrompt) {
		t.Fatalf("ponytail signature wrongly suppressed terse injection")
	}
}

// ---------------------------------------------------------------------------
// External compress: success, fail-open, timeout, fail-closed
// ---------------------------------------------------------------------------

func externalStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func bigBody(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[{"role":"user","content":"`)
	b.WriteString(strings.Repeat("x", n))
	b.WriteString(`"}]}`)
	return []byte(b.String())
}

func TestExternalCompressSuccess(t *testing.T) {
	up := externalStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/compress" {
			t.Errorf("compress service got path %q", r.URL.Path)
		}
		var req struct {
			Messages json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": json.RawMessage(`[{"role":"user","content":"SUMMARY:x"}]`)})
	})
	defer up.Close()
	s := New(Config{External: ExternalCfg{Enabled: true, URL: up.URL + "/v1/compress", MinBytes: 1, TimeoutMS: 500}})
	out, err := s.CompressExternal(context.Background(), bigBody(40000))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "SUMMARY:x") {
		t.Fatalf("messages not replaced: %.120s", out)
	}
	var probe struct {
		Messages []any `json:"messages"`
	}
	_ = json.Unmarshal(out, &probe)
	if len(probe.Messages) != 1 {
		t.Fatalf("expected compressed single message")
	}
}

func TestExternalCompressFailOpenOnHTTPError(t *testing.T) {
	up := externalStub(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer up.Close()
	s := New(Config{External: ExternalCfg{Enabled: true, URL: up.URL, MinBytes: 1}})
	raw := bigBody(40000)
	out, err := s.CompressExternal(context.Background(), raw)
	if err != nil {
		t.Fatalf("fail-open must not return error: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("fail-open must pass body through")
	}
}

func TestExternalCompressFailOpenOnTimeout(t *testing.T) {
	up := externalStub(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	})
	defer up.Close()
	s := New(Config{External: ExternalCfg{Enabled: true, URL: up.URL, MinBytes: 1, TimeoutMS: 30}})
	raw := bigBody(40000)
	start := time.Now()
	out, err := s.CompressExternal(context.Background(), raw)
	if err != nil {
		t.Fatalf("timeout must fail open: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("timeout must pass body through")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout did not bound the wait: %v", time.Since(start))
	}
}

func TestExternalCompressFailClosedWhenConfigured(t *testing.T) {
	up := externalStub(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer up.Close()
	no := false
	s := New(Config{External: ExternalCfg{Enabled: true, URL: up.URL, MinBytes: 1, FailOpen: &no}})
	raw := bigBody(40000)
	if _, err := s.CompressExternal(context.Background(), raw); err == nil {
		t.Fatalf("fail_open=false must surface the error")
	}
}

func TestExternalCompressNeverGrowsPayload(t *testing.T) {
	up := externalStub(t, func(w http.ResponseWriter, r *http.Request) {
		// Broken service: echoes a bigger messages array than it got.
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": json.RawMessage(`[{"role":"user","content":"` + strings.Repeat("y", 90000) + `"}]`)})
	})
	defer up.Close()
	s := New(Config{External: ExternalCfg{Enabled: true, URL: up.URL, MinBytes: 1}})
	raw := bigBody(40000)
	out, err := s.CompressExternal(context.Background(), raw)
	if err != nil {
		t.Fatalf("grow-guard must fail open: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("payload growth must not be forwarded")
	}
}

func TestExternalCompressSkipsSmallAndDisabled(t *testing.T) {
	calls := 0
	up := externalStub(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	})
	defer up.Close()
	off := New(Config{External: ExternalCfg{URL: up.URL, MinBytes: 1}})
	if out, err := off.CompressExternal(context.Background(), bigBody(40000)); err != nil || string(out) != string(bigBody(40000)) {
		t.Fatalf("disabled hook must pass through: %v", err)
	}
	on := New(Config{External: ExternalCfg{Enabled: true, URL: up.URL, MinBytes: 50000}})
	if _, err := on.CompressExternal(context.Background(), bigBody(40000)); err != nil {
		t.Fatalf("small body must pass through: %v", err)
	}
	if calls != 0 {
		t.Fatalf("compress service called %d times; hook must skip disabled/small", calls)
	}
	// Gemini contents-only body: hook is messages-shaped, skip silently.
	if _, err := on.CompressExternal(context.Background(), []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)); err != nil {
		t.Fatalf("contents body: %v", err)
	}
	if calls != 0 {
		t.Fatalf("contents body must not hit the service")
	}
}

func TestExternalDefaultsFilled(t *testing.T) {
	got := (&Config{}).fill()
	if got.External.TimeoutMS != 2000 || got.External.MinBytes != 32768 {
		t.Fatalf("defaults not applied: %+v", got.External)
	}
	if !got.External.failOpen() {
		t.Fatalf("fail_open default must be true")
	}
}
