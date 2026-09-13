package main

import (
	"runtime/debug"
	"runtime/metrics"
	"testing"

	"onegw/internal/config"
)

// goMemLimitBytes reads back the process-wide soft heap limit actually in
// effect, so the test proves applyMemoryTuning is wired to the configured
// buffered-byte budget, not just that the helper computes a number.
func goMemLimitBytes(t *testing.T) int64 {
	t.Helper()
	s := []metrics.Sample{{Name: "/gc/gomemlimit:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		t.Fatalf("/gc/gomemlimit:bytes kind = %v", s[0].Value.Kind())
	}
	return int64(s[0].Value.Uint64())
}

func TestApplyMemoryTuningFollowsBufferBudget(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		name   string
		budget int64
		want   int64
	}{
		// The shipped 48 MiB budget keeps the historic 90 MiB ceiling: the
		// 100 MB RSS envelope must not drift for operators who never touch
		// buffered_budget_bytes.
		{"default budget keeps 90 MiB envelope", 48 * mib, 90 * mib},
		// A raised budget must move the GC ceiling with it, or the gateway
		// thrashes GC against a limit smaller than the reservation it allows.
		{"200 MiB budget gets headroom above it", 200 * mib, 250 * mib},
		{"zero budget falls back to the envelope", 0, 90 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOMEMLIMIT", "")
			applyMemoryTuning(&config.Config{Server: config.Server{BufferCap: tc.budget}})
			if got := goMemLimitBytes(t); got != tc.want {
				t.Fatalf("gomemlimit = %d bytes, want %d", got, tc.want)
			}
		})
	}
}

// Tuning is a default, never an override: with GOMEMLIMIT present the runtime
// keeps whatever limit it parsed at boot, so applyMemoryTuning must leave it
// untouched (asserted by sentinel, because env set mid-process is inert).
func TestApplyMemoryTuningRespectsOperatorGOMEMLIMIT(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "64MiB")
	const sentinel = int64(1234567890)
	debug.SetMemoryLimit(sentinel)
	applyMemoryTuning(&config.Config{Server: config.Server{BufferCap: 200 << 20}})
	if got := goMemLimitBytes(t); got != sentinel {
		t.Fatalf("gomemlimit = %d bytes, want the pre-existing %d", got, sentinel)
	}
}
