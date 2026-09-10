package provider

// Decode-speed tracking: an EWMA of output tokens per second, observed per
// (provider, model) for combo steering and per account inside a pool for
// account preference, plus one provider-wide sample for the dashboard.
//
// It is a routing hint, not accounting: tiny answers and sub-200ms replies
// are ignored (they poison the average), and a sample older than staleAfter
// resets the average — yesterday's speed must not steer today's traffic.

import (
	"sync"
	"time"
)

const (
	// speedAlpha folds each new sample in at 25% weight.
	speedAlpha = 0.25
	// Smallest sample we fold in: below this the tokens/seconds quotient is
	// noise (a 5-token keepalive, a stub reply).
	minStreamTokens = 4
	minStreamTime   = 200 * time.Millisecond
	// staleAfter resets the EWMA when no sample arrived within the window.
	staleAfter = 10 * time.Minute
	// maxSpeedModels bounds the per-model map: config-defined models plus
	// whatever clients route directly; past the cap new models are not
	// tracked (cardinality bound, same property as the metrics registry).
	maxSpeedModels = 64
)

type speedSample struct {
	v    float64   // EWMA of output tokens/sec
	n    int64     // samples folded in
	last time.Time // last sample time
}

func (s *speedSample) observe(out int64, d time.Duration, now time.Time) {
	if out < minStreamTokens || d < minStreamTime {
		return
	}
	v := float64(out) / d.Seconds()
	if s.n == 0 || now.Sub(s.last) > staleAfter {
		s.v = v
	} else {
		s.v = speedAlpha*v + (1-speedAlpha)*s.v
	}
	s.n++
	s.last = now
}

// tps reports the smoothed decode speed; 0 when nothing (fresh) has been
// observed — callers treat 0 as "no data, keep configured order".
func (s *speedSample) tps() float64 { return s.v }

// speedState holds a Def's per-model samples plus the provider-wide one.
type speedState struct {
	mu      sync.Mutex
	all     speedSample
	byModel map[string]*speedSample
}

// ObserveSpeed folds one completed request into the provider-wide, per-model,
// and (when acct is non-nil) per-account speed samples.
func (d *Def) ObserveSpeed(acct *Account, model string, outTokens int64, dDur time.Duration) {
	now := time.Now()
	if acct != nil {
		d.pool.observeSpeed(acct, outTokens, dDur, now)
	}
	d.speed.mu.Lock()
	defer d.speed.mu.Unlock()
	d.speed.all.observe(outTokens, dDur, now)
	if d.speed.byModel == nil {
		d.speed.byModel = make(map[string]*speedSample)
	}
	m, ok := d.speed.byModel[model]
	if !ok {
		if len(d.speed.byModel) >= maxSpeedModels {
			return
		}
		m = &speedSample{}
		d.speed.byModel[model] = m
	}
	m.observe(outTokens, dDur, now)
}

// ModelTPS reports the smoothed decode speed of one model on this provider;
// 0 = no data.
func (d *Def) ModelTPS(model string) float64 {
	d.speed.mu.Lock()
	defer d.speed.mu.Unlock()
	if m, ok := d.speed.byModel[model]; ok {
		return m.tps()
	}
	return 0
}

// ProviderTPS reports the provider-wide smoothed decode speed; 0 = no data.
func (d *Def) ProviderTPS() float64 {
	d.speed.mu.Lock()
	defer d.speed.mu.Unlock()
	return d.speed.all.tps()
}

// SpeedSamples reports how many requests folded into the provider-wide
// speed sample (dashboard confidence hint).
func (d *Def) SpeedSamples() int64 {
	d.speed.mu.Lock()
	defer d.speed.mu.Unlock()
	return d.speed.all.n
}

// SpeedRow is one account's speed for the dashboard.
type SpeedRow struct {
	Account string
	TPS     float64
	Samples int64
}

// SpeedRows snapshots per-account speeds, fastest first. Accounts with no
// data are omitted.
func (d *Def) SpeedRows() []SpeedRow {
	return d.pool.speedRows()
}

// observeSpeed folds one completed request into the account's sample.
func (p *accountPool) observeSpeed(a *Account, out int64, d time.Duration, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accts {
		s := &p.accts[i]
		if s.acct.Name == a.Name && s.acct.APIKey == a.APIKey {
			s.speed.observe(out, d, now)
			return
		}
	}
}

// speedRows snapshots per-account speeds, fastest first, under the pool lock.
func (p *accountPool) speedRows() []SpeedRow {
	p.mu.Lock()
	defer p.mu.Unlock()
	rows := make([]SpeedRow, 0, len(p.accts))
	seen := map[string]bool{}
	for i := range p.accts {
		s := &p.accts[i]
		if s.speed.n == 0 {
			continue
		}
		// Weighted slots share one account; report it once.
		k := s.acct.Name + "\x00" + s.acct.APIKey
		if seen[k] {
			continue
		}
		seen[k] = true
		rows = append(rows, SpeedRow{Account: s.acct.Name, TPS: s.speed.tps(), Samples: s.speed.n})
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].TPS > rows[j-1].TPS; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	return rows
}
