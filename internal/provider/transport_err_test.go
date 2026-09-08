package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// fakeTimeoutErr satisfies net.Error with Timeout() set, the shape Go's
// http.Client returns for ResponseHeaderTimeout/dial timeouts (wrapped in
// *url.Error on the real path).
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "net/http: timeout awaiting response headers" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return false }

var _ net.Error = fakeTimeoutErr{}

// timeoutDeadlineErr models the real header-timeout error shape (probe,
// Go 1.25): a net.Error with Timeout()=true whose chain also satisfies
// errors.Is(..., context.DeadlineExceeded).
type timeoutDeadlineErr struct{ fakeTimeoutErr }

func (timeoutDeadlineErr) Unwrap() error { return context.DeadlineExceeded }

// The 2026-09-08 502 storm was undiagnosable from the dashboard because a
// gateway-side pre-first-byte timeout, a dead connection, and a real
// upstream HTTP 5xx all logged as the same "502 upstream_error". Transport
// failures must carry their own type and status so the request log tells
// them apart.
func TestTransportErrClassification(t *testing.T) {
	cases := []struct {
		name       string
		ctxErr     error // non-nil = the request context is done (client hangup)
		err        error
		wantStatus int
		wantType   string
	}{
		{
			name:       "response-header timeout",
			err:        &url.Error{Op: "Post", URL: "https://x/y", Err: fakeTimeoutErr{}},
			wantStatus: 504, wantType: "upstream_timeout",
		},
		{
			name:       "client hangup",
			ctxErr:     context.Canceled,
			err:        &url.Error{Op: "Post", URL: "https://x/y", Err: context.Canceled},
			wantStatus: 499, wantType: "client_closed",
		},
		{
			name:       "client deadline",
			ctxErr:     context.DeadlineExceeded,
			err:        fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
			wantStatus: 499, wantType: "client_closed",
		},
		{
			name:       "connection refused",
			err:        &url.Error{Op: "Post", URL: "https://x/y", Err: errors.New("connection refused")},
			wantStatus: 502, wantType: "upstream_unreachable",
		},
		{
			// Regression (2026-09-08, verified by probe on h1 and h2, Go
			// 1.25): the real ResponseHeaderTimeout error BOTH implements
			// net.Error with Timeout()=true AND satisfies
			// errors.Is(err, context.DeadlineExceeded). Chain-matching the
			// deadline alias first mislabeled real upstream timeouts as
			// 499 client_closed — and 499 is not Retryable(), so combos
			// stopped falling through. The net.Error/Timeout classification
			// must win over the deadline alias.
			name:       "header timeout aliasing DeadlineExceeded",
			err:        &url.Error{Op: "Post", URL: "https://x/y", Err: timeoutDeadlineErr{}},
			wantStatus: 504, wantType: "upstream_timeout",
		},
		{
			// (*url.Error).Timeout() only type-asserts its direct child,
			// so a timeout buried under an extra wrap layer must still be
			// found by walking the chain (errors.As stops at url.Error).
			name:       "header timeout buried under wrap layer",
			err:        &url.Error{Op: "Post", URL: "https://x/y", Err: fmt.Errorf("wrapped: %w", fakeTimeoutErr{})},
			wantStatus: 504, wantType: "upstream_timeout",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if c.ctxErr != nil {
				cancel()
			} else {
				defer cancel()
			}
			got := transportErr(ctx, c.err)
			if got.Status != c.wantStatus || got.Type != c.wantType {
				t.Fatalf("got %d/%s, want %d/%s", got.Status, got.Type, c.wantStatus, c.wantType)
			}
			if got.Message == "" {
				t.Fatal("classifier must keep the underlying error text")
			}
		})
	}
}

// A stalled upstream must be aborted by the pre-first-byte budget, and the
// resulting error must be typed upstream_timeout end-to-end through Do.
func TestDoTimeoutSurfacesUpstreamTimeout(t *testing.T) {
	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stalled // never answer headers while the test waits
	}))
	// Defer order matters (LIFO): close(stalled) is registered last so it
	// runs FIRST, unblocking the handler before Close waits it out.
	defer srv.Close()
	defer close(stalled)

	def := &Def{
		Name: "slow", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts:      []Account{{Name: "a", APIKey: "k"}},
		HeaderTimeout: 80 * time.Millisecond,
	}
	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil {
		t.Fatal("stalled upstream must error out via the header-timeout budget")
	}
	if apiErr.Type != "upstream_timeout" || apiErr.Status != 504 {
		t.Fatalf("got %d/%s, want 504/upstream_timeout", apiErr.Status, apiErr.Type)
	}
	if !apiErr.Retryable() {
		t.Fatal("upstream_timeout must stay retryable so combos fall through")
	}
}

// The per-Def client is memoized (pooling survives reload churn only within
// one Def) and distinct from the package default.
func TestHTTPClientMemoizedPerDef(t *testing.T) {
	def := &Def{Name: "p", Kind: KindOpenAI, HeaderTimeout: 90 * time.Second}
	c1 := def.httpClient()
	c2 := def.httpClient()
	if c1 != c2 {
		t.Fatal("httpClient must return one client per Def (connection pooling)")
	}
	if c1 == client {
		t.Fatal("tuned HeaderTimeout must not silently fall back to the default client")
	}
	var tr *http.Transport
	if hct, ok := c1.Transport.(*http.Transport); ok {
		tr = hct
	} else {
		t.Fatalf("unexpected transport type %T", c1.Transport)
	}
	if tr.ResponseHeaderTimeout != 90*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 90s", tr.ResponseHeaderTimeout)
	}
	// Untuned Def shares the package default (zero-config behavior).
	plain := &Def{Name: "q", Kind: KindOpenAI}
	if plain.httpClient() != client {
		t.Fatal("untuned Def must use the default client")
	}
}
