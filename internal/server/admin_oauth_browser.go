package server

// Browser (authorization-code + PKCE) sign-in — the flow where the operator
// opens a link, logs in at the vendor, and the vendor's redirect lands on a
// loopback address this process owns. This is how the official Grok CLI
// authenticates and what 9router does for xAI
// (src/lib/oauth/providers/xai.js:27 + src/lib/oauth/utils/server.js:338-435):
// same public client, S256 challenge, fixed 127.0.0.1:56121/callback listener,
// server-side code exchange, and the same "paste the code back" exit for a
// browser that cannot reach us.
//
// One listener serves every pending browser login: it opens with the first one
// and closes browserCallbackIdle after the last settles, so a gateway that
// never signs in this way binds no extra port. The address comes from the
// session that needed it (so oauth.callback_port and the profile's registered
// port both flow through), and any path is accepted because the single-use
// random `state` — not the URL — is what authorizes a callback.
//
// Secrets: the code verifier never leaves this process, the code itself is
// spent once, and the resulting token goes straight to the data-dir token
// store. Nothing sensitive is echoed to the browser beyond what the operator
// already has.

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"onegw/internal/oauth"
)

const (
	// browserCallbackLife caps one pending browser login. 9router allows 5
	// minutes for the same wait; a little more costs nothing and covers the
	// "find the SuperGrok password" time it usually takes.
	browserCallbackLife = 10 * time.Minute
)

// browserCallbackIdle is how long the loopback listener stays open after the
// last login settles, so an immediate retry need not rebind. A var so the suite
// can prove the port is genuinely released rather than held for the process's
// whole life.
var (
	browserCallbackIdle = 60 * time.Second

	// browserCallbackPort is the loopback port to bind. A var, not a const, so
	// tests bind :0 and read their ephemeral port back out of the prompt.
	browserCallbackPort = oauth.DefaultCallbackPort
)

// startBrowserLogin prepares one browser login: bind the callback listener,
// build the PKCE challenge against its address, and register the pending
// session under its state. The caller has already installed lg in the
// registry and holds no lock.
func (s *Server) startBrowserLogin(key string, spec oauth.AccountSpec, lg *oauthLogin) error {
	base, err := s.ensureCallbackListener()
	if err != nil {
		return err
	}
	sess, err := oauth.NewPKCE(spec.Provider, base+spec.Provider.RedirectPath())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), browserCallbackLife)
	lg.pkce = &sess
	lg.cancel = cancel
	lg.prompt = oauthPrompt{
		Mode:                    oauthModeBrowser,
		VerificationURL:         sess.AuthURL,
		VerificationURLComplete: sess.AuthURL,
		ExpiresAt:               time.Now().Add(browserCallbackLife).Format(time.RFC3339),
	}
	s.oa.mu.Lock()
	s.oa.states[sess.State] = key
	s.oa.mu.Unlock()
	log.Printf("admin: oauth login %s awaiting the browser callback on %s", key, sess.RedirectURI)
	go s.expireBrowserLogin(ctx, cancel, lg)
	return nil
}

// expireBrowserLogin settles a login the operator abandoned: without it a
// closed tab would report "pending" and hold the listener open for nothing.
func (s *Server) expireBrowserLogin(ctx context.Context, cancel context.CancelFunc, lg *oauthLogin) {
	<-ctx.Done()
	cancel()
	s.oa.mu.Lock()
	if !lg.finished {
		lg.finished = true
		lg.err = "browser sign-in timed out — start it again, or paste the code"
		close(lg.done)
		if lg.pkce != nil {
			delete(s.oa.states, lg.pkce.State)
		}
	}
	empty := len(s.oa.states) == 0
	s.oa.mu.Unlock()
	if empty {
		s.closeCallback()
	}
}

// ensureCallbackListener binds the shared loopback listener, or returns the
// address of the one already running.
func (s *Server) ensureCallbackListener() (string, error) {
	s.oa.mu.Lock()
	defer s.oa.mu.Unlock()
	if s.oa.callbackBase != "" {
		return s.oa.callbackBase, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", browserCallbackPort))
	if err != nil {
		// Port taken (another login helper on this box, or an explicit
		// oauth.callback_port that collides): fall back to an ephemeral
		// loopback port and say so, since the vendor only guarantees the
		// registered redirect URI.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", fmt.Errorf("bind loopback callback: %w", err)
		}
		log.Printf("admin: oauth callback port %d busy (%v); using %s instead — the vendor may reject an unregistered redirect_uri",
			browserCallbackPort, err, ln.Addr())
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(s.handleOAuthCallback),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.oa.srv, s.oa.ln = srv, ln
	s.oa.callbackBase = "http://" + ln.Addr().String()
	if s.oa.idle != nil {
		s.oa.idle.Stop()
		s.oa.idle = nil
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("admin: oauth callback server stopped: %v", err)
		}
	}()
	log.Printf("admin: oauth callback listener on %s", s.oa.callbackBase)
	return s.oa.callbackBase, nil
}

// closeCallback shuts the loopback listener when no browser login is pending.
func (s *Server) closeCallback() {
	s.oa.mu.Lock()
	defer s.oa.mu.Unlock()
	if len(s.oa.states) > 0 || s.oa.srv == nil {
		return
	}
	srv, ln := s.oa.srv, s.oa.ln
	s.oa.srv, s.oa.ln, s.oa.callbackBase = nil, nil, ""
	go func() {
		_ = srv.Close()
		_ = ln.Close()
	}()
	log.Printf("admin: oauth callback listener closed")
}

// handleOAuthCallback is the loopback redirect target: it exchanges the code
// for a token, stores it under the pending login's account key, and answers a
// "close this tab" page. The dashboard notices through its existing accounts
// poll and the `config` SSE event, so there is no second channel.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		// Cline never echoes `state` — it redirects to the exact callback_url
		// it was given — so its per-login token is the last path segment
		// (newClineSession). Any profile that does send state still matches
		// the line above, so this fallback changes nothing for them.
		if i := strings.LastIndex(r.URL.Path, "/"); i >= 0 && i+1 < len(r.URL.Path) {
			state = r.URL.Path[i+1:]
		}
	}
	s.oa.mu.Lock()
	key, known := s.oa.states[state]
	var lg *oauthLogin
	if known {
		lg = s.oa.active[key]
		// Single use: the code is spent whether the exchange lands or not.
		delete(s.oa.states, state)
	}
	s.oa.mu.Unlock()

	if !known || lg == nil {
		oauthCallbackPage(w, http.StatusBadRequest, false,
			"This sign-in link was already used or has expired. Start it again from the onegw dashboard.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.failBrowserLogin(key, lg, strings.TrimSpace(e+" "+q.Get("error_description")))
		oauthCallbackPage(w, http.StatusOK, false, "The vendor reported: "+e+".\n\nClose this tab and retry from the dashboard.")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.failBrowserLogin(key, lg, "callback carried no authorization code")
		oauthCallbackPage(w, http.StatusBadRequest, false, "No authorization code in the callback. Close this tab and retry.")
		return
	}
	if _, err := s.exchangeBrowserCode(r.Context(), key, lg, code); err != nil {
		oauthCallbackPage(w, http.StatusOK, false, err.Error())
		return
	}
	oauthCallbackPage(w, http.StatusOK, true, "Signed in. Close this tab — the dashboard has already picked it up.")
}

// exchangeBrowserCode trades a code for a token, stores it, and settles the
// pending login. Shared by the loopback callback and the paste-the-code exit.
func (s *Server) exchangeBrowserCode(ctx context.Context, key string, lg *oauthLogin, code string) (*oauth.Token, error) {
	spec, _, ok := s.spec(key)
	if !ok {
		s.failBrowserLogin(key, lg, "the OAuth account entry disappeared while signing in")
		return nil, oauthRejected("that account is no longer configured")
	}
	s.oa.mu.Lock()
	sess := lg.pkce
	s.oa.mu.Unlock()
	if sess == nil {
		return nil, oauthRejected("this sign-in was not started from the dashboard")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tok, err := spec.Provider.ExchangeCode(ctx, nil, code, sess.RedirectURI, sess.Verifier)
	if err != nil {
		s.failBrowserLogin(key, lg, err.Error())
		return nil, oauthRejected(err.Error())
	}
	if err := s.oauth.Store().Put(key, *tok); err != nil {
		s.failBrowserLogin(key, lg, "store token: "+err.Error())
		return nil, oauthRejected("token received, but onegw could not store it: " + err.Error())
	}
	s.oa.mu.Lock()
	if !lg.finished {
		lg.finished = true
		lg.err = ""
		close(lg.done)
	}
	if lg.cancel != nil {
		lg.cancel()
	}
	s.oa.mu.Unlock()
	s.finishOAuthLogin(key)
	log.Printf("admin: oauth login %s signed in from the browser callback", key)
	return tok, nil
}

// failBrowserLogin records a terminal error for a pending browser login and
// releases whoever waits on the flow.
func (s *Server) failBrowserLogin(key string, lg *oauthLogin, msg string) {
	s.oa.mu.Lock()
	if !lg.finished {
		lg.finished = true
		lg.err = msg
		close(lg.done)
		if lg.pkce != nil {
			delete(s.oa.states, lg.pkce.State)
		}
	}
	if lg.cancel != nil {
		lg.cancel()
	}
	s.oa.mu.Unlock()
	s.events.publish("config", `{"oauth":`+jsonString(key)+`,"state":"failed"}`)
	s.closeCallback()
}

// handleAdminOAuthExchange finishes a browser login from a code the operator
// pastes — the way in when their browser cannot reach this host's loopback
// (9router's manual-code route, same purpose). Accepts the bare code or the
// whole callback URL from the address bar.
func (s *Server) handleAdminOAuthExchange(w http.ResponseWriter, r *http.Request) {
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
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	code := oauthExtractCode(req.Code)
	if code == "" {
		adminError(w, http.StatusBadRequest, "code is empty — paste the ?code= value or the whole callback URL")
		return
	}
	s.oa.mu.Lock()
	lg := s.oa.active[key]
	pending := lg != nil && !lg.finished && lg.pkce != nil
	s.oa.mu.Unlock()
	if !pending {
		adminError(w, http.StatusConflict, "no browser sign-in is pending for "+key+" — click Sign in again first")
		return
	}
	if _, err := s.exchangeBrowserCode(r.Context(), key, lg, code); err != nil {
		adminError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"key": key, "state": oauthSignedIn})
}

// oauthExtractCode accepts a raw code or the full callback URL (what the
// browser address bar shows when the loopback redirect was unreachable).
func oauthExtractCode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && (u.Scheme != "" || strings.Contains(s, "?")) {
		if c := u.Query().Get("code"); c != "" {
			return c
		}
	}
	if i := strings.Index(s, "code="); i >= 0 {
		s = s[i+5:]
		if j := strings.IndexAny(s, "&#; \t\n"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

// rejectedError wraps an exchange failure the caller should surface verbatim.
type rejectedError struct{ msg string }

func (e rejectedError) Error() string { return e.msg }

func oauthRejected(msg string) error { return rejectedError{msg: msg} }

// oauthCallbackPage renders the loopback response. It opens in the vendor's
// redirect, not in the dashboard, so it is a standalone document.
func oauthCallbackPage(w http.ResponseWriter, status int, ok bool, msg string) {
	heading := "Sign-in failed"
	if ok {
		heading = "Signed in"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>onegw · %[1]s</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#101014;
color:#e8e8ea;font:15px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace}
div{max-width:34rem;padding:2rem}h1{margin:0 0 .5rem;font-size:1.2rem;color:%[2]s}
p{margin:0;opacity:.75;white-space:pre-wrap;overflow-wrap:anywhere}</style></head>
<body><div><h1>%[1]s</h1><p>%[3]s</p></div></body></html>`,
		html.EscapeString(heading),
		map[bool]string{true: "#7ee2a8", false: "#ff8f8f"}[ok],
		html.EscapeString(msg))
}
