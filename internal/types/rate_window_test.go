package types

import (
	"testing"
	"time"
)

// The window parser feeds account benches and client Retry-After values:
// it must read new-api's "Maximum 8 requests within 1 minutes" family
// (live 2026-09-09 tokenrouter) and stay silent on messages that name no
// window — a wrong positive benches accounts for a window they never had.
func TestRateWindow(t *testing.T) {
	cases := []struct {
		msg  string
		want time.Duration
	}{
		{"You have reached the request limit[z-ai/glm-5.3-free]: Maximum 8 requests within 1 minutes. (request id: 20260909155655977745357PIFzKuhT)", time.Minute},
		{"Maximum 8 requests within 1 minute.", time.Minute},
		{"Rate limited: Maximum 30 requests within 30 seconds", 30 * time.Second},
		{"Maximum 5 requests within 2 hours", 2 * time.Hour},
		{"The request rate exceeds the current model Concurrency limit 1200.", 0},
		{"BackendAdmissionRejected: Engine cold-request admission rejected", 0},
		{"rate limit exceeded, key sk-x", 0},
		{"", 0},
	}
	for _, tc := range cases {
		e := &APIError{Status: 429, Message: tc.msg}
		if got := e.RateWindow(); got != tc.want {
			t.Errorf("RateWindow(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	// nil receiver: classifier methods must not panic on absent errors.
	var e *APIError
	if got := e.RateWindow(); got != 0 {
		t.Errorf("nil RateWindow = %v, want 0", got)
	}
}
