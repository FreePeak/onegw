package main

// Regression for the "already up to date" lie: `onegw update` asks a RUNNING
// gateway what the latest release is, and the gateway answers from its own
// cached probe — refreshed only on [update] check_interval (24 h default). So
// for hours after a release shipped, both `onegw update --check` and
// `onegw update --yes` reported the previous version as current (measured
// 2026-09-16: v0.40.0 published 06:44Z, gateway booted 02:57Z, the CLI still
// said "latest release v0.39.0 — already up to date" at 03:58Z, and a manual
// POST /admin/update made it report v0.40.0 immediately). Both paths now share
// one forced probe before any decision, so this pins the fix for both: the
// apply path reads the same refreshed status.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"onegw/internal/config"
	"onegw/internal/owner"
)

// TestUpdateCheckForcesAFreshProbe serves the cached-status lie on the first
// GET and the corrected status after a POST (the gateway's own immediate
// check), then asserts the command probed rather than trusting the cache.
func TestUpdateCheckForcesAFreshProbe(t *testing.T) {
	var gets, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if pw := r.Header.Get("X-Admin-Password"); pw != "test-pw" {
			t.Errorf("admin password header = %q, want the configured one", pw)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			posts.Add(1)
			_, _ = io.WriteString(w, `{"current":"v0.39.0","latest":"v0.40.0","outdated":true}`)
		default:
			if gets.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"current":"v0.39.0","latest":"v0.39.0","outdated":false}`)
				return
			}
			_, _ = io.WriteString(w, `{"current":"v0.39.0","latest":"v0.40.0","outdated":true}`)
		}
	}))
	defer srv.Close()

	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	cfg := &config.Config{}
	cfg.Server.AdminPassword = "test-pw"
	inf := owner.Info{PID: 4242, Listen: net.JoinHostPort(host, port)}

	if code := updateViaAdmin(context.Background(), cfg, inf, true, false, true); code != 0 {
		t.Fatalf("updateViaAdmin --check exit = %d, want 0", code)
	}
	if posts.Load() == 0 {
		t.Fatal("--check answered from the cached probe; it must force a fresh one")
	}
	if gets.Load() < 2 {
		t.Fatalf("status read %d times, want the cached one and the fresh one", gets.Load())
	}
}

// TestUpdateProbeFailureKeepsWorkingAgainstOldGateways: a gateway without the
// update endpoint 404s both calls. The forced probe must not turn that into a
// hard error — the empty status is what routes the command to its staging
// fallback, exactly as before this change.
func TestUpdateProbeFailureKeepsOldGatewayPath(t *testing.T) {
	var gets, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		} else {
			gets.Add(1)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	cfg := &config.Config{}
	cfg.Server.AdminPassword = "test-pw"
	inf := owner.Info{PID: 4242, Listen: net.JoinHostPort(host, port)}

	// --check on such a gateway prints the (empty) status and exits 0 rather
	// than failing on the missing probe endpoint.
	if code := updateViaAdmin(context.Background(), cfg, inf, true, false, true); code != 0 {
		t.Fatalf("updateViaAdmin --check exit = %d on a 404 gateway, want 0", code)
	}
	if gets.Load() < 1 {
		t.Error("status was never read")
	}
}
