// Package metrics is a minimal in-process Prometheus registry: counters and
// gauges backed by atomic.Int64, one mutex only on first-use series creation
// and during scrapes. No dependencies.
//
// Series are created on first use and never evicted; label cardinality is
// bounded by what callers pass (config-defined provider/model plus fixed
// outcome codes), never by request or session volume — the same property as
// the usage tracker.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Family is one metric: name, help text, type (counter | gauge), and a
// fixed-arity label schema. Increments are lock-free atomics on the series
// value; the family mutex is taken only to create a series the first time a
// label set appears and while a scrape snapshots the series map.
type Family struct {
	name       string
	help       string
	kind       string // counter | gauge
	labelNames []string

	mu     sync.RWMutex
	series map[string]*series // key: NUL-joined label values (declared order)
}

type series struct {
	vals []string
	v    atomic.Int64
}

// Registry holds the process's metric families.
type Registry struct {
	mu   sync.Mutex
	fams map[string]*Family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{fams: make(map[string]*Family)}
}

// Counter registers a counter family (monotonically increasing).
func (r *Registry) Counter(name, help string, labelNames ...string) *Family {
	return r.register(name, help, "counter", labelNames)
}

// Gauge registers a gauge family (value may go up and down).
func (r *Registry) Gauge(name, help string, labelNames ...string) *Family {
	return r.register(name, help, "gauge", labelNames)
}

func (r *Registry) register(name, help, kind string, labelNames []string) *Family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.fams[name]; dup {
		panic(fmt.Sprintf("metrics: duplicate family %q", name))
	}
	f := &Family{name: name, help: help, kind: kind, labelNames: labelNames,
		series: make(map[string]*series)}
	r.fams[name] = f
	return f
}

// seriesFor returns the series for vals, creating it on first use. The hot
// path is one read-locked map hit plus an atomic add, but not zero-cost:
// each call builds a key string (one small allocation for a single label
// value, two or more for multi-label families). Fine at request frequency;
// if it ever matters, per-callsite cached series handles would remove it.
func (f *Family) seriesFor(vals []string) *series {
	if len(vals) != len(f.labelNames) {
		panic(fmt.Sprintf("metrics: %s: got %d label values, want %d (%v)",
			f.name, len(vals), len(f.labelNames), f.labelNames))
	}
	key := strings.Join(vals, "\x00")
	f.mu.RLock()
	s, ok := f.series[key]
	f.mu.RUnlock()
	if ok {
		return s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s2, ok := f.series[key]; ok {
		return s2
	}
	s = &series{vals: append([]string(nil), vals...)}
	f.series[key] = s
	return s
}

// Inc adds 1 to the series identified by vals (in label-declaration order).
func (f *Family) Inc(vals ...string) { f.seriesFor(vals).v.Add(1) }

// Add adds delta to the series identified by vals.
func (f *Family) Add(delta int64, vals ...string) { f.seriesFor(vals).v.Add(delta) }

// Set overwrites the series identified by vals (gauges).
func (f *Family) Set(v int64, vals ...string) { f.seriesFor(vals).v.Store(v) }

// Value reports the current value of the series identified by vals (tests
// and scrapers).
func (f *Family) Value(vals ...string) int64 { return f.seriesFor(vals).v.Load() }

// Render returns the Prometheus text exposition format (version 0.0.4):
// one "# HELP"/"# TYPE" header pair per family, then one sample line per
// series. Families are emitted in sorted name order and series in sorted
// label order, so output is deterministic for a given set of values.
func (r *Registry) Render() string {
	r.mu.Lock()
	fams := make([]*Family, 0, len(r.fams))
	for _, f := range r.fams {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })

	var b strings.Builder
	for _, f := range fams {
		f.renderTo(&b)
	}
	return b.String()
}

func (f *Family) renderTo(b *strings.Builder) {
	b.WriteString("# HELP ")
	b.WriteString(f.name)
	b.WriteByte(' ')
	b.WriteString(escapeHelp(f.help))
	b.WriteString("\n# TYPE ")
	b.WriteString(f.name)
	b.WriteByte(' ')
	b.WriteString(f.kind)
	b.WriteByte('\n')

	f.mu.RLock()
	list := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		list = append(list, s)
	}
	f.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		return strings.Join(list[i].vals, "\x00") < strings.Join(list[j].vals, "\x00")
	})
	for _, s := range list {
		b.WriteString(f.name)
		f.writeLabels(b, s.vals)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(s.v.Load(), 10))
		b.WriteByte('\n')
	}
}

// writeLabels emits {name="value",...} with pairs sorted by label name.
func (f *Family) writeLabels(b *strings.Builder, vals []string) {
	if len(vals) == 0 {
		return
	}
	type pair struct{ name, val string }
	pairs := make([]pair, len(vals))
	for i, v := range vals {
		pairs[i] = pair{f.labelNames[i], v}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(p.val))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// escapeLabelValue escapes per the text exposition spec: backslash, double
// quote, and newline. Other bytes pass through verbatim.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\n\"") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 4)
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes a HELP docstring: backslash and newline only (double
// quotes are legal there).
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
