package oauth

import (
	"context"
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

// Provider describes one OAuth device-flow provider.
type Provider struct {
	Name          string // config name: "xai", "kilocode"
	DeviceCodeURL string // RFC 8628: where a device code is requested
	TokenURL      string // where polls and refresh grants go
	Scope         string
	Extra         map[string]string // form fields appended to the start request (e.g. referrer)
	ClientID      string

	// KiloDialect switches the poller to Kilo Code's bespoke device-auth
	// API (not RFC 8628): initiate POSTs the start URL and returns
	// {code, verificationUrl, expiresIn}; polls are GETs against
	// {TokenURL}/{code} answered with 202/403/410 or an approved status.
	KiloDialect bool

	// StartTokenURL receives the initiation POST when it differs from
	// TokenURL (defaults to TokenURL when empty).
	StartTokenURL string
}

// Providers lists the built-in provider names, sorted.
func Providers() []string {
	return []string{"kilocode", "xai"}
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
		}, true
	case "kilocode":
		return Provider{
			Name:        "kilocode",
			TokenURL:    "https://api.kilo.ai/api/device-auth/codes",
			KiloDialect: true,
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
	if raw.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	return &tok, nil
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
