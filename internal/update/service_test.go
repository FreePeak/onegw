package update

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWakeLogsFailedCheck pins the durable record of a failed periodic
// check: Status.LastError alone is NOT durable — the next successful wake
// erases it — so the failure must also reach the process log with the
// provider's reason (e.g. "HTTP 500" or an x509 TLS error) while it is
// still current. Regression for the vanishing evidence: an x509
// unknown-authority failure on the 24h tick was invisible because the
// next successful tick cleared LastError before anyone looked.
func TestWakeLogsFailedCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ONEGW_RELEASE_API", srv.URL)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	s := NewService(func() Settings { return Settings{Interval: 86400, Repo: "r"} })
	s.wake()

	if st := s.Snapshot(); st.LastError == "" {
		t.Fatal("failed check must be recorded in Status.LastError")
	}
	if out := buf.String(); !strings.Contains(out, "onegw update: periodic check failed") || !strings.Contains(out, "HTTP 500") {
		t.Fatalf("failed periodic check must be logged with the upstream reason, got: %q", out)
	}
}
