package server

// Tests for the dashboard update endpoints (#61): admin gate, GET status
// shape, POST check vs apply, and the container refusal carrying host-side
// guidance. The apply path itself (download → swap → handoff) is covered in
// internal/update/apply_test.go with a full fixture; here we only pin the
// dashboard-facing contract.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/update"
)

// newUpdSrv boots a server with the update service wired (as main does) and
// the background checker disabled (interval 0 → wake is a no-op), so tests
// observe only the state they themselves drive.
func newUpdSrv(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv, h := newAdminSrv(t, "\n[update]\ncheck_interval = \"0\"\n")
	svc := update.NewService(func() update.Settings { return update.Settings{} })
	svc.Start()
	t.Cleanup(svc.Stop)
	srv.SetUpdater(svc)
	return srv, h
}

func TestUpdateStatusEndpoint(t *testing.T) {
	_, h := newUpdSrv(t)

	// Gated: no cookie, no header → 401.
	if w := do(t, h, httptest.NewRequest(http.MethodGet, "/admin/api/v1/update", nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("ungated GET: %d", w.Code)
	}
	// Cookie session (the dashboard's path) is enough.
	c, code := loginForm(t, h, "admin")
	if code != http.StatusSeeOther {
		t.Fatalf("login: %d", code)
	}
	w := do(t, h, cookieReq(http.MethodGet, "/admin/api/v1/update", c))
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	var st update.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Pid == 0 || st.Current == "" {
		t.Fatalf("status missing pid/current: %+v", st)
	}
}
func TestUpdatePOSTCheckRunsCheck(t *testing.T) {
	_, h := newUpdSrv(t)
	// Send POST explicitly (adminReq builds GETs).
	r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/update", strings.NewReader(""))
	r.Header.Set("X-Admin-Password", "admin")
	w := do(t, h, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST check: %d %s", w.Code, w.Body.String())
	}
	var st update.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The fixture repo has no releases: the check must record an error,
	// not pretend everything is fine.
	if st.LastError == "" || st.LastCheck == nil {
		t.Fatalf("expected failed-check status, got %+v", st)
	}
	// last_check must be a real timestamp (RFC3339 JSON), not epoch zero.
	if st.LastCheck.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("stale last_check: %v", st.LastCheck)
	}
}

func TestUpdateApplyInContainerRefusedWithGuidance(t *testing.T) {
	t.Setenv("ONEGW_IN_CONTAINER", "1")
	_, h := newUpdSrv(t)
	r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/update", strings.NewReader(`{"apply":true}`))
	r.Header.Set("X-Admin-Password", "admin")
	w := do(t, h, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("container apply: want 409, got %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Error    string `json:"error"`
		Guidance string `json:"guidance"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Guidance, "docker pull") || !strings.Contains(body.Guidance, update.Image) {
		t.Fatalf("guidance missing host-side pull/recreate commands: %q", body.Guidance)
	}
	if !strings.Contains(body.Error, "container") {
		t.Fatalf("error field wrong: %q", body.Error)
	}
}

func TestUpdateEndpointsWithoutServiceRefuse(t *testing.T) {
	// A server with NO updater wired (no main wiring) must answer 409,
	// never panic.
	_, h := newAdminSrv(t, "")
	r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/update", strings.NewReader(`{"apply":true}`))
	r.Header.Set("X-Admin-Password", "admin")
	if w := do(t, h, r); w.Code != http.StatusConflict {
		t.Fatalf("apply without updater: want 409, got %d", w.Code)
	}
	if w := do(t, h, adminReq(t, "/admin/api/v1/update")); w.Code != http.StatusConflict {
		t.Fatalf("status without updater: want 409, got %d", w.Code)
	}
}

func TestSettingsPageShowsVersionCard(t *testing.T) {
	srv, h := newUpdSrv(t)
	_ = srv
	w := do(t, h, adminReq(t, "/admin/ui/settings"))
	if w.Code != http.StatusOK {
		t.Fatalf("settings page: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Version", "Update now", "Check now", "/admin/api/v1/update"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	// The card's static render shows the running version stamp.
	if !strings.Contains(body, "upd-current") {
		t.Error("settings page missing current-version element")
	}
}
