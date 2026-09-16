package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider registry: built-in device-flow providers, keyed by the config
// name used in [[oauth.accounts]]. Adding a subscription provider (Claude
// Code, Codex, ...) later means adding an entry here — the framework,
// store, refresher, and CLI are provider-agnostic.
//
// Endpoints mirror the 9router registry (wire-captured from the official
// CLIs). Client IDs are public (native apps); no secrets involved.

// Provider describes one OAuth login profile: a device-flow dialect and, for
// the vendors that have one, an authorization-code + PKCE browser flow.
type Provider struct {
	Name          string // config name: "xai", "kilocode"
	DeviceCodeURL string // RFC 8628: where a device code is requested
	TokenURL      string // where polls and refresh grants go
	Scope         string
	Extra         map[string]string // form fields appended to the start request (e.g. referrer)
	ClientID      string

	// AuthURL is the authorize endpoint; non-empty opts the profile into the
	// browser (authorization-code + PKCE) flow. CallbackPort/CallbackPath
	// fix the loopback redirect address the vendor has registered for
	// ClientID, and AuthExtra/Nonce carry the extra authorize parameters the
	// real client sends (plan, referrer, nonce). See pkce.go.
	AuthURL      string
	CallbackPort int
	CallbackPath string
	AuthExtra    map[string]string
	Nonce        bool

	// MaxTokenTTL caps the lifetime onegw trusts a token to have, no
	// matter what the vendor's expires_in claims. xAI answers 21600 (6 h)
	// for device-flow tokens but revokes them silently at ~40-45 min
	// (9router's grok-cli login works around the same lie): believing 6 h
	// means the refresh loop sleeps while every request 403s. It also
	// stands in when a response carries no expires_in, which would
	// otherwise read as "no expiry known" and refresh every tick.
	MaxTokenTTL time.Duration

	// KiloDialect switches the poller to Kilo Code's bespoke device-auth
	// API (not RFC 8628): initiate POSTs the start URL and returns
	// {code, verificationUrl, expiresIn}; polls are GETs against
	// {TokenURL}/{code} answered with 202/403/410 or an approved status.
	KiloDialect bool

	// StartTokenURL receives the initiation POST when it differs from
	// TokenURL (defaults to TokenURL when empty).
	StartTokenURL string

	// ClineFlow marks the Cline/ClinePass credential dialect: the browser
	// login carries no PKCE and no client_id, the credential arrives
	// base64-wrapped in the redirect's `code` (with a camelCase JSON POST as
	// the fallback), refresh is a JSON grant at ClineRefreshURL rather than a
	// form post at TokenURL, and the stored bearer is normalized to
	// `workos:<jwt>`. There is no device flow. See cline.go.
	ClineFlow bool
	// ClineRefreshURL is where a ClineFlow refresh is POSTed. A field rather
	// than a constant so a test can point the whole dialect at its own server.
	ClineRefreshURL string

	// ClineAuthenticateURL and ClineRegisterURL are the device-login hops that
	// differ from the browser login's endpoints: the poll answers at the
	// authorization server (WorkOS user_management, not Cline), and its token
	// pair is NOT the inference credential until Cline core exchanges it at
	// RegisterURL. Empty falls back to the vendor's own URLs; a test overrides
	// them to point the whole flow at a stub.
	ClineAuthenticateURL string
	ClineRegisterURL     string
}

// Providers lists the built-in provider names, sorted.
func Providers() []string {
	return []string{"cline", "clinepass", "kilocode", "xai"}
}

// Lookup returns the built-in provider spec by name.
func Lookup(name string) (Provider, bool) {
	switch name {
	case "xai":
		return Provider{
			Name:          "xai",
			ClientID:      "b1a00492-073a-47ea-816f-4c329264a828", // public Grok CLI client
			DeviceCodeURL: "https://auth.x.ai/oauth2/device/code",
			TokenURL:      "https://auth.x.ai/oauth2/token",
			Scope:         "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write",
			Extra:         map[string]string{"referrer": "grok-build"},
			// The Grok CLI's own login: browser authorize + loopback
			// callback on the registered fixed port. Params mirror
			// CLIProxyAPI's BuildAuthorizeURL (via 9router), which is what
			// this public client id is provisioned for.
			AuthURL:      "https://auth.x.ai/oauth2/authorize",
			CallbackPort: DefaultCallbackPort,
			CallbackPath: "/callback",
			AuthExtra:    map[string]string{"plan": "generic", "referrer": "cli-proxy-api"},
			Nonce:        true,
			// SuperGrok device tokens die silently at ~40-45 min despite
			// the 6 h expires_in, so trust 40 and let the loop rotate.
			MaxTokenTTL: 40 * time.Minute,
		}, true
	case "kilocode":
		return Provider{
			Name:        "kilocode",
			TokenURL:    "https://api.kilo.ai/api/device-auth/codes",
			KiloDialect: true,
		}, true
	case "cline", "clinepass":
		// ClinePass is a plan inside the same account: identical authorize,
		// token, register, refresh and chat endpoints, and the same
		// /api/v1/chat/completions upstream — only the catalog differs (the
		// `cline-pass/` ids the subscription unlocks). Two profiles exist so a
		// config can bind them to separate [[providers]] rows, one of which
		// borrows the other's login via `owner`, mirroring 9router's registry
		// (providers/registry/cline.js + clinepass.js).
		return Provider{
			Name: name,
			// Browser flow (9router's choice, and the Cline SDK's fallback):
			// no PKCE, credential in the redirect `code`.
			AuthURL:         clineAuthorizeURL,
			TokenURL:        clineTokenURL,
			ClineFlow:       true,
			ClineRefreshURL: clineRefreshURL,
			// Callback: cline bounces the browser through WorkOS and back to the
			// exact callback_url it is handed (measured: any 127.0.0.1/localhost
			// port and path is accepted), so the shared listener address works
			// unchanged. The per-login token rides in the path, because `state`
			// is not echoed.
			CallbackPort: DefaultCallbackPort,
			CallbackPath: "/callback",
			// Device flow (the Cline SDK's DEFAULT, useWorkOSDeviceAuth ?? true):
			// RFC 8628-shaped against WorkOS user_management with this public
			// client id, then one extra hop at Cline core. Scope stays empty —
			// the device authorize takes client_id alone.
			DeviceCodeURL:        clineWorkOSDeviceURL,
			ClientID:             clineWorkOSClientID,
			ClineAuthenticateURL: clineWorkOSAuthenticateURL,
			ClineRegisterURL:     clineRegisterURL,
		}, true
	default:
		return Provider{}, false
	}
}

// PollerFor builds a Poller for p using the given HTTP client (nil =
// default).
func PollerFor(p Provider, hc *http.Client) Poller {
	if hc == nil {
		hc = http.DefaultClient
	}
	if p.ClineFlow {
		return clineDevicePoller{p: p, hc: hc}
	}
	if p.KiloDialect {
		return kiloPoller{p: p, hc: hc}
	}
	return rfc8628{p: p, hc: hc}
}

// ---------------------------------------------------------------------------
// RFC 8628 (xAI / Grok)
// ---------------------------------------------------------------------------

type rfc8628 struct {
	p  Provider
	hc *http.Client
}

type rfc8628Start struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func (r rfc8628) Start(ctx context.Context) (*DeviceStart, error) {
	form := url.Values{"client_id": {r.p.ClientID}}
	if r.p.Scope != "" {
		form.Set("scope", r.p.Scope)
	}
	for k, v := range r.p.Extra {
		form.Set(k, v)
	}
	body, err := postFormJSON(ctx, r.hc, r.p.DeviceCodeURL, form)
	if err != nil {
		return nil, err
	}
	var raw rfc8628Start
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%s device start: %w", r.p.Name, err)
	}
	if raw.DeviceCode == "" || raw.VerificationURI == "" {
		return nil, fmt.Errorf("%s device start: missing device_code/verification_uri", r.p.Name)
	}
	return &DeviceStart{
		DeviceCode:              raw.DeviceCode,
		UserCode:                raw.UserCode,
		VerificationURL:         raw.VerificationURI,
		VerificationURLComplete: raw.VerificationURIComplete,
		ExpiresIn:               secsOrDefault(raw.ExpiresIn, defaultDeviceExpiry),
		Interval:                secsOrDefault(raw.Interval, defaultPollInterval),
	}, nil
}

func (r rfc8628) Poll(ctx context.Context, deviceCode string) (*Token, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {r.p.ClientID},
	}
	status, body, err := postFormRaw(ctx, r.hc, r.p.TokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if status >= 500 {
		// 5xx means the server is unhappy regardless of the body: retry.
		return nil, fmt.Errorf("%w: %s token poll HTTP %d: %s", ErrTransient, r.p.Name, status, truncate(body))
	}
	_ = json.Unmarshal(body, &raw)
	switch raw.Error {
	case "":
		// fall through to the success path
	case "authorization_pending":
		return nil, ErrPending
	case "slow_down":
		return nil, ErrSlowDown
	case "expired_token":
		return nil, ErrExpired
	case "access_denied":
		return nil, ErrDenied
	case "server_error", "temporarily_unavailable":
		// RFC 8628 §3.5: retryable.
		return nil, fmt.Errorf("%w: %s", ErrTransient, raw.Error)
	default:
		return nil, fmt.Errorf("%s token poll: %s (%s)", r.p.Name, raw.Error, raw.ErrorDescription)
	}
	if status >= 400 {
		// 4xx without a known error field: malformed or rejected — fatal.
		return nil, fmt.Errorf("%s token poll HTTP %d: %s", r.p.Name, status, truncate(body))
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("%s token poll: no access_token in response", r.p.Name)
	}
	tok := Token{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, Scope: raw.Scope}
	tok.ExpiresAt = tokenExpiry(time.Now(), raw.ExpiresIn, r.p.MaxTokenTTL)
	return &tok, nil
}

// tokenExpiry turns a vendor expires_in into an instant, capped at the
// profile's MaxTokenTTL: a vendor that overstates (xAI claims 6 h for
// device tokens it revokes in ~45 min) or omits the field still gets a
// lifetime onegw can refresh inside. Zero means "nothing to go on".
func tokenExpiry(now time.Time, expiresIn int, maxTTL time.Duration) time.Time {
	ttl := time.Duration(expiresIn) * time.Second
	if maxTTL > 0 && (ttl <= 0 || ttl > maxTTL) {
		ttl = maxTTL
	}
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

// accessExpiry reads the `exp` claim from a JWT access token — the expiry the
// vendor actually signed, so it outranks whatever the store was told. Opaque
// bearer tokens, or a JWT without exp, report false and change nothing.
func accessExpiry(jwt string) (time.Time, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var cl struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &cl) != nil || cl.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(cl.Exp, 0).UTC(), true
}

// ---------------------------------------------------------------------------
// Kilo Code device dialect: POST /api/device-auth/codes → {code,
// verificationUrl, expiresIn}; GET /api/device-auth/codes/{code} →
// 202 pending, 403 denied, 410 expired, 200 {status:"approved", token}.
// Tokens do not refresh (no refresh token issued): re-login on expiry.
// ---------------------------------------------------------------------------

type kiloPoller struct {
	p  Provider
	hc *http.Client
}

type kiloStart struct {
	Code            string `json:"code"`
	VerificationURL string `json:"verificationUrl"`
	ExpiresIn       int    `json:"expiresIn"`
}

func (k kiloPoller) Start(ctx context.Context) (*DeviceStart, error) {
	startURL := k.p.StartTokenURL
	if startURL == "" {
		startURL = k.p.TokenURL
	}
	body, err := postFormJSON(ctx, k.hc, startURL, url.Values{})
	if err != nil {
		return nil, err
	}
	var raw kiloStart
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("kilo device start: %w", err)
	}
	if raw.Code == "" || raw.VerificationURL == "" {
		return nil, fmt.Errorf("kilo device start: missing code/verificationUrl")
	}
	return &DeviceStart{
		DeviceCode:      raw.Code,
		UserCode:        raw.Code, // same value; the URL carries it
		VerificationURL: raw.VerificationURL,
		ExpiresIn:       secsOrDefault(raw.ExpiresIn, 5*time.Minute), // 9router: 300s default
		Interval:        3 * time.Second,                             // 9router polls every 3s
	}, nil
}

func (k kiloPoller) Poll(ctx context.Context, deviceCode string) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(k.p.TokenURL, "/")+"/"+url.PathEscape(deviceCode), nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil, ErrPending
	case http.StatusForbidden:
		return nil, ErrDenied
	case http.StatusGone:
		return nil, ErrExpired
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: kilo poll HTTP %d: %s", ErrTransient, resp.StatusCode, truncate(body))
	}
	var raw struct {
		Status    string `json:"status"`
		Token     string `json:"token"`
		UserEmail string `json:"userEmail"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: kilo poll: %v", ErrTransient, err)
	}
	if raw.Status != "approved" || raw.Token == "" {
		return nil, ErrPending
	}
	return &Token{AccessToken: raw.Token}, nil
}

// ---------------------------------------------------------------------------
// shared HTTP helpers
// ---------------------------------------------------------------------------

// postFormJSON POSTs form-encoded fields and returns the JSON body; any
// non-2xx is an error carrying the body.
func postFormJSON(ctx context.Context, hc *http.Client, endpoint string, form url.Values) ([]byte, error) {
	status, body, err := postFormRaw(ctx, hc, endpoint, form)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("%s: HTTP %d: %s", endpoint, status, truncate(body))
	}
	return body, nil
}

// postFormRaw POSTs form-encoded fields, returning (status, body).
func postFormRaw(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

func secsOrDefault(n int, d time.Duration) time.Duration {
	if n <= 0 {
		return d
	}
	return time.Duration(n) * time.Second
}

func truncate(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
