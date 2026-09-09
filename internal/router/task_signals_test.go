package router

import (
	"context"
	"encoding/json"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// CollectSignals: raw body → signals across the three surfaces.
// ---------------------------------------------------------------------------

func TestCollectSignalsOpenAI(t *testing.T) {
	body := `{
		"model": "stack",
		"messages": [
			{"role": "system", "content": "You are a debugging assistant. Investigate the root cause."},
			{"role": "user", "content": "please refactor this module"}
		],
		"max_tokens": 9000,
		"reasoning_effort": "HIGH"
	}`
	sig := CollectSignals([]byte(body))
	if sig.Messages != 2 {
		t.Fatalf("messages = %d, want 2", sig.Messages)
	}
	if sig.MaxTokens != 9000 {
		t.Fatalf("max_tokens = %d, want 9000", sig.MaxTokens)
	}
	if sig.Effort != "high" {
		t.Fatalf("effort = %q, want high", sig.Effort)
	}
	if !sig.HeavyKW {
		t.Fatal("debug/refactor/investigate must trip HeavyKW")
	}
	if sig.PromptChars == 0 {
		t.Fatal("prompt chars must count")
	}
	// The size + effort signals classify heavy.
	task := sig.Classify()
	if task.Level != TaskHeavy {
		t.Fatalf("level = %v, want heavy", task.Level)
	}
}

func TestCollectSignalsAnthropic(t *testing.T) {
	body := `{
		"model": "stack",
		"system": [{"type": "text", "text": "Security review: find the vulnerability."}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "audit this auth bypass report"}]}],
		"max_tokens": 64000
	}`
	sig := CollectSignals([]byte(body))
	if !sig.CriticalKW {
		t.Fatalf("security/vulnerability/bypass must trip CriticalKW: %+v", sig)
	}
	if sig.MaxTokens != 64000 {
		t.Fatalf("max_tokens = %d", sig.MaxTokens)
	}
	// 64k output → huge-output → critical.
	if got := sig.Classify().Level; got != TaskCritical {
		t.Fatalf("level = %v, want critical", got)
	}
}

func TestCollectSignalsGemini(t *testing.T) {
	body := `{
		"contents": [
			{"role": "user", "parts": [{"text": "quick question"}]},
			{"role": "model", "parts": [{"text": "sure"}]},
			{"role": "user", "parts": [{"text": "thanks"}]}
		],
		"generationConfig": {"maxOutputTokens": 200}
	}`
	sig := CollectSignals([]byte(body))
	if sig.Messages != 3 {
		t.Fatalf("messages = %d, want 3", sig.Messages)
	}
	if sig.MaxTokens != 200 {
		t.Fatalf("maxTokens = %d, want 200", sig.MaxTokens)
	}
	if !sig.LightKW {
		t.Fatal("quick/thanks must trip LightKW")
	}
	if got := sig.Classify().Level; got != TaskLight {
		t.Fatalf("level = %v, want light", got)
	}
}

func TestCollectSignalsImages(t *testing.T) {
	openai := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}},{"type":"text","text":"what is this"}]}]}`
	sig := CollectSignals([]byte(openai))
	if !sig.HasImage {
		t.Fatal("image_url part must set HasImage")
	}
	gemini := `{"contents":[{"parts":[{"inline_data":{"mime_type":"image/png"}},{"text":"describe"}]}]}`
	sig = CollectSignals([]byte(gemini))
	if !sig.HasImage {
		t.Fatal("inline_data part must set HasImage")
	}
}

func TestCollectSignalsMalformed(t *testing.T) {
	sig := CollectSignals([]byte(`{not json`))
	if sig != (TaskSignals{}) {
		t.Fatalf("malformed body must yield zero signals: %+v", sig)
	}
	// Zero signals classify light (the off/degenerate path is safe).
	if got := sig.Classify().Level; got != TaskLight {
		t.Fatalf("zero signals must classify light, got %v", got)
	}
}

// The keyword probe is bounded: a huge haystack must not blow up the scan
// (size signals still count fully).
func TestCollectSignalsTextBound(t *testing.T) {
	big := make([]byte, 0, 64<<10)
	word := `debug `
	for len(big) < 60<<10 {
		big = append(big, word...)
	}
	body, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"role": "user", "content": string(big)}},
	})
	sig := CollectSignals(body)
	if sig.PromptChars < 60_000 {
		t.Fatalf("prompt chars = %d, want full count", sig.PromptChars)
	}
}

// ---------------------------------------------------------------------------
// Server wiring contract: signals ride the ctx; WithTask/TaskFrom round-trip.
// ---------------------------------------------------------------------------

func TestTaskContextRoundTrip(t *testing.T) {
	sig := TaskSignals{PromptChars: 42, Messages: 2, Tools: 1}
	ctx := WithTask(context.Background(), sig)
	got, ok := TaskFrom(ctx)
	if !ok || got != sig {
		t.Fatalf("TaskFrom = %+v ok=%v, want %+v", got, ok, sig)
	}
	// Absent context (task routing off path).
	if _, ok := TaskFrom(context.Background()); ok {
		t.Fatal("untagged ctx must report ok=false")
	}
	// Nil-safety.
	if _, ok := TaskFrom(nil); ok {
		t.Fatal("nil ctx must report ok=false")
	}
}

// End-to-end at Execute level with a real body: a big debugging prompt
// routed to a combo [cheap, strong] must hit strong first when on.
func TestExecuteWithCollectedSignals(t *testing.T) {
	r := New(tierPool())
	r.SetTaskRouting(true)
	r.SetCombos([]*Combo{taskCombo()})
	body := `{"messages":[{"role":"user","content":"` +
		string(make([]byte, 0)) + `Investigate and debug the architecture. ` +
		"Analyze the root cause across the codebase end-to-end. " +
		`"}],"reasoning_effort":"high"}`
	sig := CollectSignals([]byte(body))
	res := mustResolve(t, r, "stack")
	var first string
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		if first == "" {
			first = def.Name + "/" + model
		}
		return "ok", nil
	}
	if err := r.Execute(WithTask(context.Background(), sig), res, caller, func(a any) {}); err != nil {
		t.Fatal(err)
	}
	if first != "p1/strong" {
		t.Fatalf("heavy debug task must try strong first, got %s", first)
	}
}
