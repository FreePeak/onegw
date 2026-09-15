package oauth

// Authorization-code + PKCE (RFC 7636): the flow where the operator's browser
// opens the vendor's authorize URL, logs in, and gets redirected back to a
// LOOPBACK address this process owns. The redirect carries `code`, which is
// exchanged for the token with the PKCE verifier that only this process holds
// — no device code to read out and type.
//
// This is how the official Grok CLI logs in and what 9router's xAI
// "Grok Build OAuth" does (src/lib/oauth/providers/xai.js:36-79 plus the
// 127.0.0.1:56121/callback loopback proxy in utils/server.js:298-435). onegw
// mirrors it endpoint for endpoint: same public client id, S256 challenge,
// fixed loopback port and /callback path — and the same "paste the code back"
// fallback for a browser that cannot reach us (9router's manual-code route).
//
// A profile opts in by setting AuthURL; device flow stays available for every
// profile, because it is the only option when the browser is on another box.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// DefaultCallbackPort is the loopback port the public Grok CLI client
// registers (and the one 9router binds). xAI accepts this fixed address; a
// different port is only used when something else already holds it.
const DefaultCallbackPort = 56121

// pkceVerifierBytes is the random length behind the code verifier: base64url
// of 96 bytes is 128 characters, inside RFC 7636's 43..128 window.
const pkceVerifierBytes = 96

// BrowserFlow reports whether this profile can do an authorization-code +
// PKCE login; false means device flow only.
func (p Provider) BrowserFlow() bool { return p.AuthURL != "" }

// RedirectPath is the loopback path the vendor redirects back to.
func (p Provider) RedirectPath() string {
	if p.CallbackPath != "" {
		return p.CallbackPath
	}
	return "/callback"
}

// RedirectURI builds the loopback redirect address for a bound port. Pass
// port 0 for the profile's fixed port.
func (p Provider) RedirectURI(port int) string {
	if port <= 0 {
		port = p.CallbackPort
	}
	if port <= 0 {
		port = DefaultCallbackPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, p.RedirectPath())
}

// PKCESession is one in-flight browser login. The verifier stays here, server
// side; the authorize URL carries only its SHA-256, and `state` is the
// CSRF-matching key the callback is looked up by.
type PKCESession struct {
	State       string
	Verifier    string
	RedirectURI string
	AuthURL     string // what the operator opens
}

// NewPKCE builds the challenge for p and the URL to open. redirectURI ""
// means the profile's fixed loopback address; the caller passes its actual
// bound address when the fixed port was busy.
func NewPKCE(p Provider, redirectURI string) (PKCESession, error) {
	if !p.BrowserFlow() {
		return PKCESession{}, fmt.Errorf("%s has no authorization endpoint (device flow only)", p.Name)
	}
	if p.ClineFlow {
		// No challenge to build: the redirect carries the credential itself.
		return newClineSession(p, redirectURI)
	}
	verifier, err := randomB64URL(pkceVerifierBytes)
	if err != nil {
		return PKCESession{}, fmt.Errorf("pkce verifier: %w", err)
	}
	state, err := randomB64URL(16)
	if err != nil {
		return PKCESession{}, fmt.Errorf("pkce state: %w", err)
	}
	s := PKCESession{State: state, Verifier: verifier, RedirectURI: p.RedirectURI(0)}
	if redirectURI != "" {
		s.RedirectURI = redirectURI
	}
	sum := sha256.Sum256([]byte(verifier))

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", s.RedirectURI)
	q.Set("scope", p.Scope)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", s.State)
	if p.Nonce {
		// OIDC replay binding. The id_token is not validated here (the bearer
		// token is what onegw uses), so the nonce is sent and forgotten — the
		// CLI sends one and xAI's authorize page expects the parameter.
		if n, err := randomHex(16); err == nil {
			q.Set("nonce", n)
		}
	}
	for k, v := range p.AuthExtra {
		q.Set(k, v)
	}
	s.AuthURL = p.AuthURL + "?" + q.Encode()
	return s, nil
}

// ExchangeCode trades the callback's `code` for a token. The caller already
// matched `state` to this session, so it is not re-checked here.
func (p Provider) ExchangeCode(ctx context.Context, hc *http.Client, code, redirectURI, verifier string) (*Token, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if p.ClineFlow {
		return p.exchangeCline(ctx, hc, code, redirectURI)
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {p.ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	status, body, err := postFormRaw(ctx, hc, p.TokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("%s code exchange: %w", p.Name, err)
	}
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &raw)
	if raw.Error != "" {
		return nil, fmt.Errorf("%s code exchange: %s (%s)", p.Name, raw.Error, raw.ErrorDescription)
	}
	if status >= 400 {
		return nil, fmt.Errorf("%s code exchange: HTTP %d: %s", p.Name, status, truncate(body))
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("%s code exchange: no access_token in response", p.Name)
	}
	return &Token{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresAt:    tokenExpiry(time.Now(), raw.ExpiresIn, p.MaxTokenTTL),
		Scope:        raw.Scope,
	}, nil
}

func randomB64URL(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
