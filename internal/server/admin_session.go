package server

// Admin session auth (#45): the header-only X-Admin-Password gate cannot
// ride an EventSource connection, so the dashboard logs in once and gets a
// short-TTL HttpOnly SameSite=Strict cookie. Header callers (smoke.sh,
// scripts) keep working unchanged; empty admin_password stays "open".
//
// Sessions are in-memory and bounded (64): a gateway restart logs everyone
// out, and changing admin_password invalidates every session instantly
// (each token records the sha256 of the password it was issued for).

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"onegw/internal/server/dashboard"
)

const (
	sessionCookie   = "onegw_admin"
	sessionTTL      = 12 * time.Hour
	maxSessions     = 64
	maxLoginFails   = 5
	loginBlockAfter = 24 * time.Hour
)

type adminSession struct {
	pwHash [sha256.Size]byte
	exp    time.Time
}

type adminSessions struct {
	mu sync.Mutex
	m  map[string]adminSession // token hex → session
}

func newAdminSessions() *adminSessions {
	return &adminSessions{m: map[string]adminSession{}}
}

// loginGuard blocks brute-force attempts per remote IP: after
// maxLoginFails failures the IP waits loginBlockAfter. Bounded map
// (256 entries).
type loginGuard struct {
	mu sync.Mutex
	m  map[string]*loginFails
}

type loginFails struct {
	n       int
	blocked time.Time
}

func newLoginGuard() *loginGuard {
	return &loginGuard{m: map[string]*loginFails{}}
}

// issue creates a session token for pw. Callers gate on a passed password
// check; issue itself only mints.
func (a *adminSessions) issue(pw string) string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failure is catastrophic but never silently weak:
		// fall back to a time-derived token (still secrets-gated).
		h := sha256.Sum256(append([]byte(pw), time.Now().String()...))
		copy(raw[:], h[:])
	}
	tok := hex.EncodeToString(raw[:])
	s := adminSession{pwHash: sha256.Sum256([]byte(pw)), exp: time.Now().Add(sessionTTL)}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Bounded: sweep expired, then evict soonest-expiring when still full.
	if len(a.m) >= maxSessions {
		a.sweepLocked()
	}
	if len(a.m) >= maxSessions {
		var oldest string
		var oldestT time.Time
		for k, v := range a.m {
			if oldest == "" || v.exp.Before(oldestT) {
				oldest, oldestT = k, v.exp
			}
		}
		delete(a.m, oldest)
	}
	a.m[tok] = s
	return tok
}

func (a *adminSessions) sweepLocked() {
	now := time.Now()
	for k, v := range a.m {
		if now.After(v.exp) {
			delete(a.m, k)
		}
	}
}

func (a *adminSessions) drop(tok string) {
	a.mu.Lock()
	delete(a.m, tok)
	a.mu.Unlock()
}

// valid reports whether tok is a live session issued for pw.
func (a *adminSessions) valid(tok string, pw string) bool {
	a.mu.Lock()
	s, ok := a.m[tok]
	a.mu.Unlock()
	if !ok || time.Now().After(s.exp) {
		return false
	}
	want := sha256.Sum256([]byte(pw))
	return subtle.ConstantTimeCompare(s.pwHash[:], want[:]) == 1
}

func (a *adminSessions) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.m)
}

func (g *loginGuard) blocked(ip string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.m[ip]
	if f == nil {
		return false, 0
	}
	if d := time.Until(f.blocked); d > 0 {
		return true, d
	}
	return false, 0
}

func (g *loginGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.m) >= 256 {
		cut := time.Now().Add(-loginBlockAfter)
		for k, v := range g.m {
			if v.blocked.Before(cut) {
				delete(g.m, k)
			}
		}
		for k := range g.m { // still full: drop one arbitrary; bounded is what matters
			if len(g.m) < 256 {
				break
			}
			delete(g.m, k)
		}
	}
	f := g.m[ip]
	if f == nil {
		f = &loginFails{}
		g.m[ip] = f
	}
	f.n++
	if f.n >= maxLoginFails {
		f.blocked = time.Now().Add(loginBlockAfter)
		f.n = 0
	}
}

func (g *loginGuard) pass(ip string) {
	g.mu.Lock()
	delete(g.m, ip)
	g.mu.Unlock()
}

// handleAdminLogin answers POST /admin/login (form field `password`, or a
// JSON body {"password": ...}). Success sets the session cookie and
// redirects back to /admin; failures re-render the login page.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	pw := s.cur().cfg.Server.AdminPassword
	if pw == "" {
		// Nothing to authenticate; the console is open.
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	ip := remoteIP(r)
	if blocked, d := s.logins.blocked(ip); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		s.renderLogin(w, "too many failed attempts — retry in "+d.Truncate(time.Second).String())
		return
	}
	var given string
	if isJSON(r) && r.Body != nil {
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err == nil {
			given = body.Password
		}
	} else if err := r.ParseForm(); err == nil {
		given = r.PostFormValue("password")
	}
	if subtle.ConstantTimeCompare([]byte(given), []byte(pw)) != 1 {
		s.logins.fail(ip)
		s.renderLogin(w, "wrong password")
		return
	}
	s.logins.pass(ip)
	tok := s.sessions.issue(pw)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// renderLogin writes the login page (200 — it IS the page a browser
// should see; API clients get 401 from the data endpoints themselves).
func (s *Server) renderLogin(w http.ResponseWriter, errMsg string) {
	out, err := dashboard.Render("login", dashboard.Shell{V: dashboard.LoginView{Error: errMsg}})
	if err != nil {
		http.Error(w, "render error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}

func isJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return len(ct) >= 16 && ct[:16] == "application/json"
}

func remoteIP(r *http.Request) string {
	// Admin binds loopback by default; RemoteAddr is the identity. Proxy
	// headers are deliberately ignored (spoofable — would defeat the guard).
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
