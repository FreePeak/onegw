package router

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"onegw/internal/provider"
)

// sizeAwarePool is two legs with the measured api.b.ai shape (2026-09-11,
// ~200K-token requests): the configuration lists the slow-prefill leg first,
// and that leg also has the better decode number — so decode-only steering
// keeps it, while size-aware steering must move it behind the fast-prefill
// leg.
//
// (glm-5.3-flash: prefill 4.2-7.6s / decode ~54 tok/s;
// qwen3.8-flash: prefill up to 63.7s / decode ~92 tok/s.)
func sizeAwarePool(t *testing.T, prefillSamples int) *provider.Pool {
	t.Helper()
	p := provider.NewPool()
	slow := &provider.Def{Name: "slowprefill", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}}}
	fast := &provider.Def{Name: "fastprefill", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "b", APIKey: "k2"}}}
	bucket := provider.PrefillBucket(200_000)
	for range prefillSamples {
		slow.ObservePrefill("m", bucket, 200_000, 63*time.Second) // ≈3k tok/s
		fast.ObservePrefill("m", bucket, 200_000, 6*time.Second)  // ≈33k tok/s
	}
	slow.ObserveSpeed(nil, "m", 920, 10*time.Second) // 92 tok/s decode (best)
	fast.ObserveSpeed(nil, "m", 540, 10*time.Second) // 54 tok/s decode (worse)
	p.Set(slow)
	p.Set(fast)
	return p
}

func comboResolution() *Resolution {
	return &Resolution{
		IsCombo: true, SpeedOrder: true, Model: "stack",
		Targets: []Target{
			{Provider: "slowprefill", Model: "m"},
			{Provider: "fastprefill", Model: "m"},
		},
	}
}

// TestReorderBySpeedUsesPrefillForLargeRequests is the behavior this change
// exists for: at 200K input tokens the leg with the best decode rate took
// 73.7s end to end on the live gateway because its prefill was ~10x slower,
// so ordering must follow predicted wall time, not decode.
//
// Mutation check: disable the size-aware branch (score on decode alone) and
// this fails — the configured slow-prefill leg stays first.
func TestReorderBySpeedUsesPrefillForLargeRequests(t *testing.T) {
	r := New(sizeAwarePool(t, provider.MinPrefillSamplesForOrdering))
	res := comboResolution()
	ctx := WithInputSize(context.Background(), 200_000)
	r.reorderBySpeed(ctx, res)
	if got := res.Targets[0].Provider; got != "fastprefill" {
		t.Fatalf("large request must lead with the fast-prefill leg, got %q (order %v)",
			got, targetNames(res))
	}
	if len(res.Targets) != 2 {
		t.Fatalf("reordering must preserve the whole chain, got %v", targetNames(res))
	}
}

// TestReorderBySpeedKeepsDecodeOrderForSmallRequests pins the gate: below
// provider.PrefillMattersAt tokens the decode EWMA stays the predictor, so the
// fastest-decode leg leads even though prefill data exists.
func TestReorderBySpeedKeepsDecodeOrderForSmallRequests(t *testing.T) {
	r := New(sizeAwarePool(t, provider.MinPrefillSamplesForOrdering))
	res := comboResolution()
	r.reorderBySpeed(WithInputSize(context.Background(), 5_000), res)
	if got := res.Targets[0].Provider; got != "slowprefill" {
		t.Fatalf("small request must keep decode-ranked order (slowprefill has 92 tok/s), got %q (%v)",
			got, targetNames(res))
	}
}

// TestReorderBySpeedRequiresSamples pins the confidence gate: without enough
// prefill samples the configured order must survive, so a single outlier
// cannot flip the chain.
func TestReorderBySpeedRequiresSamples(t *testing.T) {
	r := New(sizeAwarePool(t, provider.MinPrefillSamplesForOrdering-1))
	res := comboResolution()
	r.reorderBySpeed(WithInputSize(context.Background(), 200_000), res)
	if got := res.Targets[0].Provider; got != "slowprefill" {
		t.Fatalf("below the sample gate the configured order must hold, got %q (%v)",
			got, targetNames(res))
	}
}

// TestReorderBySpeedNoSizeOnContext pins the back-compat path: callers that
// never tag a size (headerless/test paths) keep decode-only behavior.
func TestReorderBySpeedNoSizeOnContext(t *testing.T) {
	r := New(sizeAwarePool(t, provider.MinPrefillSamplesForOrdering))
	res := comboResolution()
	r.reorderBySpeed(context.Background(), res)
	if got := res.Targets[0].Provider; got != "slowprefill" {
		t.Fatalf("untagged size must keep decode-only ordering, got %q (%v)", got, targetNames(res))
	}
}

// TestReorderBySpeedWithoutPrefillDataUnaffected guards the pre-existing
// contract for pools that have never folded a prefill sample.
func TestReorderBySpeedWithoutPrefillDataUnaffected(t *testing.T) {
	p := provider.NewPool()
	slowDecode := &provider.Def{Name: "a", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}}}
	fastDecode := &provider.Def{Name: "b", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "b", APIKey: "k2"}}}
	slowDecode.ObserveSpeed(nil, "m", 100, 10*time.Second)
	fastDecode.ObserveSpeed(nil, "m", 900, 10*time.Second)
	p.Set(slowDecode)
	p.Set(fastDecode)
	r := New(p)
	res := &Resolution{IsCombo: true, SpeedOrder: true, Targets: []Target{
		{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"},
	}}
	r.reorderBySpeed(WithInputSize(context.Background(), 200_000), res)
	if got := res.Targets[0].Provider; got != "b" {
		t.Fatalf("no prefill data must fall back to decode ordering, got %q", got)
	}
}

// TestReorderBySpeedNoDataLegSortsBehindMeasured pins the mixed-data contract
// the 2026-09-12 00:30 `dev` revert was really about: once ANY leg has prefill
// samples for the size bucket, measured legs rank by predicted wall time and
// the legs WITHOUT samples go behind them, keeping their configured order.
// Before 2026-09-13 a no-data leg scored 0 against the measured legs' negative
// predicted seconds, so it was promoted to the front of the chain — on the
// live gateway that handed a 200K-token request to an unmeasured leg.
//
// Mutation check: restore the size-aware regime's default score to 0 and the
// no-data leg leads again, failing the order assertion.
func TestReorderBySpeedNoDataLegSortsBehindMeasured(t *testing.T) {
	p := provider.NewPool()
	measured := &provider.Def{Name: "measured", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "m", APIKey: "km"}}}
	cold1 := &provider.Def{Name: "cold1", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "c1", APIKey: "k1"}}}
	cold2 := &provider.Def{Name: "cold2", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "c2", APIKey: "k2"}}}
	bucket := provider.PrefillBucket(200_000)
	for range provider.MinPrefillSamplesForOrdering {
		measured.ObservePrefill("m", bucket, 200_000, 63*time.Second) // ≈3k tok/s
	}
	// The no-data legs are the decode favourites (200/180 tok/s vs 54), so
	// only the size-aware regime can sink them.
	measured.ObserveSpeed(nil, "m", 540, 10*time.Second)
	cold1.ObserveSpeed(nil, "m", 2000, 10*time.Second)
	cold2.ObserveSpeed(nil, "m", 1800, 10*time.Second)
	p.Set(measured)
	p.Set(cold1)
	p.Set(cold2)
	r := New(p)

	// Configured order brackets the measured leg with the two cold ones.
	targets := func() *Resolution {
		return &Resolution{IsCombo: true, SpeedOrder: true, Model: "stack", Targets: []Target{
			{Provider: "cold1", Model: "m"},
			{Provider: "measured", Model: "m"},
			{Provider: "cold2", Model: "m"},
		}}
	}
	res := targets()
	r.reorderBySpeed(WithInputSize(context.Background(), 200_000), res)
	want := []string{"measured/m", "cold1/m", "cold2/m"}
	if got := targetNames(res); !slices.Equal(got, want) {
		t.Fatalf("size-aware order = %v, want %v (measured legs first, no-data legs behind in configured order)", got, want)
	}

	// Below PrefillMattersAt the decode EWMA still picks, so the cold legs lead.
	res2 := targets()
	r.reorderBySpeed(WithInputSize(context.Background(), 5_000), res2)
	if got := targetNames(res2); !slices.Equal(got, []string{"cold1/m", "cold2/m", "measured/m"}) {
		t.Fatalf("decode-ranked order = %v, want cold1, cold2, measured", got)
	}
}

// TestStrategySizeAwareReordersOnlyLargeRequests pins the whole point of
// strategy = "size-aware": the chain is re-sorted on measured prefill at
// PrefillMattersAt tokens and above, and left in its CONFIGURED order below
// that — even when a paid leg has the better decode number. That gating is
// what lets a fast-but-paid leg share a chain with free legs without taking
// over the cheap turns, the starvation that took "fastest" out of the
// free-first combos on 2026-09-11.
func TestStrategySizeAwareReordersOnlyLargeRequests(t *testing.T) {
	r := New(sizeAwarePool(t, provider.MinPrefillSamplesForOrdering))
	var speed []string
	r.SpeedLog = func(model, detail string) { speed = append(speed, detail) }

	// Two configured orders, one per regime, so each half fails on its own:
	// the SMALL turn must keep whatever sequence the operator wrote (here the
	// fast-prefill leg leads on purpose), while the LARGE turn is decided by
	// measured prefill (the same leg must win from last place). Without the
	// gate the small turn re-ranks on decode EWMA — slowprefill scores 92
	// tok/s against fastprefill's 54 — failing the first half; never
	// reordering fails the second.
	small := &Resolution{IsCombo: true, PrefillOrder: true, Model: "stack", Targets: []Target{
		{Provider: "fastprefill", Model: "m"},
		{Provider: "slowprefill", Model: "m"},
	}}
	r.reorderBySpeed(WithInputSize(context.Background(), 5_000), small)
	if got := targetNames(small); !slices.Equal(got, []string{"fastprefill/m", "slowprefill/m"}) {
		t.Fatalf("small request must keep the configured chain, got %v", got)
	}
	if len(speed) != 0 {
		t.Fatalf("a gated no-op must not log a decision: %v", speed)
	}

	large := &Resolution{IsCombo: true, PrefillOrder: true, Model: "stack", Targets: []Target{
		{Provider: "slowprefill", Model: "m"},
		{Provider: "fastprefill", Model: "m"},
	}}
	r.reorderBySpeed(WithInputSize(context.Background(), 200_000), large)
	if got := targetNames(large); !slices.Equal(got, []string{"fastprefill/m", "slowprefill/m"}) {
		t.Fatalf("large request must lead with the measured fast-prefill leg, got %v", got)
	}
	if len(speed) != 1 {
		t.Fatalf("the large reorder must log exactly one decision, got %v", speed)
	}
}

// TestResolveSizeAwareStrategyMarksResolution pins the config→router wire: the
// strategy name reaches Execute as PrefillOrder (not SpeedOrder), so the two
// modes cannot silently trade places.
func TestResolveSizeAwareStrategyMarksResolution(t *testing.T) {
	p := provider.NewPool()
	p.Set(&provider.Def{Name: "a", Kind: provider.KindOpenAI, Accounts: []provider.Account{{Name: "x", APIKey: "k"}}})
	p.Set(&provider.Def{Name: "b", Kind: provider.KindOpenAI, Accounts: []provider.Account{{Name: "y", APIKey: "k"}}})
	r := New(p)
	r.SetCombos([]*Combo{{Name: "sa", Strategy: "size-aware", Targets: []Target{
		{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"},
	}}})
	res, err := r.Resolve("sa")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.PrefillOrder || res.SpeedOrder {
		t.Fatalf("size-aware must set PrefillOrder only, got PrefillOrder=%v SpeedOrder=%v", res.PrefillOrder, res.SpeedOrder)
	}
}

// TestSizeAwareReorderLogsSpeedDecision pins the observability contract this
// feature's live verification depends on: when prefill ordering changes a
// combo's chain, exactly one SpeedLog row is emitted and the task-routing
// sink stays untouched. A reorder that silently logs nothing is
// indistinguishable in production from a reorder that never happened.
func TestSizeAwareReorderLogsSpeedDecision(t *testing.T) {
	var speed, task []string
	r := New(sizeAwarePool(t, provider.MinPrefillSamplesForOrdering))
	r.SpeedLog = func(model, detail string) { speed = append(speed, model+"|"+detail) }
	r.TaskLog = func(model, detail string) { task = append(task, model+"|"+detail) }

	res := comboResolution()
	res.Model = "steer"
	r.reorderBySpeed(WithInputSize(context.Background(), 200_000), res)

	if len(speed) != 1 {
		t.Fatalf("want exactly one speed_order decision row, got %d (%v)", len(speed), speed)
	}
	if !strings.Contains(speed[0], "prefill-order") ||
		!strings.Contains(speed[0], "fastprefill/m > slowprefill/m") {
		t.Fatalf("decision detail = %q", speed[0])
	}
	if len(task) != 0 {
		t.Fatalf("prefill ordering must not write to the task-routing sink: %v", task)
	}

	// A small request keeps decode ordering and must stay silent here.
	speed = nil
	res2 := comboResolution()
	res2.Model = "steer"
	r.reorderBySpeed(WithInputSize(context.Background(), 5_000), res2)
	if len(speed) != 0 {
		t.Fatalf("decode-only ordering must not log a prefill decision: %v", speed)
	}
}

func targetNames(res *Resolution) []string {
	out := make([]string, 0, len(res.Targets))
	for _, t := range res.Targets {
		out = append(out, t.Provider+"/"+t.Model)
	}
	return out
}
