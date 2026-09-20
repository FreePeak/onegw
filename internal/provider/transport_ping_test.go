package provider

import (
	"net/http"
	"testing"
	"time"
)

// The 2026-09-10 16:29-16:48 b-ai storm: every account of one provider
// multiplexes onto ONE http2 connection per host, so a degraded connection
// stalled every account simultaneously — each request burned the full
// response-header budget ("http2: timeout awaiting response headers" 504s)
// while fresh connections served instantly. The shared upstream transport
// must run h2 health pings so a dead/stalled connection is dropped in
// ~readIdle+ping instead of letting every request ride it to the timeout.
func TestNewHTTPClientEnablesH2HealthPings(t *testing.T) {
	c := newHTTPClient(120 * time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 120s", tr.ResponseHeaderTimeout)
	}
	// Wiring: the transport must have been h2-configured (TLSNextProto
	// populated) — that is what enables the ping loop below.
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) == 0 {
		t.Fatal("transport not h2-configured: no health pings possible")
	}
	// Helper contract, on a fresh transport (ConfigureTransports is
	// once-per-transport): pings must be armed and returned non-nil.
	h2 := configureHTTP2(&http.Transport{})
	if h2 == nil {
		t.Fatal("configureHTTP2 returned nil")
	}
	if h2.ReadIdleTimeout != 30*time.Second || h2.PingTimeout != 15*time.Second {
		t.Fatalf("h2 health pings misconfigured: readIdle=%v ping=%v", h2.ReadIdleTimeout, h2.PingTimeout)
	}
}
