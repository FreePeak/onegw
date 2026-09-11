package server

// Delivered-throughput tracking: the CLIENT-experienced tokens/sec — output
// tokens of the winning attempt over the whole request wall time (handler
// entry → relay end). Failed attempts, cooldown rotations, retry backoff
// and prefill all sit in the denominator, so a combo chain that burns two
// 429s and a 1s backoff before serving reports the true delivery rate the
// user saw, not the winning leg's decode speed. The per-leg decode EWMA
// (provider/speed.go) answers "how fast does this lane stream"; this
// answers "how fast did the client actually receive its answer".
//
// Both TTFT (handler entry → first upstream byte) and delivered tok/s are
// smoothed per CLIENT model string (the routed "provider/model", combo
// name, alias or bare model the client named), because one client prompt
// spans multiple upstream attempts and the client cares about the model it
// asked for, not the leg that happened to win.

import (
	"context"
	"sync"
	"time"
)

// delivery is the client-experienced clock for one request. Created at
// handler entry (start = when the request arrived), model filled in once
// the client's model string is parsed; carried on the request context so
// the single success relay site can fold the sample.
type delivery struct {
	start time.Time
	model string
}

type deliveryKey struct{}

func withDelivery(ctx context.Context, d *delivery) context.Context {
	return context.WithValue(ctx, deliveryKey{}, d)
}

func deliveryFrom(ctx context.Context) *delivery {
	d, _ := ctx.Value(deliveryKey{}).(*delivery)
	return d
}

const (
	// Same alpha / staleness / token gate as the decode EWMA so the two
	// numbers are comparable; NO time floor here — a reply that took 30s
	// to produce 20 tokens is exactly the collapse this metric exists to
	// surface, and tiny/fast replies are excluded by the token gate.
	deliveredAlpha  = 0.25
	minDeliveredTok = 4
	deliveredStale  = 10 * time.Minute
	// maxDeliveredKeys bounds the per-model map (same cardinality
	// property as the metrics registry): boundedModel collapses unroutable
	// client strings to "unresolved", and config-defined routes bound the
	// rest.
	maxDeliveredKeys = 128
)

// deliveredSample is one client model's smoothed view.
type deliveredSample struct {
	tps, ttftMs float64 // delivered tok/s EWMA; first-byte TTFT EWMA (ms)
	n           int64
	last        time.Time
}

// DeliveredRow is one client model's snapshot for /metrics and dashboards.
type DeliveredRow struct {
	Model   string
	TPS     float64 // delivered tokens/sec EWMA
	TTFTMs  float64 // first-byte latency EWMA (ms)
	Samples int64
}

// deliveredTracker holds per-client-model delivered samples.
type deliveredTracker struct {
	mu sync.Mutex
	m  map[string]*deliveredSample
}

// observe folds one completed request into the model's sample. out is the
// winning attempt's output tokens; wall is handler-entry → relay end.
func (t *deliveredTracker) observe(model string, out int64, wall, ttft time.Duration, now time.Time) {
	if t == nil || model == "" || out < minDeliveredTok || wall <= 0 {
		return
	}
	if ttft < 0 {
		ttft = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[string]*deliveredSample)
	}
	s := t.m[model]
	if s == nil {
		if len(t.m) >= maxDeliveredKeys {
			return // cardinality cap: unknown models stop being tracked
		}
		s = &deliveredSample{}
		t.m[model] = s
	}
	v := float64(out) / wall.Seconds()
	tf := float64(ttft.Milliseconds())
	if s.n == 0 || now.Sub(s.last) > deliveredStale {
		s.tps, s.ttftMs = v, tf
	} else {
		s.tps = deliveredAlpha*v + (1-deliveredAlpha)*s.tps
		s.ttftMs = deliveredAlpha*tf + (1-deliveredAlpha)*s.ttftMs
	}
	s.n++
	s.last = now
}

// rows snapshots every tracked model (map iteration order is irrelevant to
// the render: gauges are keyed by model label).
func (t *deliveredTracker) rows() []DeliveredRow {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]DeliveredRow, 0, len(t.m))
	for m, s := range t.m {
		if s.n == 0 {
			continue
		}
		out = append(out, DeliveredRow{Model: m, TPS: s.tps, TTFTMs: s.ttftMs, Samples: s.n})
	}
	return out
}
