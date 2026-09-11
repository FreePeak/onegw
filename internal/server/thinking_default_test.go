package server

import (
	"encoding/json"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// alwaysThinkingDef builds the b-ai/glm shape: the upstream reasons
// unconditionally, and the operator opted into a cheaper default effort.
func alwaysThinkingDef(defaultEffort string) *provider.Def {
	return &provider.Def{
		Name:           "b-ai",
		Kind:           provider.KindOpenAI,
		AlwaysThinking: []string{"glm-5.3", "glm-5.3-flash"},
		DefaultEffort:  defaultEffort,
	}
}

func effortOf(t *testing.T, body []byte) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	s, _ := root["reasoning_effort"].(string)
	return s
}

// TestDefaultEffortAppliedWhenClientSendsNone is the measured behavior:
// glm-5.3-flash with no knob falls back to the vendor's "max"
// (647/676 output tokens, 395/392 reasoning, 6.2-6.5s in the 2026-09-11
// measurement); with default_effort = "low" the gateway sends "low" itself
// (246/234 output, 57/50 reasoning, 3.3-3.9s).
//
// Mutation check: drop the injection branch and this fails with an empty
// effort, i.e. the upstream's max default comes back.
func TestDefaultEffortAppliedWhenClientSendsNone(t *testing.T) {
	def := alwaysThinkingDef("low")
	in := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	out := adaptThinkingBody(in, "glm-5.3-flash", def)
	if got := effortOf(t, out); got != "low" {
		t.Fatalf("reasoning_effort=%q, want the configured default %q", got, "low")
	}
}

// TestDefaultEffortNeverOverridesClient pins the contract that the knob is a
// DEFAULT, not a policy: an explicit client value survives (coerced to the
// accepted enum, never replaced by the default).
func TestDefaultEffortNeverOverridesClient(t *testing.T) {
	def := alwaysThinkingDef("low")
	in := []byte(`{"model":"glm-5.3-flash","reasoning_effort":"high","messages":[]}`)
	out := adaptThinkingBody(in, "glm-5.3-flash", def)
	if got := effortOf(t, out); got != "high" {
		t.Fatalf("client's high must survive, got %q", got)
	}
}

// TestDefaultEffortSkippedWhenClientExpressedThinking pins the no-invention
// rule for requests that already carry a thinking preference: a client
// sending thinking{type:enabled} made its intent explicit, so the gateway
// must not add a competing effort knob beside it.
func TestDefaultEffortSkippedWhenClientExpressedThinking(t *testing.T) {
	def := alwaysThinkingDef("low")
	in := []byte(`{"model":"glm-5.3-flash","thinking":{"type":"enabled"},"messages":[]}`)
	out := adaptThinkingBody(in, "glm-5.3-flash", def)
	if got := effortOf(t, out); got != "" {
		t.Fatalf("no effort may be invented beside an explicit thinking knob, got %q", got)
	}
}

// TestDefaultEffortOffByDefault pins that an unconfigured provider behaves
// exactly as before this change: the body must come back byte-identical.
func TestDefaultEffortOffByDefault(t *testing.T) {
	def := alwaysThinkingDef("")
	in := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	out := adaptThinkingBody(in, "glm-5.3-flash", def)
	if string(out) != string(in) {
		t.Fatalf("unconfigured provider must forward verbatim:\n got %s\nwant %s", out, in)
	}
}

// TestDefaultEffortScopedToAlwaysThinking pins the scope: on a model whose
// upstream does not force thinking, an absent knob means "do not think", and
// injecting an effort there would change the client's contract.
func TestDefaultEffortScopedToAlwaysThinking(t *testing.T) {
	def := alwaysThinkingDef("low")
	in := []byte(`{"model":"other-model","messages":[]}`)
	out := adaptThinkingBody(in, "other-model", def)
	if string(out) != string(in) {
		t.Fatalf("non-always-thinking model must be untouched: %s", out)
	}
	if eff := def.DefaultEffortFor("other-model"); eff != "" {
		t.Fatalf("DefaultEffortFor must yield \"\" for unscoped models, got %q", eff)
	}
}

// TestDefaultEffortUnifiedPath covers the cross-format branch, where the
// adaptation happens on the decoded struct rather than the raw body.
func TestDefaultEffortUnifiedPath(t *testing.T) {
	def := alwaysThinkingDef("low")
	u := &types.ChatRequest{Model: "glm-5.3-flash"}
	adaptThinkingUnified(u, "glm-5.3-flash", def)
	if u.ReasoningEffort != "low" {
		t.Fatalf("unified path must apply the default, got %q", u.ReasoningEffort)
	}
	// A client-set value still wins on this path too.
	u2 := &types.ChatRequest{Model: "glm-5.3-flash", ReasoningEffort: "high"}
	adaptThinkingUnified(u2, "glm-5.3-flash", def)
	if u2.ReasoningEffort != "high" {
		t.Fatalf("client effort must survive the unified path, got %q", u2.ReasoningEffort)
	}
}

// TestDefaultEffortUnifiedOffByDefault is the unified-path twin of the
// byte-identical guard above.
func TestDefaultEffortUnifiedOffByDefault(t *testing.T) {
	def := alwaysThinkingDef("")
	u := &types.ChatRequest{Model: "glm-5.3-flash"}
	adaptThinkingUnified(u, "glm-5.3-flash", def)
	if u.ReasoningEffort != "" {
		t.Fatalf("unconfigured provider must leave effort empty, got %q", u.ReasoningEffort)
	}
}
