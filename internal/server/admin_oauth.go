package server

// Dashboard OAuth sign-in endpoints — the UI twin of the onegw-oauth CLI for
// [[oauth.accounts]] entries. A Grok / SuperGrok subscription account has no
// API key to paste: its credential is an OAuth login, so the dashboard has to
// start one and follow it. Two dialects, picked by the service profile:
//
//   - browser (authorization-code + PKCE) — the default wherever the profile
//     publishes an authorize endpoint (xAI). The operator opens the link, and
//     the vendor redirects back to a loopback address this process owns; see
//     admin_oauth_browser.go. No code to type.
//   - device (RFC 8628) — the fallback for every profile, and the only option
//     when the browser cannot reach this host (`?flow=device`).
//
//	GET    /admin/config/oauth/accounts          — sign-in state of every entry
//	POST   /admin/config/oauth/login?key=K       — start (or re-offer) K's login
//	POST   /admin/config/oauth/exchange?key=K    — finish a browser login from a
//	                                               pasted code/callback URL
//	POST   /admin/config/oauth/logout?key=K      — drop K's stored token
//
// K is the token-store key "provider/account" of a configured entry; it goes
// in a query parameter because the account part is often an email address
// (xAI names sessions by login), which would need percent-encoding in a path
// segment. Only entries with their own login are addressable: a borrower
// (`owner` set) resolves another entry's session, so it is managed through
// that owner. Secrets never appear in responses — expiry, state flags and
// the public verification prompt only.
//
// The flow runs in a background goroutine per account and the response
// returns the prompt as soon as the authorization server issues it; the
// dashboard polls `accounts` until the state flips to signed-in. Starting
// while a login is already pending returns that same prompt instead of a
// second flow, because xAI rotates sessions — two live logins for one
// account knock each other out (README § Grok subscriptions).
//
// Pending prompts are process-local: a restart or a reload drops them (finish
// with `onegw-oauth login`), never a stored token — those live in the data
// dir and outlive every reload.

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"onegw/internal/config"
	"onegw/internal/oauth"
)

// oauthPrompt is the operator-facing half of a login: what to open, and for
// the device dialect what to type. The device code and the PKCE verifier stay
// server-side — they are the polling/exchange credentials, not something to
// display.
type oauthPrompt struct {
	Mode                    string `json:"mode"` // "browser" | "device"
	VerificationURL         string `json:"verification_uri"`
	VerificationURLComplete string `json:"verification_uri_complete,omitempty"`
	UserCode                string `json:"user_code,omitempty"`  // device only
	ExpiresAt               string `json:"expires_at,omitempty"` // RFC 3339; "" if the server said nothing
	Interval                int    `json:"interval,omitempty"`   // poll cadence, seconds; device only
}

// oauthLogin is one login attempt. `finished` distinguishes a live
// flow (whose prompt must be re-offered to a second caller) from a terminal
// one whose error is still worth showing until the next attempt. `pkce` is
// set for a browser flow: the pending verifier, keyed by state.
type oauthLogin struct {
	cancel   context.CancelFunc
	done     chan struct{}
	prompt   oauthPrompt
	finished bool
	err      string
	pkce     *oauth.PKCESession
}

// oauthAdmin is the server's login registry. It lives on the Server
// (not the reloadable state) so a SIGHUP mid-login cannot orphan the
// goroutine that holds the polling credential, nor the loopback listener
// awaiting a browser callback.
type oauthAdmin struct {
	mu     sync.Mutex
	active map[string]*oauthLogin // by store key

	// Browser-flow listener: one loopback server shared by every pending
	// login, opened on demand and closed when the last one settles.
	states       map[string]string // OAuth state -> store key of the login awaiting it
	callbackBase string            // "http://127.0.0.1:PORT" of the live listener; "" when closed
	boundPort    int               // port the live listener actually bound (0 when closed)
	ln           net.Listener
	srv          *http.Server
	idle         *time.Timer // closes the listener when nothing is pending
}

func newOauthAdmin() *oauthAdmin {
	return &oauthAdmin{active: map[string]*oauthLogin{}, states: map[string]string{}}
}

// oauthState is one [[oauth.accounts]] entry as the dashboard sees it.
type oauthState struct {
	Key         string       `json:"key"`
	Provider    string       `json:"provider"`
	Account     string       `json:"account"`
	Service     string       `json:"service"`
	Owner       string       `json:"owner,omitempty"`       // borrower: session comes from this key
	State       string       `json:"state"`                 // signed-in | expired | pending | signed-out | failed
	ExpiresAt   string       `json:"expires_at,omitempty"`  // stored token expiry (RFC 3339)
	Cooling     bool         `json:"cooling,omitempty"`     // upstream parked this account
	Invalidated bool         `json:"invalidated,omitempty"` // terminal billing verdict (#80)
	Prompt      *oauthPrompt `json:"prompt,omitempty"`      // set while a flow awaits the operator
	Error       string       `json:"error,omitempty"`       // last attempt's failure
}

// OAuth states, in `oauthState.State`.
const (
	oauthSignedIn  = "signed-in"
	oauthExpired   = "expired"
	oauthPending   = "pending"
	oauthSignedOut = "signed-out"
	oauthFailed    = "failed"
)

// OAuth dialects, in `oauthPrompt.Mode`. Browser is the default wherever the
// service profile publishes an authorize endpoint; device is the fallback.
const (
	oauthModeBrowser = "browser"
	oauthModeDevice  = "device"
)

// oauthKeyParam reads and validates the store key of an oauth request.
func oauthKeyParam(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

// spec resolves the configured entry behind key: its service profile with
// the entry's endpoint overrides applied (the same resolution the CLI does),
// so a self-hosted IdP or a test stub works from the UI unchanged. Borrowers
// and unknown services are refused.
func (s *Server) spec(key string) (oauth.AccountSpec, config.OAuthAccount, bool) {
	st := s.cur()
	if st == nil || key == "" {
		return oauth.AccountSpec{}, config.OAuthAccount{}, false
	}
	for _, a := range st.cfg.OAuthAccounts() {
		if a.Owner != "" || a.StoreKey() != key {
			continue
		}
		p, ok := oauth.Lookup(a.Service)
		if !ok {
			return oauth.AccountSpec{}, config.OAuthAccount{}, false
		}
		if a.DeviceURL != "" {
			p.DeviceCodeURL = a.DeviceURL
		}
		if a.TokenURL != "" {
			p.TokenURL = a.TokenURL
		}
		if a.ClientID != "" {
			p.ClientID = a.ClientID
		}
		if a.Scope != "" {
			p.Scope = a.Scope
		}
		if port := st.cfg.OAuth.CallbackPort; port > 0 {
			p.CallbackPort = port
		}
		return oauth.AccountSpec{Key: key, Provider: p}, a, true
	}
	return oauth.AccountSpec{}, config.OAuthAccount{}, false
}

// handleAdminOAuthAccounts lists every configured entry with its sign-in
// state. The dashboard renders the badges from it and polls it while a device
// flow is open, so it must be cheap: one store read + one pool snapshot per
// provider, no network.
func (s *Server) handleAdminOAuthAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	writeJSON(w, map[string]any{"accounts": s.oauthStates()})
}

func (s *Server) oauthStates() []oauthState {
	st := s.cur()
	if st == nil {
		return nil
	}
	out := make([]oauthState, 0, len(st.cfg.OAuth.Accounts))
	for _, a := range st.cfg.OAuthAccounts() {
		v := oauthState{Key: a.StoreKey(), Provider: a.Provider, Account: a.Account, Service: a.Service, Owner: a.Owner}
		if a.Owner != "" {
			// Borrower: no login of its own; its state is its owner's token.
			v.State = oauthSignedOut
			if tok, ok := s.tokenOf(v.Key); ok {
				v.State = oauthSignedIn
				v.ExpiresAt = tok
			}
			out = append(out, v)
			continue
		}
		if tok, ok := s.tokenOf(v.Key); ok {
			v.ExpiresAt = tok
			v.State = oauthSignedIn
			if tok != "" && expired(tok) {
				v.State = oauthExpired
			}
		} else {
			v.State = oauthSignedOut
		}
		if def, ok := st.pool.Get(a.Provider); ok {
			for _, ps := range def.PoolStates() {
				if ps.Name != a.Account {
					continue
				}
				v.Cooling, v.Invalidated = ps.Cooling, ps.Invalidated
			}
		}
		s.oa.mu.Lock()
		if lg := s.oa.active[v.Key]; lg != nil {
			if !lg.finished {
				v.State = oauthPending
				p := lg.prompt
				v.Prompt = &p
			} else if lg.err != "" && (v.State == oauthSignedOut || v.State == oauthExpired) {
				v.State = oauthFailed
			}
			v.Error = lg.err
		}
		s.oa.mu.Unlock()
		out = append(out, v)
	}
	return out
}

// tokenOf returns the stored token's expiry ("" when it never expires) and
// whether a token exists at all.
func (s *Server) tokenOf(key string) (string, bool) {
	if s.oauth == nil {
		return "", false
	}
	tok, ok := s.oauth.Store().Get(key)
	if !ok {
		return "", false
	}
	if tok.ExpiresAt.IsZero() {
		return "", true
	}
	return tok.ExpiresAt.Format(time.RFC3339), true
}

func expired(rfc3339 string) bool {
	t, err := time.Parse(time.RFC3339, rfc3339)
	return err == nil && !time.Now().Before(t)
}

// handleAdminOAuthLogin starts (or re-offers) one account's sign-in. The
// service profile decides the dialect: browser (authorize URL + loopback
// callback) where the vendor publishes one, device flow otherwise — and
// `?flow=device` forces the device dialect on a browser-capable profile, for
// an operator whose browser cannot reach this host's callback port.
func (s *Server) handleAdminOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	if s.oauth == nil {
		adminError(w, http.StatusServiceUnavailable, "oauth unavailable (no data dir)")
		return
	}
	key := oauthKeyParam(r)
	spec, _, ok := s.spec(key)
	if !ok {
		adminError(w, http.StatusNotFound, "no OAuth account "+key+" — tick the subscription box on that account in the provider editor and save first")
		return
	}
	browser := spec.Provider.BrowserFlow() && r.URL.Query().Get("flow") != "device"

	s.oa.mu.Lock()
	if cur := s.oa.active[key]; cur != nil && !cur.finished {
		p := cur.prompt
		s.oa.mu.Unlock()
		writeJSON(w, map[string]any{"key": key, "state": oauthPending, "prompt": p})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	lg := &oauthLogin{cancel: cancel, done: make(chan struct{})}
	s.oa.active[key] = lg
	s.oa.mu.Unlock()

	if browser {
		// xAI's own login: the operator opens a link and the vendor's redirect
		// comes back to a loopback port we own — nothing to read out or type.
		// The authorize URL is built locally, so the response carries it
		// immediately; a failure to bind the callback port is reported as-is,
		// and `?flow=device` is the way around it.
		if err := s.startBrowserLogin(key, spec, lg); err != nil {
			s.oa.mu.Lock()
			delete(s.oa.active, key)
			s.oa.mu.Unlock()
			cancel()
			adminError(w, http.StatusServiceUnavailable, "browser sign-in unavailable: "+err.Error())
			return
		}
		s.oa.mu.Lock()
		p := lg.prompt
		s.oa.mu.Unlock()
		writeJSON(w, map[string]any{"key": key, "state": oauthPending, "prompt": p})
		return
	}

	go s.runDeviceFlow(ctx, lg, spec)

	// The device start is one round-trip; waiting briefly means the normal
	// response already carries the code the operator needs.
	select {
	case <-lg.done:
	case <-time.After(2 * time.Second):
	}
	s.oa.mu.Lock()
	p, state, errText := lg.prompt, oauthPending, lg.err
	if lg.finished {
		if lg.err != "" {
			state = oauthFailed
		} else {
			state = oauthSignedIn
		}
	}
	s.oa.mu.Unlock()
	writeJSON(w, map[string]any{"key": key, "state": state, "prompt": p, "error": errText})
}

// finishOAuthLogin is what a successful sign-in of either dialect does once
// the token is in the store: clear any terminal billing verdict that account
// earned with the OLD credential (otherwise it stays out of rotation until an
// operator notices the reset button) and tell every dashboard tab.
func (s *Server) finishOAuthLogin(key string) {
	if st := s.cur(); st != nil && st.pool != nil {
		if def, ok := st.pool.Get(providerOf(key)); ok {
			def.RevalidateByName(accountOf(key))
		}
	}
	s.events.publish("config", `{"oauth":`+jsonString(key)+`,"state":"signed-in"}`)
}

// runDeviceFlow drives one login to completion: the manager polls the
// provider, stores the token on success, and the prompt callback publishes
// what the operator must enter. Called in its own goroutine.
func (s *Server) runDeviceFlow(ctx context.Context, lg *oauthLogin, spec oauth.AccountSpec) {
	defer close(lg.done)
	tok, err := s.oauth.Login(ctx, spec, func(ds oauth.DeviceStart) {
		p := oauthPrompt{
			Mode:                    oauthModeDevice,
			VerificationURL:         ds.VerificationURL,
			VerificationURLComplete: ds.VerificationURLComplete,
			UserCode:                ds.UserCode,
			Interval:                int(ds.Interval.Seconds()),
		}
		if ds.ExpiresIn > 0 {
			p.ExpiresAt = time.Now().Add(ds.ExpiresIn).Format(time.RFC3339)
		}
		s.oa.mu.Lock()
		lg.prompt = p
		s.oa.mu.Unlock()
	})
	s.oa.mu.Lock()
	lg.finished = true
	if err != nil {
		lg.err = err.Error()
	}
	s.oa.mu.Unlock()
	if err != nil {
		log.Printf("admin: oauth login %s failed: %v", spec.Key, err)
		s.events.publish("config", `{"oauth":`+jsonString(spec.Key)+`,"state":"failed"}`)
		return
	}
	expiry := "no expiry known"
	if !tok.ExpiresAt.IsZero() {
		expiry = tok.ExpiresAt.Format(time.RFC3339)
	}
	log.Printf("admin: oauth login %s signed in (token stored, %s)", spec.Key, expiry)
	s.finishOAuthLogin(spec.Key)
}

// providerOf / accountOf split a "provider/account" store key. Provider names
// never contain "/", and account names may (rare; e.g. an email localpart
// never does), so the FIRST separator wins.
func providerOf(key string) string {
	if i := strings.Index(key, "/"); i > 0 {
		return key[:i]
	}
	return key
}

func accountOf(key string) string {
	if i := strings.Index(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return ""
}

// handleAdminOAuthLogout deletes one account's stored token. The config entry
// stays, so the account keeps its subscription box; the provider falls back
// to its static api_key (if any) until the next sign-in.
func (s *Server) handleAdminOAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	if s.oauth == nil {
		adminError(w, http.StatusServiceUnavailable, "oauth unavailable (no data dir)")
		return
	}
	key := oauthKeyParam(r)
	if _, _, ok := s.spec(key); !ok {
		adminError(w, http.StatusNotFound, "no OAuth account "+key)
		return
	}
	s.oa.mu.Lock()
	lg := s.oa.active[key]
	s.oa.mu.Unlock()
	if lg != nil && !lg.finished {
		lg.cancel()
		<-lg.done
	}
	if err := s.oauth.Store().Delete(key); err != nil {
		adminError(w, http.StatusInternalServerError, "delete token: "+err.Error())
		return
	}
	log.Printf("admin: oauth token %s deleted from the dashboard (re-login to restore)", key)
	s.events.publish("config", `{"oauth":`+jsonString(key)+`,"state":"signed-out"}`)
	writeJSON(w, map[string]any{"key": key, "deleted": true})
}
