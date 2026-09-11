package provider

// Prefill tracking: an EWMA of the pre-first-byte phase (queue + prefill)
// expressed as input tokens per second, bucketed by REQUEST SIZE.
//
// Why it exists. The decode EWMA (speed.go) measures headers→relay-end, and
// combo ordering steers on it. Ring evidence 2026-09-11 shows that steering
// optimizes the wrong term for large requests: a b-ai/qwen3.8-flash request
// with 232,621 input tokens decoded in 0.96s yet took 73.7s end to end — the
// whole cost sat BEFORE the headers. Direct measurement against api.b.ai at
// ~200K input tokens (2026-09-11): glm-5.3-flash prefill 4.2-7.6s (n=4)
// versus qwen3.8-flash 4.4-63.7s (n=6, bimodal — the same lane is sometimes
// fast, sometimes queued a minute). Ordering by decode speed picks a leg on
// a number that does not contain the dominant term.
//
// This is a routing hint like the decode EWMA, not accounting: samples below
// the noise gates are ignored, a stale sample resets rather than steering
// yesterday's traffic, and 0 means "no data, keep configured order".
import (
	"sort"
	"sync"
	"time"
)

const (
	// prefillBuckets splits requests by size: the measured prefill cost is a
	// property of (lane, size), not of the lane alone.
	prefillBuckets = 3
	// Bucket bounds, in input tokens. 32K/128K are the natural split for the
	// observed traffic (a 200K-token session vs a short prompt) and keep the
	// per-bucket sample count high enough to steer on.
	prefillMidTokens   = 32_000
	prefillLargeTokens = 128_000
	// Noise gates: a 200-token prompt prefills in milliseconds, and
	// tokens/0.001s is not a rate anyone queued at.
	minPrefillTokens = 512
	minPrefillTime   = 50 * time.Millisecond
	// Prefill staleness resets the average (same window as the decode EWMA).
	// PrefillMattersAt is the input size from which the pre-first-byte phase
	// can dominate a request's wall time, and therefore from which size-aware
	// ordering may reorder combo legs. Below it the decode EWMA stays the
	// better predictor and ordering is unchanged.
	PrefillMattersAt = prefillMidTokens

	prefillStaleAfter = 10 * time.Minute
	// maxPrefillModels bounds the per-model map (cardinality, same property
	// as the metrics registry).
	maxPrefillModels = 64
)

// PrefillBucket reports which size bucket an input size falls into. Exported
// for tests and the dashboard.
func PrefillBucket(inTokens int64) int {
	switch {
	case inTokens < prefillMidTokens:
		return 0
	case inTokens < prefillLargeTokens:
		return 1
	default:
		return 2
	}
}

type prefillSample struct {
	v    float64 // EWMA of prefill input-tokens/sec
	n    int64
	last time.Time
}

func (s *prefillSample) observe(inTokens int64, d time.Duration, now time.Time) {
	if inTokens < minPrefillTokens || d < minPrefillTime {
		return
	}
	v := float64(inTokens) / d.Seconds()
	if s.n == 0 || now.Sub(s.last) > prefillStaleAfter {
		s.v = v
	} else {
		s.v = speedAlpha*v + (1-speedAlpha)*s.v
	}
	s.n++
	s.last = now
}

type prefillState struct {
	mu      sync.Mutex
	byModel map[string]*[prefillBuckets]prefillSample
}

// ObservePrefill folds one successful attempt's pre-first-byte phase into the
// (model, size-bucket) sample.
//
// dur is measured from the attempt's start to the arrival of the upstream
// response headers. `tokens` is the ACCURATE input size (the upstream-reported
// count) and is used for the rate; `bucket` is the size class and MUST be the
// one the ordering will look up — derived from the same body-size estimate the
// router carries, because the two sides disagreeing about the key silently
// disables the steering (measured 2026-09-11: a 27,814-token request stored
// bucket "small" while the router's 4-bytes-per-token estimate said "mid", so
// the lookup found no samples and prefill ordering never engaged).
func (d *Def) ObservePrefill(model string, bucket int, tokens int64, dur time.Duration) {
	if tokens < minPrefillTokens || dur < minPrefillTime {
		return
	}
	if bucket < 0 || bucket >= prefillBuckets {
		return
	}
	now := time.Now()
	d.prefill.mu.Lock()
	defer d.prefill.mu.Unlock()
	if d.prefill.byModel == nil {
		d.prefill.byModel = make(map[string]*[prefillBuckets]prefillSample)
	}
	m, ok := d.prefill.byModel[model]
	if !ok {
		if len(d.prefill.byModel) >= maxPrefillModels {
			return
		}
		m = &[prefillBuckets]prefillSample{}
		d.prefill.byModel[model] = m
	}
	m[bucket].observe(tokens, dur, now)
}

// ModelPrefillTPS reports the smoothed prefill rate (input tokens/sec) of one
// model for the size bucket the given input size falls into; 0 = no data.
func (d *Def) ModelPrefillTPS(model string, bucket int) float64 {
	if bucket < 0 || bucket >= prefillBuckets {
		return 0
	}
	d.prefill.mu.Lock()
	defer d.prefill.mu.Unlock()
	if m, ok := d.prefill.byModel[model]; ok {
		return m[bucket].v
	}
	return 0
}

// PrefillRow is one (model, size-bucket) prefill observation for the
// dashboard and /metrics.
type PrefillRow struct {
	Model   string
	Bucket  string // "small" | "mid" | "large"
	TPS     float64
	Samples int64
}

// BucketName renders a prefill bucket for labels and the dashboard.
func BucketName(b int) string {
	switch b {
	case 0:
		return "small"
	case 1:
		return "mid"
	default:
		return "large"
	}
}

// PrefillRows snapshots every (model, bucket) with data, fastest first, so an
// operator can see exactly what the size-aware steering knows.
func (d *Def) PrefillRows() []PrefillRow {
	d.prefill.mu.Lock()
	defer d.prefill.mu.Unlock()
	rows := make([]PrefillRow, 0, len(d.prefill.byModel)*prefillBuckets)
	for model, buckets := range d.prefill.byModel {
		for b := range buckets {
			s := &buckets[b]
			if s.n == 0 {
				continue
			}
			rows = append(rows, PrefillRow{Model: model, Bucket: BucketName(b), TPS: s.v, Samples: s.n})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TPS > rows[j].TPS })
	return rows
}

// ModelPrefillSamples reports how many samples back ModelPrefillTPS (the
// ordering gate needs a minimum before it outranks the configured order).
func (d *Def) ModelPrefillSamples(model string, bucket int) int64 {
	if bucket < 0 || bucket >= prefillBuckets {
		return 0
	}
	d.prefill.mu.Lock()
	defer d.prefill.mu.Unlock()
	if m, ok := d.prefill.byModel[model]; ok {
		return m[bucket].n
	}
	return 0
}

// PredictSeconds estimates the whole-request wall for one leg and one request
// size: measured prefill for this bucket plus the decode of a nominal reply at
// the leg's measured decode speed. The nominal output is deliberately fixed —
// the ordering decision is dominated by prefill at the sizes where this
// matters, and a fixed constant keeps the comparison across legs fair. Legs
// with no prefill data for the bucket report 0 and keep their configured
// relative order (same contract as the decode-only steering).
func (d *Def) PredictSeconds(model string, inTokens int64) float64 {
	tps := d.ModelPrefillTPS(model, PrefillBucket(inTokens))
	if tps <= 0 {
		return 0
	}
	total := float64(inTokens) / tps
	if dec := d.ModelTPS(model); dec > 0 {
		total += nominalOutputTokens / dec
	}
	return total
}

// nominalOutputTokens is the reply size assumed when comparing legs on
// predicted wall time: larger than a chat one-liner, smaller than a long
// generation, so neither phase is assumed away.
const nominalOutputTokens = 256.0

// MinPrefillSamplesForOrdering is how many samples a (model, bucket) pair
// needs before size-aware ordering may reorder on it.
const MinPrefillSamplesForOrdering = 3
