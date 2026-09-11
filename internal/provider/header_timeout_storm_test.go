package provider

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// Header-timeout storm bench: the gateway's own pre-first-byte abort is
// request-shaped when LONE (one oversized prefill must not exile the model),
// but a storm of them on the same (provider, model) indicts the leg — before
// this mechanism every request re-burned the full budget (b-ai
// 2026-09-10 16:29-16:48: 52 timeouts, zero state recorded, leg #1 first for
// every request). Three timeouts inside the tumbling window bench the leg via
// the existing #72 BenchModel, so Execute skips it at zero cost.
// ---------------------------------------------------------------------------

// stallSrv accepts requests and never answers — every Do rides the per-Def
// header timeout.
func stallSrv(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done(): // the client's header budget aborts the stream
		}
	}))
	t.Cleanup(func() {
		close(release) // release handlers BEFORE Close (t.Cleanup is LIFO)
		srv.Close()
	})
	return srv
}

func doStall(t *testing.T, def *Def, model string) *types.APIError {
	t.Helper()
	_, apiErr := def.Do(context.Background(), &def.Accounts[0], model, nil,
		bytes.NewReader([]byte(`{"model":"`+model+`","messages":[]}`)), false)
	return apiErr
}

func TestHeaderTimeoutStormBenchesModel(t *testing.T) {
	_, def := newDef(t, stallSrv(t).URL)
	def.HeaderTimeout = 200 * time.Millisecond // the per-Def memoized client picks it up
	// Literal 3, NOT headerTimeoutStrikes: a test that follows the constant
	// cannot catch the threshold being raised.
	for range 3 {
		apiErr := doStall(t, def, "glm-5.3-flash")
		if apiErr == nil || apiErr.Status != 504 || !apiErr.NoSameTargetRetry {
			t.Fatalf("want header-budget 504, got %+v", apiErr)
		}
	}
	benched, ready := def.ModelBenched("glm-5.3-flash")
	if !benched {
		t.Fatal("storm of 3 header timeouts must bench the leg")
	}
	if d := time.Until(ready); d <= 0 || d > headerTimeoutBench {
		t.Fatalf("bench TTL %v outside (0, %v]", d, headerTimeoutBench)
	}
	// Sibling models on the same provider keep serving: bench is model-scoped.
	if b, _ := def.ModelBenched("qwen3.8-flash"); b {
		t.Fatal("only the storming model is benched")
	}
	// And the accounts are NOT benched — this indicts the leg, not keys.
	for range def.Accounts {
		a, _ := def.NextAccount("")
		if a == nil {
			t.Fatal("header-timeout storm must not drain the account pool")
		}
	}
}

func TestLoneHeaderTimeoutDoesNotBench(t *testing.T) {
	_, def := newDef(t, stallSrv(t).URL)
	def.HeaderTimeout = 200 * time.Millisecond
	apiErr := doStall(t, def, "glm-5.3-flash")
	if apiErr == nil || apiErr.Status != 504 {
		t.Fatalf("want 504, got %+v", apiErr)
	}
	if b, _ := def.ModelBenched("glm-5.3-flash"); b {
		t.Fatal("a lone request-shaped timeout must not bench the model")
	}
}

func TestHeaderTimeoutWindowTumbles(t *testing.T) {
	_, def := newDef(t, "http://127.0.0.1:1") // never dialed: direct strikes
	base := time.Now()
	def.noteHeaderTimeout("m", base)
	def.noteHeaderTimeout("m", base.Add(2*time.Minute)) // inside window: 2 strikes
	def.noteHeaderTimeout("m", base.Add(5*time.Minute)) // >3min since first: fresh window
	if b, _ := def.ModelBenched("m"); b {
		t.Fatal("strikes spread beyond the window must not bench")
	}
	def.noteHeaderTimeout("m", base.Add(6*time.Minute)) // 2 in the new window
	def.noteHeaderTimeout("m", base.Add(7*time.Minute)) // 3rd: trips
	if b, _ := def.ModelBenched("m"); !b {
		t.Fatal("3 strikes inside one window must bench")
	}
	// Trip resets the window: a lone next strike must not keep it benched
	// forever — verify the reset by counting the new strike only after the
	// bench expires logically (checked via the strikes map, not the bench).
	def.noteHeaderTimeout("m", base.Add(7*time.Minute+time.Second))
	def.htMu.Lock()
	w := def.htStrikes["m"]
	def.htMu.Unlock()
	if w.count != 1 || !w.since.Equal(base.Add(7*time.Minute+time.Second)) {
		t.Fatalf("trip must reset the window, got %+v", w)
	}
}
