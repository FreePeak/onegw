package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/translat"
	"onegw/internal/types"
)

// TestRelayResponseBudgetSurvivesReloadMidAcquire is the regression guard for
// issue #40: the buffered cross-format path in relayResponse must take its
// Acquire/Release pair on ONE state snapshot. Before dbe02bd the deferred
// Release re-read s.cur() after Acquire returned, so a SIGHUP reload landing
// in between released the bytes on the NEW budget and leaked the reservation
// on the OLD one — a monotonic leak in exactly the counter that bounds RSS
// (the 100 MB contract).
//
// Deterministic trigger: fill the live budget completely so relayResponse's
// cross-format Acquire blocks polling for room; while it blocks, Reload (the
// SIGHUP-equivalent apply swap) installs a new budget, then free the bytes.
// The unblocked Acquire must land on the same snapshot it will later release.
func TestRelayResponseBudgetSurvivesReloadMidAcquire(t *testing.T) {
	cfg1 := makeCfg(t, "key-bud", "pw", false, providerSpec{name: "p1", up: "http://127.0.0.1:1", model: "m1"})
	cfg1.Server.BufferCap = 1024 // 1 KiB: the minimum, small enough to fill from the test
	srv, err := New(cfg1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	// Fill the live budget entirely from the test so relayResponse's
	// cross-format Acquire has to wait for room. The fill stays held until
	// after the reload below — that is what pins relayResponse inside
	// budget.Acquire across the state swap.
	budget := srv.cur().budget
	if err := budget.Acquire(t.Context(), budget.Capacity()); err != nil {
		t.Fatalf("pre-fill acquire: %v", err)
	}

	// Non-streaming client (OpenAI) + Anthropic upstream: a non-ForcedStream
	// kind with differing formats forces the buffered cross-format path.
	res := &provider.CallResult{
		Resp: &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			ContentLength: 1024, // reserve == MaxBody headroom == the whole budget
			Body:          io.NopCloser(strings.NewReader(`{"garbage":true}`)),
		},
		Format: translat.FmtOpenAI,
	}
	w := httptest.NewRecorder()

	done := make(chan *types.APIError, 1)
	go func() {
		done <- srv.relayResponse(w, res, &provider.Def{Name: "p1", Kind: provider.KindOpenAI}, "m",
			translat.FmtOpenAI, translat.FmtAnthropic, false, 64, 0, nil, t.Context())
	}()

	// Wait until relayResponse is genuinely blocked inside budget.Acquire.
	deadline := time.Now().Add(2 * time.Second)
	for budget.Waiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("relayResponse never entered budget.Acquire")
		}
		time.Sleep(time.Millisecond)
	}

	// SIGHUP-equivalent: apply() swaps the whole state atomically, installing
	// a brand-new budget. The in-flight relayResponse keeps running on the
	// old snapshot — that is the contract under test.
	srv.Reload(cfg1)
	newBudget := srv.cur().budget
	if newBudget == budget {
		t.Fatal("reload did not install a new budget; test premise broken")
	}

	// Unblock: the Acquire polling inside relayResponse can now proceed. It
	// must return its reservation to the old snapshot's budget, not to the
	// budget that happens to be current once it unblocks.
	budget.Release(budget.Capacity())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relayResponse never returned after budget freed")
	}

	if held, _ := budget.Stats(); held != 0 {
		t.Fatalf("old budget leaked %d bytes across reload: reservation was released on the new budget instead (issue #40 pattern)", held)
	}
	if held, _ := newBudget.Stats(); held != 0 {
		t.Fatalf("new budget holds %d bytes it never reserved", held)
	}

	// The observable contract: after the reload the live budget is fully
	// available — a fresh full-capacity reservation succeeds immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := newBudget.Acquire(ctx, newBudget.Capacity()); err != nil {
		t.Fatalf("post-reload full-capacity acquire failed: %v", err)
	}
	newBudget.Release(newBudget.Capacity())
}
