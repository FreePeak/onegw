package server

import (
	"testing"
	"time"
)

// Wire-check: the [server] response_header_timeout knob must reach every
// built provider Def (the per-Def HTTP client is memoized from it).
func TestHeaderTimeoutKnobWiring(t *testing.T) {
	cfg := makeCfg(t, "k", "pw", false, providerSpec{name: "p1", up: "http://127.0.0.1:1", model: "m"})
	cfg.Server.ResponseHeaderTimeout = "90s"
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	def, ok := srv.cur().pool.Get("p1")
	if !ok {
		t.Fatal("provider p1 missing")
	}
	if def.HeaderTimeout != 90*time.Second {
		t.Fatalf("HeaderTimeout = %v, want 90s", def.HeaderTimeout)
	}
}
