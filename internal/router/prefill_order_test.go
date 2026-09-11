package router

import (
	"context"
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
