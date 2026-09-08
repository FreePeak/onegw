package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Manager owns the OAuth accounts of one gateway process: token resolution
// for request-time injection, a per-account refresh goroutine (refresh with
// lead time before expiry, single-flight per account), and the CLI login
// entrypoint. It survives config hot reloads; Sync (start/stop goroutines)
// runs on every apply.
type Manager struct {
	store *TokenStore
	hc    *http.Client

	// Cooler, when set, is called with the account key after a failed
	// refresh so the gateway can stop routing to that account until the
	// next attempt.
	Cooler func(key string)

	// wake interrupts the refresh loops' sleep when settings change
	// (SetCheckEvery/SetLead), so tests and hot reloads take effect fast.
	wake chan struct{}

	mu         sync.Mutex
	checkEvery time.Duration // refresh-loop wake interval (default 30s)
	lead       time.Duration // refresh this long before expiry (default 5m)
	specs      map[string]AccountSpec
	stop       map[string]context.CancelFunc
	busy       map[string]bool // single-flight per account
	closed     bool
}

// AccountSpec is one managed OAuth account: its store key ("provider/
// account") and the resolved provider profile (endpoints, client id).
type AccountSpec struct {
	Key      string
	Provider Provider
}

// NewManager builds a manager over st.
func NewManager(st *TokenStore) *Manager {
	return &Manager{
		store:      st,
		wake:       make(chan struct{}),
		hc:         &http.Client{Timeout: 30 * time.Second},
		checkEvery: 30 * time.Second,
		lead:       5 * time.Minute,
		specs:      map[string]AccountSpec{},
		stop:       map[string]context.CancelFunc{},
		busy:       map[string]bool{},
	}
}

// SetCheckEvery overrides the refresh-loop wake interval (tests, tuning)
// and wakes every loop immediately.
func (m *Manager) SetCheckEvery(d time.Duration) {
	m.mu.Lock()
	m.checkEvery = d
	m.broadcastLocked()
	m.mu.Unlock()
}

// SetLead overrides the refresh lead time and wakes every loop.
func (m *Manager) SetLead(d time.Duration) {
	m.mu.Lock()
	m.lead = d
	m.broadcastLocked()
	m.mu.Unlock()
}

// broadcastLocked wakes every refresh loop by closing the current wake
// channel and installing a fresh one. Close is a broadcast: every loop's
// select sees it. Callers hold mu.
func (m *Manager) broadcastLocked() {
	close(m.wake)
	m.wake = make(chan struct{})
}

// Token returns the current access token for key, or "" when none is
// stored. This is the request-time resolver handed to provider accounts.
func (m *Manager) Token(key string) string {
	tok, ok := m.store.Get(key)
	if !ok {
		return ""
	}
	return tok.AccessToken
}

// Store exposes the token store (CLI listing, tests).
func (m *Manager) Store() *TokenStore { return m.store }

// Specs returns the currently managed account specs (diagnostics, tests).
func (m *Manager) Specs() []AccountSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AccountSpec, 0, len(m.specs))
	for _, s := range m.specs {
		out = append(out, s)
	}
	return out
}

// Sync aligns the running refresh goroutines with specs: starts loops for
// new accounts, restarts loops whose provider profile changed, stops
// removed ones. Called on every config apply.
func (m *Manager) Sync(specs []AccountSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	want := make(map[string]AccountSpec, len(specs))
	for _, s := range specs {
		if s.Key == "" {
			continue
		}
		want[s.Key] = s
	}
	for key, cancel := range m.stop {
		if _, keep := want[key]; !keep {
			cancel()
			delete(m.stop, key)
			delete(m.specs, key)
		}
	}
	for key, spec := range want {
		if old, running := m.specs[key]; running && sameSpec(old, spec) {
			continue
		}
		if cancel, running := m.stop[key]; running {
			cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.stop[key] = cancel
		m.specs[key] = spec
		go m.refreshLoop(ctx, spec)
	}
}

// Stop cancels every refresh loop. The token store stays valid.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for key, cancel := range m.stop {
		cancel()
		delete(m.stop, key)
	}
	m.specs = map[string]AccountSpec{}
}

// Login runs the full device flow for spec against the live provider
// endpoints: start, prompt (print URL + code), poll until authorized, then
// store the token. Used by the CLI and tests.
func (m *Manager) Login(ctx context.Context, spec AccountSpec, prompt func(DeviceStart)) (*Token, error) {
	poller := PollerFor(spec.Provider, m.hc)
	ds, err := poller.Start(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := Wait(ctx, poller, ds, prompt)
	if err != nil {
		return nil, err
	}
	if err := m.store.Put(spec.Key, *tok); err != nil {
		return nil, fmt.Errorf("store token: %w", err)
	}
	return tok, nil
}

// RefreshNow forces one refresh for key, ignoring the expiry lead. Used by
// the CLI and tests.
func (m *Manager) RefreshNow(ctx context.Context, key string) error {
	m.mu.Lock()
	spec, ok := m.specs[key]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("oauth account %q is not configured", key)
	}
	if _, err := m.refresh(ctx, spec); err != nil {
		if m.Cooler != nil {
			m.Cooler(key)
		}
		return err
	}
	return nil
}

// refreshLoop wakes every checkEvery to top up the token before it expires.
// A wake signal (settings change) restarts the interval immediately.
func (m *Manager) refreshLoop(ctx context.Context, spec AccountSpec) {
	for {
		m.mu.Lock()
		every, lead, closed, wake := m.checkEvery, m.lead, m.closed, m.wake
		m.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
			continue
		case <-time.After(every):
		}
		m.maybeRefresh(ctx, spec, lead)
	}
}

// maybeRefresh refreshes spec's token if a refresh token exists and expiry
// is within lead. Single-flight per account: a concurrent refresh (another
// tick racing RefreshNow) skips.
func (m *Manager) maybeRefresh(ctx context.Context, spec AccountSpec, lead time.Duration) {
	tok, ok := m.store.Get(spec.Key)
	if !ok || tok.RefreshToken == "" || tok.AccessToken == "" {
		return // nothing to refresh (e.g. Kilo: no refresh token)
	}
	if !tok.ExpiresAt.IsZero() && time.Until(tok.ExpiresAt) > lead {
		return
	}
	m.mu.Lock()
	if m.busy[spec.Key] {
		m.mu.Unlock()
		return
	}
	m.busy[spec.Key] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.busy, spec.Key)
		m.mu.Unlock()
	}()

	if _, err := m.refresh(ctx, spec); err != nil {
		// Stale token kept: the account keeps serving until the gateway
		// cools it (Cooler) and the next tick retries the refresh.
		if m.Cooler != nil {
			m.Cooler(spec.Key)
		}
	}
}

// refresh exchanges the stored refresh token for a new access token and
// persists it. Returns the new token.
func (m *Manager) refresh(ctx context.Context, spec AccountSpec) (*Token, error) {
	old, ok := m.store.Get(spec.Key)
	if !ok || old.RefreshToken == "" {
		return nil, errors.New("no refresh token stored for " + spec.Key)
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {old.RefreshToken},
		"client_id":     {spec.Provider.ClientID},
	}
	status, body, err := postFormRaw(ctx, m.hc, spec.Provider.TokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("token refresh: HTTP %d: %s", status, truncate(body))
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("token refresh: no access_token in response")
	}
	tok := Token{AccessToken: raw.AccessToken, Scope: raw.Scope}
	if raw.RefreshToken != "" {
		tok.RefreshToken = raw.RefreshToken // rotation: use the new one
	} else {
		tok.RefreshToken = old.RefreshToken
	}
	if raw.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	} else {
		tok.ExpiresAt = old.ExpiresAt
	}
	if err := m.store.Put(spec.Key, tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// sameSpec reports whether two account specs are identical (Provider
// contains a map, so it is not comparable with ==).
func sameSpec(a, b AccountSpec) bool {
	if a.Key != b.Key || a.Provider.Name != b.Provider.Name ||
		a.Provider.DeviceCodeURL != b.Provider.DeviceCodeURL ||
		a.Provider.TokenURL != b.Provider.TokenURL ||
		a.Provider.Scope != b.Provider.Scope ||
		a.Provider.ClientID != b.Provider.ClientID ||
		a.Provider.KiloDialect != b.Provider.KiloDialect ||
		a.Provider.StartTokenURL != b.Provider.StartTokenURL ||
		len(a.Provider.Extra) != len(b.Provider.Extra) {
		return false
	}
	for k, v := range a.Provider.Extra {
		if b.Provider.Extra[k] != v {
			return false
		}
	}
	return true
}
