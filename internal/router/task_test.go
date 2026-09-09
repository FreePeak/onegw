package router

import (
	"context"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// tierPool builds two providers with declared tiers for task-routing
// tests: p1 = strong reasoning models, p2 = light/cheap models.
func tierPool() *provider.Pool {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}},
		Tiers: []provider.ModelTier{
			{Model: "strong", Power: 95, Vision: true, Reasoning: true, Context: 200_000, MaxOut: 64_000},
			{Model: "strong-novision", Power: 95, Reasoning: true, Context: 200_000},
			{Model: "small-ctx", Power: 90, Reasoning: true, Context: 8_000},
		},
	})
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k2"}},
		Tiers: []provider.ModelTier{
			{Model: "cheap", Power: 30, Vision: true},
			{Model: "cheap-novision", Power: 25},
		},
	})
	return p
}

func taskCombo() *Combo {
	return &Combo{
		Name: "stack",
		Targets: []Target{
			{Provider: "p2", Model: "cheap"},
			{Provider: "p1", Model: "strong"},
		},
	}
}

// ---------------------------------------------------------------------------
// Classifier buckets (taskAwareRouting.ts line-for-line conditions).
// ---------------------------------------------------------------------------

func TestClassifyBuckets(t *testing.T) {
	tests := []struct {
		name string
		sig  TaskSignals
		want TaskLevel
	}{
		{"zero signals default light", TaskSignals{}, TaskLight},
		{"tiny greeting", TaskSignals{PromptChars: 40, Messages: 1}, TaskLight},
		{"translate short", TaskSignals{PromptChars: 300, Messages: 1, MaxTokens: 256}, TaskLight},
		{"light keyword small", TaskSignals{PromptChars: 3500, Messages: 2, LightKW: true}, TaskLight},
		{"light keyword too big", TaskSignals{PromptChars: 5000, Messages: 2, LightKW: true}, TaskStandard},
		{"light keyword with tools", TaskSignals{PromptChars: 3000, Messages: 2, Tools: 1, LightKW: true}, TaskStandard},
		{"light keyword heavy effort", TaskSignals{PromptChars: 3000, Messages: 2, LightKW: true, Effort: "high"}, TaskHeavy},
		{"default mid", TaskSignals{PromptChars: 6000, Messages: 4}, TaskStandard},
		{"one heavy signal only", TaskSignals{PromptChars: 6000, Messages: 20}, TaskStandard},
		{"two heavy signals", TaskSignals{PromptChars: 60_000, Messages: 20}, TaskHeavy},
		{"large context alone", TaskSignals{PromptChars: 60_000}, TaskHeavy},
		{"huge context is critical", TaskSignals{PromptChars: 120_000}, TaskCritical},
		{"high effort alone", TaskSignals{PromptChars: 100, Effort: "high"}, TaskHeavy},
		{"xhigh effort alone", TaskSignals{PromptChars: 100, Effort: "xhigh"}, TaskHeavy},
		{"critical domain keywords", TaskSignals{PromptChars: 10_000, Tools: 3, CriticalKW: true}, TaskCritical},
		{"critical keyword weak request", TaskSignals{PromptChars: 500, CriticalKW: true}, TaskStandard},
		{"huge output", TaskSignals{PromptChars: 100, MaxTokens: 40000}, TaskCritical},
		{"many tools large context", TaskSignals{PromptChars: 20_000, Tools: 8}, TaskCritical},
		{"low effort stays light", TaskSignals{PromptChars: 1500, Effort: "low"}, TaskLight},
		{"heavy keyword small prompt", TaskSignals{PromptChars: 5000, HeavyKW: true}, TaskStandard},
		{"heavy keyword + large context", TaskSignals{PromptChars: 60_000, HeavyKW: true}, TaskHeavy},
		{"critical keyword + high effort", TaskSignals{PromptChars: 100, Effort: "high", CriticalKW: true}, TaskCritical},
		{"critical keyword + big prompt", TaskSignals{PromptChars: 10_000, CriticalKW: true}, TaskCritical},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.sig.Classify()
			if got.Level != tc.want {
				t.Fatalf("level = %v, want %v (reasons %v)", got.Level, tc.want, got.Reasons)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Scoring: base fit + hard-miss penalties + overkill/underkill.
// ---------------------------------------------------------------------------

func TestScoreTargetPenalties(t *testing.T) {
	heavy := TaskSignals{PromptChars: 100, Effort: "high"}.Classify()
	light := TaskSignals{PromptChars: 500, Messages: 1, MaxTokens: 100}.Classify()
	std := TaskSignals{PromptChars: 4000}.Classify() // standard: 4k chars, no other signals
	critical := TaskSignals{PromptChars: 200_000}.Classify()
	imageHeavy := TaskSignals{PromptChars: 100, Effort: "high", HasImage: true}.Classify()

	tests := []struct {
		name  string
		tier  tierMeta
		task  Task
		score int
	}{
		{"exact fit heavy", tierMeta{power: 95, vision: true, reasoning: true, declared: true}, heavy, 100},
		{"vision miss on image request", tierMeta{power: 95, vision: false, reasoning: true, declared: true}, imageHeavy, 100 - 10000},
		{"undeclared vision neutral", tierMeta{power: 95}, imageHeavy, 100},
		{"non-reasoning on heavy", tierMeta{power: 95, vision: true, declared: true}, heavy, 100 - 120},
		{"undeclared no reasoning penalty", tierMeta{power: 95}, heavy, 100},
		{"context overflow", tierMeta{power: 95, vision: true, reasoning: true, context: 1000, declared: true}, std, 70 - 200},
		{"context within budget", tierMeta{power: 95, vision: true, reasoning: true, context: 8000, declared: true}, std, 70},
		{"max_out overflow", tierMeta{power: 95, vision: true, reasoning: true, maxOut: 1000, declared: true},
			TaskSignals{PromptChars: 100, MaxTokens: 2000}.Classify(), 70 - 80},
		{"light overkill strong model", tierMeta{power: 130, vision: true, reasoning: true, declared: true}, light, 5 - 35},
		{"heavy underkill weak model", tierMeta{power: 30, vision: true, reasoning: true, declared: true}, heavy, 35 - 60},
		{"critical underkill", tierMeta{power: 60, vision: true, reasoning: true, declared: true}, critical, 40 - 100},
		{"undeclared neutral power", tierMeta{power: 65}, heavy, 70},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreTarget(tc.tier, tc.task); got != tc.score {
				t.Fatalf("score = %d, want %d", got, tc.score)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Execute-level: off-switch no-op, best-fit first, chain preserved.
// ---------------------------------------------------------------------------

// execOrder resolves "stack" and runs Execute with a caller that fails the
// first target it sees (retryable 429, twice per MaxAttempts) and serves
// the next. The returned sequence shows the effective target order
// including retries: [first, first, second].
func execOrder(t *testing.T, r *Router, ctx context.Context) []string {
	t.Helper()
	res := mustResolve(t, r, "stack")
	var order []string
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		order = append(order, def.Name+"/"+model)
		if len(order) <= 2 { // first target: both MaxAttempts fail
			return nil, &types.APIError{Status: 429, Type: "rate_limit_error", Message: "quota"}
		}
		return "ok", nil
	}
	if err := r.Execute(ctx, res, caller, func(a any) {}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return order
}

func TestTaskRoutingOffIsNoOp(t *testing.T) {
	r := New(tierPool())
	r.SetCombos([]*Combo{taskCombo()})
	// Config order: cheap first. A critical request would flip the order
	// if task routing were on; off must keep the configured order.
	ctx := WithTask(context.Background(), TaskSignals{PromptChars: 100_000, Effort: "high"})
	got := execOrder(t, r, ctx)
	want := []string{"p2/cheap", "p2/cheap", "p1/strong"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("off-switch order = %v, want %v", got, want)
	}
}

func TestTaskRoutingSignalsWithoutSwitchIsNoOp(t *testing.T) {
	// Signals tagged but the router never enabled: still byte-identical.
	r := New(tierPool())
	r.SetCombos([]*Combo{taskCombo()})
	ctx := WithTask(context.Background(), TaskSignals{PromptChars: 100_000})
	got := execOrder(t, r, ctx)
	if got[0] != "p2/cheap" {
		t.Fatalf("signals without switch reordered: %v", got)
	}
}

func TestTaskRoutingReordersBestFitFirst(t *testing.T) {
	r := New(tierPool())
	r.SetTaskRouting(true)
	r.SetCombos([]*Combo{taskCombo()})
	// Critical task: strong model first, but the full chain remains —
	// when the best fit fails, the cheap fallback still serves.
	ctx := WithTask(context.Background(), TaskSignals{PromptChars: 100_000})
	got := execOrder(t, r, ctx)
	want := []string{"p1/strong", "p1/strong", "p2/cheap"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("reordered order = %v, want %v", got, want)
	}
}

func TestTaskRoutingLightKeepsCheapFirst(t *testing.T) {
	r := New(tierPool())
	r.SetTaskRouting(true)
	r.SetCombos([]*Combo{taskCombo()})
	ctx := WithTask(context.Background(), TaskSignals{PromptChars: 300})
	got := execOrder(t, r, ctx)
	if got[0] != "p2/cheap" {
		t.Fatalf("light task should keep cheap first: %v", got)
	}
}

func TestTaskRoutingSingleTargetNoReorder(t *testing.T) {
	r := New(tierPool())
	r.SetTaskRouting(true)
	r.SetModels([]string{"p1/strong"})
	ctx := WithTask(context.Background(), TaskSignals{PromptChars: 100_000})
	res, _ := r.Resolve("p1/strong")
	if err := r.Execute(ctx, res, func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		return "ok", nil
	}, func(a any) {}); err != nil {
		t.Fatal(err)
	}
}

// Vision hard-miss at the Execute level: a declared non-vision model on
// an image request sorts behind the vision-capable one.
func TestTaskRoutingVisionHardMiss(t *testing.T) {
	r := New(tierPool())
	r.SetTaskRouting(true)
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p2", Model: "cheap-novision"},
			{Provider: "p1", Model: "strong"},
		},
	}})
	ctx := WithTask(context.Background(), TaskSignals{PromptChars: 300, HasImage: true})
	got := execOrder(t, r, ctx)
	if got[0] != "p1/strong" {
		t.Fatalf("vision-capable model should be tried first: %v", got)
	}
	// Fallback chain preserved: the non-vision target is still tried.
	if got[len(got)-1] != "p2/cheap-novision" {
		t.Fatalf("chain must keep the penalized target: %v", got)
	}
}

// Stable tie behavior: equal scores keep the original order. Two targets
// with identical tiers must not swap.
func TestTaskRoutingStableTies(t *testing.T) {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}},
		Tiers:    []provider.ModelTier{{Model: "m", Power: 95, Vision: true, Reasoning: true}},
	})
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k2"}},
		Tiers:    []provider.ModelTier{{Model: "m", Power: 95, Vision: true, Reasoning: true}},
	})
	r := New(p)
	r.SetTaskRouting(true)
	r.SetCombos([]*Combo{{
		Name:    "stack",
		Targets: []Target{{Provider: "p1", Model: "m"}, {Provider: "p2", Model: "m"}},
	}})
	ctx := WithTask(context.Background(), TaskSignals{PromptChars: 100_000})
	got := execOrder(t, r, ctx)
	if got[0] != "p1/m" {
		t.Fatalf("tie must keep original order, got %v", got)
	}
}

// Decision logging: only changed orders emit, detail carries level +
// reasons + the new order, and the model label is the client string.
func TestTaskRoutingLog(t *testing.T) {
	r := New(tierPool())
	r.SetTaskRouting(true)
	r.SetCombos([]*Combo{taskCombo()})
	var logs []string
	r.TaskLog = func(model, detail string) {
		logs = append(logs, model+" | "+detail)
	}
	okCaller := func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		return "ok", nil
	}

	// Light request: no order change → no log row.
	r.Execute(WithTask(context.Background(), TaskSignals{PromptChars: 300}),
		mustResolve(t, r, "stack"), okCaller, func(a any) {})
	if len(logs) != 0 {
		t.Fatalf("unchanged order must not log: %v", logs)
	}

	// Heavy request (large context, not critical): order changes → one row.
	r.Execute(WithTask(context.Background(), TaskSignals{PromptChars: 60_000}),
		mustResolve(t, r, "stack"), okCaller, func(a any) {})
	if len(logs) != 1 {
		t.Fatalf("changed order must log once, got %v", logs)
	}
	if logs[0] != "stack | task=heavy [large-context,medium-large-context] p1/strong > p2/cheap" {
		t.Fatalf("log line = %q", logs[0])
	}
}

// No signals in ctx (e.g. caller without WithTask): configured order.
func TestTaskRoutingNoSignalsKeepsOrder(t *testing.T) {
	r := New(tierPool())
	r.SetTaskRouting(true)
	r.SetCombos([]*Combo{taskCombo()})
	got := execOrder(t, r, context.Background())
	if got[0] != "p2/cheap" {
		t.Fatalf("missing signals must keep order: %v", got)
	}
}

func mustResolve(t *testing.T, r *Router, model string) *Resolution {
	t.Helper()
	res, err := r.Resolve(model)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
