package types

import "testing"

// Live 2026-09-11 06:14Z (seq 595, b-ai/glm-5.3-flash, account linh.mn):
// the one-api distributor answered 503 "No available channel for model
// glm-5.3-flash under group default (distributor)" — its upstream channel
// pool for the model was empty. Every key of the provider hits the same
// empty pool, so the error must classify shared: Router.Execute falls
// through to the next combo leg after ONE doomed call (no same-target
// retry, no key ladder), and edgeFault exempts it from the flap breaker.
// Per the 1da3c2e contract there is deliberately NO wording park.
func TestChannelEmpty503IsSharedWall(t *testing.T) {
	const live = "No available channel for model glm-5.3-flash under group default (distributor) (request id: 20260911061408451165665c955d5687nrIByVO)"
	e := &APIError{Status: 503, Type: "api_error", Message: live}
	if !e.SharedConcurrency() {
		t.Fatal("channel-empty 503 must classify shared: every key hits the same empty pool")
	}
	if !e.Retryable() {
		t.Fatal("503 must stay retryable so combos fall through")
	}
	// The phrase is matched on the 503 lane only: a 429 carrying it keeps
	// the per-key ladder (no live evidence that shape exists), and unrelated
	// 503 wordings stay ordinary-retryable.
	if (&APIError{Status: 429, Message: "no available channel"}).SharedConcurrency() {
		t.Fatal("429 with channel wording must stay on the per-key ladder")
	}
	if (&APIError{Status: 503, Message: "model glm-5.3-flash is unavailable, try another endpoint"}).SharedConcurrency() {
		t.Fatal("unrelated 503 wording must not classify shared")
	}
}
