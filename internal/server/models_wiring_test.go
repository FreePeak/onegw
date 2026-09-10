package server

import (
	"testing"
)

// Wire-check: every provider's advertised models must reach the router's
// direct-route table as one SetModels call. The historical per-provider loop
// replaced the table wholesale — the LAST provider's list won — and because
// SetModels drops slash-less routes, a config ending in a bare-id provider
// (live 2026-09-10: tokenharbor) emptied the table entirely. Failure rows
// then collapsed advertised slash-models to "unresolved" (tokenrouter 504s
// logged model "unresolved" while the same model labeled fine on success
// rows, which bypass boundedModel). The regression test mirrors that shape:
// a slash-model provider FIRST, an all-bare provider LAST.
func TestAdvertisedModelsSurviveWiring(t *testing.T) {
	up := upstreamStub("m")
	defer up.Close()
	cfg := makeCfg(t, "k", "pw", false,
		providerSpec{name: "tokenrouter", up: up.URL, model: "z-ai/glm-5.3-free"},
		providerSpec{name: "tokenharbor", up: up.URL, model: "th-orchestra"},
	)
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	rt := srv.cur().router

	// Bare "z-ai/glm-5.3-free": the prefix is NOT a provider, so only the
	// advertised-model scan can vouch for it — the exact seq-204 label path.
	if !rt.KnownModel("z-ai/glm-5.3-free") {
		t.Fatal("advertised slash-model must stay known (failure-row label honesty)")
	}
	// The all-bare last provider's id must be known too (it is what emptied
	// the table under the old wiring).
	if !rt.KnownModel("th-orchestra") {
		t.Fatal("last provider's advertised bare model must stay known")
	}
	// Qualified client strings resolve to the advertising provider.
	res, rerr := rt.Resolve("tokenrouter/z-ai/glm-5.3-free")
	if rerr != nil {
		t.Fatalf("Resolve qualified slash-model: %v", rerr)
	}
	if len(res.Targets) != 1 || res.Targets[0].Provider != "tokenrouter" ||
		res.Targets[0].Model != "z-ai/glm-5.3-free" {
		t.Fatalf("qualified route resolved wrong: %+v", res.Targets)
	}
}
