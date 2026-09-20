// Package oauth implements OAuth device-flow logins for subscription
// providers (xAI/Grok, Kilo Code, later Claude Code/Codex/Copilot per #2):
// RFC 8628 and provider-specific device dialects, a token store under the
// data dir, and a per-account auto-refresher with single-flight refreshes.
// Tokens are injected into upstream requests at call time via the provider
// Account hook — they never sit in TOML.
package oauth

import (
	"context"
	"errors"
	"time"
)

// Sentinel poll outcomes driving the device-flow wait loop.
var (
	ErrPending   = errors.New("authorization_pending")
	ErrSlowDown  = errors.New("slow_down")
	ErrExpired   = errors.New("expired_token")
	ErrDenied    = errors.New("access_denied")
	ErrTransient = errors.New("transient poll failure") // retry on next tick
)

// Token is a stored OAuth credential.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"` // zero = no expiry known
	Scope        string    `json:"scope,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DeviceStart is the payload an authorization server returns when a device
// flow begins: what the user must visit and enter, and how the client polls.
type DeviceStart struct {
	DeviceCode              string
	UserCode                string
	VerificationURL         string
	VerificationURLComplete string
	ExpiresIn               time.Duration // device code lifetime; 0 = 10 min
	Interval                time.Duration // poll interval; 0 = 5 s
}

// Poller is one device-flow dialect: how to start it and what one poll
// attempt returns. Poll returns the token on success, a sentinel error
// (ErrPending/ErrSlowDown/ErrExpired/ErrDenied) to drive the loop, an
// ErrTransient-wrapped error to retry, or any other error to abort.
type Poller interface {
	Start(ctx context.Context) (*DeviceStart, error)
	Poll(ctx context.Context, deviceCode string) (*Token, error)
}

const (
	defaultPollInterval = 5 * time.Second
	defaultDeviceExpiry = 10 * time.Minute
)

// slowDownPenalty is the RFC 8628 minimum interval bump after a slow_down.
// A variable so tests can tighten it.
var slowDownPenalty = 5 * time.Second

// Wait runs the polling loop after a successful Start: call Poll every
// start.Interval, honoring slow-down bumps, until a token arrives, the
// device code expires, the user denies access, or ctx ends. onPrompt runs
// once with the start payload (the CLI prints the URL + user code from it).
func Wait(ctx context.Context, p Poller, ds *DeviceStart, onPrompt func(DeviceStart)) (*Token, error) {
	if onPrompt != nil {
		onPrompt(*ds)
	}
	interval := ds.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	expiry := ds.ExpiresIn
	if expiry <= 0 {
		expiry = defaultDeviceExpiry
	}
	deadline := time.Now().Add(expiry)
	for {
		tok, err := p.Poll(ctx, ds.DeviceCode)
		switch {
		case err == nil:
			return tok, nil
		case errors.Is(err, ErrPending):
		case errors.Is(err, ErrSlowDown):
			interval += slowDownPenalty
		case errors.Is(err, ErrExpired):
			return nil, ErrExpired
		case errors.Is(err, ErrDenied):
			return nil, ErrDenied
		case errors.Is(err, ErrTransient):
			// Network hiccup or 5xx: keep polling.
		default:
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, ErrExpired
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}
