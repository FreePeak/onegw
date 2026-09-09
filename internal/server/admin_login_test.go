package server

// Login lockout tests (#goal: block 1 day after 5 failed password
// attempts). Tested at the same seam as the old dashboard_test.go: form
// posts against POST /admin/login.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

const loginTestToml = `
listen = "127.0.0.1:0"

[server]
admin_password = "admin"
data_dir = "memory"

[auth]
keys = ["sk-test-gw"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "http://127.0.0.1:1"
api_key = "sk-test-upstream-secret"
models = ["m1"]
`

// postLogin posts the password to /admin/login like the login form does.
func postLogin(t *testing.T, h http.Handler, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(t, h, r)
}

func sessionCookieOf(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestFiveFailedLoginsBlockForOneDay(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, loginTestToml)

	for i := 0; i < 5; i++ {
		w := postLogin(t, h, "wrong")
		if w.Code != http.StatusOK {
			t.Fatalf("failed attempt %d: status %d", i+1, w.Code)
		}
	}

	// The correct password no longer logs in: 5 failures block the source
	// IP for one day.
	w := postLogin(t, h, "admin")
	if c := sessionCookieOf(w); c != nil {
		t.Fatalf("locked-out login issued a session cookie")
	}
	ra, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || ra < 86000 || ra > 86401 {
		t.Fatalf("Retry-After = %q (err %v), want ~one day (86400s)", w.Header().Get("Retry-After"), err)
	}
}

func TestFourFailedLoginsDoNotBlock(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, loginTestToml)

	for i := 0; i < 4; i++ {
		postLogin(t, h, "wrong")
	}

	// The block only triggers on the fifth failure: the fifth attempt with
	// the correct password still logs in.
	w := postLogin(t, h, "admin")
	if w.Code != http.StatusSeeOther || sessionCookieOf(w) == nil {
		t.Fatalf("correct login after 4 failures: status %d, want redirect + session cookie", w.Code)
	}
}
