package server

// Client-experienced throughput tests: the delivered tok/s + TTFT EWMA that
// answers "how fast did the client actually receive its answer" — output
// tokens of the winning attempt over the WHOLE request wall, so failed
// attempts, account rotation and retry backoff all sit in the denominator.
// The per-leg decode EWMA (provider speed.go) is per-attempt; this is
// per-request, which is why the ring row and the client gauges carry it.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/config"
)

func TestDeliveredTrackerEWMAAndGates(t *testing.T) {
	tk := &deliveredTracker{}
	now := time.Now()

	tk.observe("free", 0, time.Second, time.Second, now)          // no output tokens: no sample
	tk.observe("free", 3, time.Second, 100*time.Millisecond, now) // below minDeliveredTok: no sample
	if got := tk.rows(); len(got) != 0 {
		t.Fatalf("gated samples must not appear, got %+v", got)
	}

	// 10 tokens in 1s -> 10 tok/s delivered, 100ms TTFT.
	tk.observe("free", 10, time.Second, 100*time.Millisecond, now.Add(time.Minute))
	// 30 tokens in 1s -> 30 tok/s; EWMA blends at deliveredAlpha (0.25).
	tk.observe("free", 30, time.Second, 300*time.Millisecond, now.Add(2*time.Minute))
	rows := tk.rows()
	if len(rows) != 1 || rows[0].Model != "free" {
		t.Fatalf("rows=%+v, want one free row", rows)
	}
	wantTPS := 0.25*30 + 0.75*10
	if rows[0].TPS < wantTPS-1e-9 || rows[0].TPS > wantTPS+1e-9 {
		t.Fatalf("TPS=%v, want %v", rows[0].TPS, wantTPS)
	}
	wantTTFT := 0.25*300 + 0.75*100
	if rows[0].TTFTMs < wantTTFT-1e-9 || rows[0].TTFTMs > wantTTFT+1e-9 {
		t.Fatalf("TTFTMs=%v, want %v", rows[0].TTFTMs, wantTTFT)
	}
	if rows[0].Samples != 2 {
		t.Fatalf("Samples=%d, want 2", rows[0].Samples)
	}
}

func TestDeliveredTrackerStaleResets(t *testing.T) {
	tk := &deliveredTracker{}
	now := time.Now()
	tk.observe("free", 10, time.Second, 100*time.Millisecond, now)
	// Past deliveredStale: the old average must not steer today.
	tk.observe("free", 200, time.Second, 100*time.Millisecond, now.Add(deliveredStale+time.Minute))
	rows := tk.rows()
	if len(rows) != 1 || rows[0].TPS < 199 {
		t.Fatalf("stale sample must reset the EWMA, got %+v", rows[0])
	}
}

func TestDeliveredTrackerCardinalityCap(t *testing.T) {
	tk := &deliveredTracker{}
	now := time.Now()
	for i := range maxDeliveredKeys + 20 {
		tk.observe("m"+string(rune('a'+i)), 10, time.Second, time.Second, now.Add(time.Duration(i)*time.Second))
	}
	if rows := tk.rows(); len(rows) > maxDeliveredKeys {
		t.Fatalf("rows=%d exceeds cap %d", len(rows), maxDeliveredKeys)
	}
}

func TestDeliveryContextRoundTrip(t *testing.T) {
	if deliveryFrom(context.Background()) != nil {
		t.Fatal("empty ctx must carry no delivery")
	}
	d := &delivery{start: time.Now(), model: "dev"}
	ctx := withDelivery(context.Background(), d)
	got := deliveryFrom(ctx)
	if got == nil || got.model != "dev" {
		t.Fatalf("deliveryFrom=%+v, want dev delivery", got)
	}
	got.model = "free" // pointer: later mutation is visible to the relay site
	if got2 := deliveryFrom(ctx); got2.model != "free" {
		t.Fatalf("mutation must be visible through ctx, got %q", got2.model)
	}
}

// TestDeliveredFoldedEndToEnd drives a real request through the handler:
// the delivery clock starts at proxy() entry and the relay site must fold
// the winning attempt's tokens over the WHOLE wall into the per-client-model
// EWMA AND stamp the ring row with the client view.
func TestDeliveredFoldedEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		time.Sleep(15 * time.Millisecond) // make the wall measurable
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":2,"completion_tokens":12,"total_tokens":14}}`))
	}))
	defer up.Close()

	srv, h := newTestServer2(t, gatedCfg(t, config.ProviderCfg{
		Name: "p1", Kind: "openai", BaseURL: up.URL, Models: []string{"m1"},
		Accounts: []config.Acct{{Name: "a1", APIKey: "key-ok"}},
	}))
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	req = authSk(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	rows := srv.m.delivered.rows()
	if len(rows) != 1 || rows[0].Model != "m1" || rows[0].TPS <= 0 || rows[0].TTFTMs <= 0 {
		t.Fatalf("delivered not folded end-to-end: %+v", rows)
	}
	entries := srv.reqlog.latest(1)
	if entries[0].E2EMs <= 0 || entries[0].DTps <= 0 {
		t.Fatalf("ring row missing client view: %+v", entries[0])
	}
	if e := srv.reqlog.latest(1)[0]; e.E2EMs < 15 {
		t.Fatalf("e2e_ms=%d, want >= stub sleep 15ms", e.E2EMs)
	}
}
