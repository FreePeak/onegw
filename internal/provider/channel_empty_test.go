package provider

import (
	"bytes"
	"context"
	"testing"
)

// Live 2026-09-11 06:14Z (seq 595): b-ai 503 "No available channel for
// model glm-5.3-flash under group default (distributor)". The credential
// is healthy and the lane is shared by every key, so Do must NOT bench
// the account (keys stay warm) and must NOT park the (provider, model)
// pair (1da3c2e: wording parks break the fast-path whole-body replay).
// The error rides the shared-wall contract: Router.Execute falls through
// after one doomed call.
func TestChannelEmpty503KeepsKeysWarmNoPark(t *testing.T) {
	const body = `{"error":{"message":"No available channel for model glm-5.3-flash under group default (distributor) (request id: 20260911061408451165665c955d5687nrIByVO)","type":"api_error","code":503}}`
	srv, _ := mkErrStub(t, 503, body)
	def := newSingleDef(t, srv, "b-ai")
	a1 := &def.Accounts[0]
	_, apiErr := def.Do(context.Background(), a1, "glm-5.3-flash", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 503 || !apiErr.SharedConcurrency() {
		t.Fatalf("got %+v, want 503 SharedConcurrency", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() || slot.strikes != 0 {
		t.Fatalf("dead-lane 503 must not bench the healthy key, cooldown=%v strikes=%d", slot.cooldown, slot.strikes)
	}
	if benched, _ := def.ModelBenched("glm-5.3-flash"); benched {
		t.Fatal("wording walls must not park the (provider, model) pair (1da3c2e)")
	}
	if edgeFault(apiErr) {
		t.Fatal("shared-wall 503 must not strike the provider-wide flap breaker")
	}
}
