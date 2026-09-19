package router

import (
	"context"
	"testing"

	"onegw/internal/provider"
)

// TestSetEvalConfig verifies that SetEvalConfig toggles
// verdict-driven combo reorder on and off without panicking.
// T3: the router calls TypeSafe's own /v1/systemone when
// strategy="jev-eval" and an eval body is tagged via
// WithEvalBody; off (nil evals) must be byte-identical.
func TestSetEvalConfig(t *testing.T) {
	pool := newTestPool()
	r := New(pool)
	// Default: off
	if r.evalStrategy != "" {
		t.Errorf("default strategy = %q, want \"\"", r.evalStrategy)
	}
	if r.evals != nil {
		t.Errorf("default evals = %v, want nil", r.evals)
	}
	// Toggle on
	r.SetEvalConfig(map[string]*provider.EvalCfg{"test-combo": {
		Questions: []string{"question-1"},
	}}, "jev-eval")
	if r.evalStrategy != "jev-eval" {
		t.Errorf(`strategy = %q, want "jev-eval"`, r.evalStrategy)
	}
	if len(r.evals) != 1 {
		t.Errorf("evals len = %d, want 1", len(r.evals))
	}
	// Toggle off
	r.SetEvalConfig(nil, "")
	if r.evalStrategy != "" || r.evals != nil {
		t.Errorf(`after reset: strategy=%q evals=%v`, r.evalStrategy, r.evals)
	}
}

// TestEvalBodyFrom verifies the context-tagged eval body.
func TestEvalBodyFrom(t *testing.T) {
	ctx := context.Background()
	// Absent
	if _, ok := EvalBodyFrom(ctx); ok {
		t.Error("EvalBodyFrom on clean ctx = ok, want false")
	}
	// Present
	body := []byte(`{"model":"x","messages":[]}`)
	ctx = WithEvalBody(ctx, body)
	got, ok := EvalBodyFrom(ctx)
	if !ok {
		t.Fatal("EvalBodyFrom after WithEvalBody = !ok")
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch: %q", got)
	}
	// nil context
	if _, ok := EvalBodyFrom(nil); ok {
		t.Error("EvalBodyFrom(nil) = ok, want false")
	}
}
