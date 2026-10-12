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

	"onegw/internal/router"
)

// delivery is the client-experienced clock for one request. Created at
// handler entry (start = when the request arrived), model filled in once
// the client's model string is parsed; carried on the request context so
// the single success relay site can fold the sample.
type delivery struct {
	start time.Time
	model string
	// failover records that the resolved route has a sibling target, so a
	// failure found before the first client byte can be replaced by
	// Router.Execute's fall-through. Set once the route is known; read by
	// the relay's corrupt-stream guard, which only withholds a stream head
	// when a retry can actually serve the request (see corruptHoldBytes).
	failover bool
	// requested is the model string the CLIENT named (combo name, alias,
	// "provider/model" or a bare id) — the "what I asked for" half of the
	// X-OneGW-Decision header. Empty when the caller has no client model.
	requested string
	// firstProvider/firstModel are the resolution's own FIRST target: the
	// leg this request would have been served by with no fall-through.
	// routeKnown says they were recorded at all (a caller that resolved
	// without a delivery clock leaves it false, and no fallback is claimed).
	firstProvider string
	firstModel    string
	routeKnown    bool
}

type deliveryKey struct{}

func withDelivery(ctx context.Context, d *delivery) context.Context {
	return context.WithValue(ctx, deliveryKey{}, d)
}

func deliveryFrom(ctx context.Context) *delivery {
	d, _ := ctx.Value(deliveryKey{}).(*delivery)
	return d
}

// recordRoute notes the client's model string and the resolution's first
// target, which is what setDecisionHeader reports as request=/fallback=.
// Called once per request, at the same place failover is set (so the two
// answers about the route can never disagree).
func (d *delivery) recordRoute(requested string, res *router.Resolution) {
	if d == nil {
		return
	}
	d.requested = requested
	if res != nil && len(res.Targets) > 0 {
		d.firstProvider, d.firstModel, d.routeKnown = res.Targets[0].Provider, res.Targets[0].Model, true
	}
}

// requestedModel is the client's own model string, "" when none was recorded
// (a headerless caller, or a route that never resolved).
func (d *delivery) requestedModel() string {
	if d == nil {
		return ""
	}
	return d.requested
}

// servedFallback reports whether the target about to serve differs from the
// route's own first target. An unrecorded route is NOT a fallback: nothing
// was skipped to serve this request.
func (d *delivery) servedFallback(providerName, model string) bool {
	if d == nil || !d.routeKnown {
		return false
	}
	return d.firstProvider != providerName || d.firstModel != model
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
	tps, rps, combined, ttftMs float64 // delivered tok/s, req/s, combined and TTFT EWMAs
	n                          int64
	last                       time.Time
}

// DeliveredRow is one client model's snapshot for /metrics and dashboards.
type DeliveredRow struct {
	Model    string
	TPS      float64 // delivered tokens/sec EWMA
	RPS      float64 // requests/sec EWMA (1/wall per completed request)
	Combined float64 // TPS + RPS
	TTFTMs   float64 // first-byte latency EWMA (ms)
	Samples  int64
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
	ws := wall.Seconds()
	v := float64(out) / ws
	r := 1 / ws // one completed request per its wall: req/s
	c := v + r
	tf := float64(ttft.Milliseconds())
	if s.n == 0 || now.Sub(s.last) > deliveredStale {
		s.tps, s.rps, s.combined, s.ttftMs = v, r, c, tf
	} else {
		s.tps = deliveredAlpha*v + (1-deliveredAlpha)*s.tps
		s.rps = deliveredAlpha*r + (1-deliveredAlpha)*s.rps
		s.combined = deliveredAlpha*c + (1-deliveredAlpha)*s.combined
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
		out = append(out, DeliveredRow{Model: m, TPS: s.tps, RPS: s.rps, Combined: s.combined, TTFTMs: s.ttftMs, Samples: s.n})
	}
	return out
}
