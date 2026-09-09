package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// The 2026-09-09 live evidence: with >=20 sessions in flight the Tencent
// model-wide wall ("Concurrency limit 1200") keeps surfacing on
// b-ai/glm-5.3-flash. The wall is shared by ALL keys and ALL accounts of
// the provider, so a same-target retry (different key, same model) can only
// re-hit it after burning a 1s backoff; a different MODEL sits in a
// different concurrency bucket. The router must fall through to the next
// combo target immediately instead of retrying the saturated model.
func TestExecuteSharedConcurrencyFallsThroughImmediately(t *testing.T) {
	r := New(newTestPool())
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("stack")
	calls := map[string]int{}
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls[def.Name]++
		if def.Name == "p1" {
			return nil, &types.APIError{Status: 429, Type: "upstream_error",
				Message: "The request rate exceeds the current model Concurrency limit 1200. Please reduce the request frequency or contact Tencent Cloud support to request a higher limit."}
		}
		return "ok", nil
	}
	start := time.Now()
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("expected success via the next combo target, got %v", got)
	}
	if calls["p1"] != 1 || calls["p2"] != 1 {
		t.Fatalf("shared wall must skip the same-target retry: %v", calls)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("fall-through must not wait out the shared-wall backoff, took %v", time.Since(start))
	}
}

// A direct route (single target) has no next model to hop to: one attempt,
// then the shared-wall 429 surfaces with the honest 2s Retry-After so the
// client's SDK backs off instead of instant-failing.
func TestExecuteSharedConcurrencyDirectRouteSurfacesWithRetryAfter(t *testing.T) {
	r := New(newTestPool())
	res, _ := r.Resolve("p1/m1")
	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		return nil, &types.APIError{Status: 429, Type: "upstream_error",
			Message: "The request rate exceeds the current model Concurrency limit 1200."}
	}
	got := r.Execute(context.Background(), res, caller, func(a any) {})
	if got == nil || got.Status != 429 || !got.SharedConcurrency() {
		t.Fatalf("direct route must surface the shared-wall 429, got %+v", got)
	}
	if got.RetryAfter != "2" {
		t.Fatalf("Retry-After = %q, want 2", got.RetryAfter)
	}
	if calls != 1 {
		t.Fatalf("shared wall must not burn retries on the same model, calls=%d", calls)
	}
}

// Guard against over-reach: an ordinary per-account 429 (no "concurrency
// limit" phrasing) keeps its same-target retry — the cooldown ladder and
// the 250ms tier do the rotation work there, and account rotation DOES
// help that class.
func TestExecuteOrdinary429StillRetriesSameTarget(t *testing.T) {
	r := New(newTestPool())
	res, _ := r.Resolve("p1/m1")
	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		return nil, &types.APIError{Status: 429, Type: "upstream_error",
			Message: "rate limit exceeded, key sk-x"}
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got == nil || !strings.Contains(got.Message, "rate limit") {
		t.Fatalf("expected the 429 to surface: %+v", got)
	}
	if calls != 2 { // MaxAttempts
		t.Fatalf("ordinary per-account 429 must keep the same-target retry, calls=%d", calls)
	}
}

// Live 2026-09-09 (tokenrouter/z-ai/glm-5.3-free @linh, 21:37): the
// engine's cold-prefill admission wall surfaces as a 429
// "BackendAdmissionRejected: Engine cold-request admission rejected …
// policies=prefill_pressure …" and as 503s ("cache-only admission
// rejected a cold, unavailable, or overloaded request", "gateway
// overloaded: …"). The budget is the ENGINE's shared uncached-prefill
// pool — the identical request succeeded seconds later on the OTHER
// account (ring: 429 @harvey 15:01:19 -> 200 @linh in=159561 15:01:23) —
// so same-target retries only re-send the same huge uncached prefill
// into the same saturated engine. Fall through immediately, like the
// Tencent concurrency wall.
func TestExecuteAdmissionWallFallsThroughImmediately(t *testing.T) {
	const admission429 = "BackendAdmissionRejected: Engine cold-request admission rejected: dp_rank=0, policies=prefill_pressure, queued_uncached_tokens=0, inflight_uncached_tokens=0, outstanding_uncached_tokens=0, incoming_uncached_tokens=214293, pending_uncached_prefill_tokens=214293, max_outstanding_uncached_prefill_tokens=200000"
	cases := []struct {
		name string
		err  *types.APIError
	}{
		{"429 cold-request admission rejected", &types.APIError{Status: 429, Type: "upstream_error", Message: admission429}},
		{"503 cache-only admission", &types.APIError{Status: 503, Type: "Service Unavailable", Message: "cache-only admission rejected a cold, unavailable, or overloaded request"}},
		{"503 gateway overloaded hard concurrency", &types.APIError{Status: 503, Type: "ServiceUnavailable", Message: "gateway overloaded: hard concurrency limit reached"}},
		{"503 gateway overloaded cache-aware", &types.APIError{Status: 503, Type: "ServiceUnavailable", Message: "gateway overloaded: cache-aware admission is unavailable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(newTestPool())
			r.SetCombos([]*Combo{{
				Name: "free",
				Targets: []Target{
					{Provider: "p1", Model: "m1"},
					{Provider: "p2", Model: "m2"},
				},
			}})
			res, _ := r.Resolve("free")
			calls := map[string]int{}
			caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
				calls[def.Name]++
				if def.Name == "p1" {
					return nil, tc.err
				}
				return "ok", nil
			}
			if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
				t.Fatalf("expected success via the next combo target, got %v", got)
			}
			if calls["p1"] != 1 || calls["p2"] != 1 {
				t.Fatalf("admission wall must skip the same-target retry: %v", calls)
			}
		})
	}
}

// Direct route (no next combo rung): the admission wall surfaces as-is
// with the honest 2s shared-wall Retry-After, and the upstream 503s keep
// their own status instead of being re-stamped.
func TestExecuteAdmissionWallDirectRouteSurfacesWithRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		err  *types.APIError
		code int
	}{
		{"429 admission", &types.APIError{Status: 429, Type: "upstream_error",
			Message: "BackendAdmissionRejected: Engine cold-request admission rejected: policies=prefill_pressure"}, 429},
		{"503 cache-only admission", &types.APIError{Status: 503, Type: "Service Unavailable",
			Message: "cache-only admission rejected a cold, unavailable, or overloaded request"}, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(newTestPool())
			res, _ := r.Resolve("p1/m1")
			calls := 0
			caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
				calls++
				return nil, tc.err
			}
			got := r.Execute(context.Background(), res, caller, func(a any) {})
			if got == nil || got.Status != tc.code || !got.SharedConcurrency() {
				t.Fatalf("direct route must surface the admission wall, got %+v", got)
			}
			if got.RetryAfter != "2" {
				t.Fatalf("Retry-After = %q, want 2", got.RetryAfter)
			}
			if calls != 1 {
				t.Fatalf("admission wall must not burn retries on the same engine, calls=%d", calls)
			}
		})
	}
}
