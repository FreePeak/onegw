package provider

import (
	"testing"
	"time"
)

// TestPrefillBucketBoundaries pins the size classification the ordering gate
// and the sample store both depend on. A wrong boundary silently mixes a
// short prompt's prefill with a 200K-token session's, which is exactly the
// contamination this store exists to avoid.
func TestPrefillBucketBoundaries(t *testing.T) {
	cases := []struct {
		in   int64
		want int
	}{
		{0, 0}, {511, 0}, {31_999, 0},
		{32_000, 1}, {127_999, 1},
		{128_000, 2}, {262_000, 2}, {1_000_000, 2},
	}
	for _, c := range cases {
		if got := PrefillBucket(c.in); got != c.want {
			t.Errorf("PrefillBucket(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

// TestPrefillObserveGatesNoise pins the noise floor: tiny or sub-50ms samples
// must not seed a bucket, otherwise a trivial prompt's instant prefill makes a
// queued lane look fast.
func TestPrefillObserveGatesNoise(t *testing.T) {
	d := &Def{Name: "p"}
	d.ObservePrefill("m", PrefillBucket(100), 100, time.Second)                 // below minPrefillTokens
	d.ObservePrefill("m", PrefillBucket(200_000), 200_000, 10*time.Millisecond) // below minPrefillTime
	if s := d.ModelPrefillSamples("m", PrefillBucket(200_000)); s != 0 {
		t.Fatalf("gated samples must not seed the bucket, got n=%d", s)
	}
	if tps := d.ModelPrefillTPS("m", PrefillBucket(200_000)); tps != 0 {
		t.Fatalf("no-data bucket must report 0, got %v", tps)
	}
	d.ObservePrefill("m", PrefillBucket(200_000), 200_000, 10*time.Second) // 20k tok/s, admissible
	if s := d.ModelPrefillSamples("m", PrefillBucket(200_000)); s != 1 {
		t.Fatalf("admissible sample not folded: n=%d", s)
	}
	if tps := d.ModelPrefillTPS("m", PrefillBucket(200_000)); tps < 19_000 || tps > 21_000 {
		t.Fatalf("prefill tps=%v want ~20000", tps)
	}
}

// TestPrefillStalenessResets guards the "yesterday's speed must not steer
// today's traffic" contract shared with the decode EWMA: after the staleness
// window the next sample replaces the average instead of blending with it.
func TestPrefillStalenessResets(t *testing.T) {
	d := &Def{Name: "p"}
	d.ObservePrefill("m", PrefillBucket(200_000), 200_000, 20*time.Second) // 10k tok/s
	var s [prefillBuckets]prefillSample
	s[PrefillBucket(200_000)].observe(200_000, 20*time.Second, time.Now())
	// Fold a much faster sample after the staleness window.
	s[2].observe(200_000, 2*time.Second, time.Now().Add(prefillStaleAfter+time.Minute))
	if got := s[2].v; got < 99_000 {
		t.Fatalf("stale sample must be replaced, not blended: v=%v", got)
	}
}

// TestPredictSecondsFavorsFastPrefill is the core of the size-aware ordering:
// at 200K input the leg with the measured-fast prefill must be predicted
// ahead of the leg with the better decode rate. Mutation check: if
// PredictSeconds ignored prefill (decode-only), the slow-prefill leg would
// score ~102s and the fast-prefill leg ~18s, so this assertion fails.
func TestPredictSecondsFavorsFastPrefill(t *testing.T) {
	fastPrefill := &Def{Name: "fastprefill"}
	slowPrefill := &Def{Name: "slowprefill"}
	// Measured shape from api.b.ai 2026-09-11 at ~200K tokens:
	// glm-5.3-flash prefill 4.2-7.6s (≈33k tok/s), decode ≈54 tok/s;
	// qwen3.8-flash prefill up to 63.7s (≈3k tok/s), decode ≈92 tok/s.
	for range MinPrefillSamplesForOrdering {
		fastPrefill.ObservePrefill("m", PrefillBucket(200_000), 200_000, 6*time.Second)
		slowPrefill.ObservePrefill("m", PrefillBucket(200_000), 200_000, 63*time.Second)
	}
	fastPrefill.ObserveSpeed(nil, "m", 540, 10*time.Second) // 54 tok/s decode
	slowPrefill.ObserveSpeed(nil, "m", 920, 10*time.Second) // 92 tok/s decode

	fast := fastPrefill.PredictSeconds("m", 200_000)
	slow := slowPrefill.PredictSeconds("m", 200_000)
	if fast <= 0 || slow <= 0 {
		t.Fatalf("predicted seconds must be positive: fast=%v slow=%v", fast, slow)
	}
	if fast >= slow {
		t.Fatalf("fast-prefill leg must win at 200K input: fast=%vs slow=%vs", fast, slow)
	}
	// Sanity on the arithmetic: 200k/33.3k ≈ 6s prefill + ~4.7s decode ≈ 10.7s.
	if fast > 20 {
		t.Fatalf("fast leg predicted %vs, want ≈6s prefill + nominal decode", fast)
	}
}

// TestPredictSecondsNeedsSamples pins the confidence gate: one sample is not
// evidence, so ordering must keep the configured order until the minimum is
// reached (otherwise a single outlier flips the chain).
func TestPredictSecondsNeedsSamples(t *testing.T) {
	d := &Def{Name: "p"}
	d.ObservePrefill("m", PrefillBucket(200_000), 200_000, 6*time.Second)
	if n := d.ModelPrefillSamples("m", PrefillBucket(200_000)); n != 1 {
		t.Fatalf("expected 1 sample, got %d", n)
	}
	if d.ModelPrefillSamples("m", PrefillBucket(200_000)) >= MinPrefillSamplesForOrdering {
		t.Fatal("one sample must not satisfy the ordering gate")
	}
	// A different size bucket has its own (empty) samples: prefill speed is a
	// property of (lane, size), not of the lane alone.
	if n := d.ModelPrefillSamples("m", PrefillBucket(5_000)); n != 0 {
		t.Fatalf("small-request bucket must be independent, got n=%d", n)
	}
}

// TestPrefillModelCapBoundsCardinality mirrors the decode EWMA's cardinality
// bound: client-supplied model strings must not grow the map without limit.
func TestPrefillModelCapBoundsCardinality(t *testing.T) {
	d := &Def{Name: "p"}
	for i := range maxPrefillModels + 10 {
		d.ObservePrefill(string(rune('a'+i%26))+string(rune('a'+i/26)), PrefillBucket(200_000), 200_000, 5*time.Second)
	}
	d.prefill.mu.Lock()
	n := len(d.prefill.byModel)
	d.prefill.mu.Unlock()
	if n > maxPrefillModels {
		t.Fatalf("prefill map grew past the cap: %d > %d", n, maxPrefillModels)
	}
}

// TestPrefillBucketComesFromTheEstimate pins the defect found while
// verifying this live on 2026-09-11: a request the gateway estimated at
// 40,000 tokens actually measured 27,814, and those two numbers fall in
// different buckets. Storage keyed on the ACTUAL count while the router
// looked up the ESTIMATE bucket, so the lookup found no samples and
// size-aware ordering silently never engaged — a steering feature that
// exists in the code but does nothing in production, invisible in every
// metric. The contract is now: bucket = the estimate the router will use,
// rate = the accurate token count.
func TestPrefillBucketComesFromTheEstimate(t *testing.T) {
	const est, actual = int64(40_000), int64(27_814)
	if PrefillBucket(est) == PrefillBucket(actual) {
		t.Skip("fixture no longer straddles a bucket boundary; pick sizes that do")
	}
	// Wrong side of the contract: stored under the actual-token bucket.
	wrong := &Def{Name: "p"}
	for range MinPrefillSamplesForOrdering {
		wrong.ObservePrefill("m", PrefillBucket(actual), actual, 8*time.Second)
	}
	if got := wrong.ModelPrefillSamples("m", PrefillBucket(est)); got != 0 {
		t.Fatalf("fixture invalid: estimate bucket should be empty, got n=%d", got)
	}
	if secs := wrong.PredictSeconds("m", est); secs != 0 {
		t.Fatalf(" PredictSeconds must report no-data for a mismatched bucket, got %v", secs)
	}
	// Right side: stored under the estimate bucket, rate from the actual count.
	right := &Def{Name: "p"}
	for range MinPrefillSamplesForOrdering {
		right.ObservePrefill("m", PrefillBucket(est), actual, 8*time.Second)
	}
	secs := right.PredictSeconds("m", est)
	if secs <= 0 {
		t.Fatalf("steering must engage when both sides key on the estimate, got %v", secs)
	}
	// The rate itself still uses the accurate token count, not the estimate:
	// 27,814 actual / 8s ≈ 3.5k tok/s, so a 40K estimate predicts ~11.5s.
	if secs < 8 || secs > 15 {
		t.Fatalf("predicted %vs, want ≈11.5s from the accurate rate x the estimate", secs)
	}
}
