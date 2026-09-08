package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake8628 is an RFC 8628 device authorization server: /device starts a
// flow, /token answers per a scripted sequence of poll outcomes.
type fake8628 struct {
	expiresIn int // advertised device-code lifetime, seconds (0 = 2)

	mu              sync.Mutex
	polls           int
	outcomes        []pollOutcome // consumed per poll; last one repeats
	starts          int
	refreshes       int
	lastStartForm   map[string]string
	lastTokenForm   map[string]string
	lastRefreshForm map[string]string
	tokensIssued    atomic.Int64
}

type pollOutcome struct {
	status int // 0 = 200 OK with token
	err    string
}

// expiresInOr returns the fake's configured device-code lifetime.
func (f *fake8628) expiresInOr(def int) int {
	if f.expiresIn > 0 {
		return f.expiresIn
	}
	return def
}

func (f *fake8628) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.starts++
		_ = r.ParseForm()
		f.lastStartForm = formMap(r.PostForm)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "DEV-123",
			"user_code":                 "ABCD-WXYZ",
			"verification_uri":          "https://auth.example.com/activate",
			"verification_uri_complete": "https://auth.example.com/activate?code=ABCD-WXYZ",
			"expires_in":                f.expiresInOr(2),
			"interval":                  1, // 1s: expiry tests finish fast
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			i := f.polls
			if i >= len(f.outcomes) {
				i = len(f.outcomes) - 1
			}
			out := f.outcomes[i]
			f.polls++
			f.lastTokenForm = formMap(r.PostForm)
			if out.status != 0 {
				w.WriteHeader(out.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": out.err})
				return
			}
			n := f.tokensIssued.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-" + strconv.FormatInt(n, 10),
				"refresh_token": "rt-" + strconv.FormatInt(n, 10),
				"expires_in":    3600,
				"scope":         "api:access",
			})
		case "refresh_token":
			f.refreshes++
			f.lastRefreshForm = formMap(r.PostForm)
			n := f.tokensIssued.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-" + strconv.FormatInt(n, 10),
				"refresh_token": "rt-" + strconv.FormatInt(n, 10), // rotation
				"expires_in":    3600,
			})
		default:
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
		}
	})
	return mux
}

func (f *fake8628) server() *httptest.Server {
	return httptest.NewServer(f.handler())
}

func formMap(v map[string][]string) map[string]string {
	m := map[string]string{}
	for k, vs := range v {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return m
}

func xaiLike(serverURL string) Provider {
	return Provider{
		Name:          "xai",
		ClientID:      "test-client-id",
		DeviceCodeURL: serverURL + "/device",
		TokenURL:      serverURL + "/token",
		Scope:         "openid offline_access api:access",
		Extra:         map[string]string{"referrer": "grok-build"},
	}
}

func TestMain(m *testing.M) {
	// Tighten the RFC slow_down penalty so scripted slow_down outcomes do
	// not add 5 real seconds to a test.
	slowDownPenalty = time.Millisecond
	os.Exit(m.Run())
}

func TestDeviceFlowEndToEnd(t *testing.T) {
	f := &fake8628{
		expiresIn: 30, // generous: 3 pending polls at 1s intervals
		outcomes: []pollOutcome{
			{status: 400, err: "authorization_pending"},
			{status: 400, err: "slow_down"},
			{status: 400, err: "authorization_pending"},
			{}, // success
		},
	}
	srv := f.server()
	defer srv.Close()

	store := NewTokenStore("memory")
	mgr := NewManager(store)
	mgr.SetCheckEvery(time.Hour) // no background refresh during the test
	mgr.SetLead(time.Minute)

	var prompts []DeviceStart
	tok, err := mgr.Login(context.Background(), AccountSpec{Key: "xai/main", Provider: xaiLike(srv.URL)}, func(ds DeviceStart) {
		prompts = append(prompts, ds)
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Fatalf("unexpected token %+v", tok)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("expiry not set from expires_in")
	}
	if len(prompts) != 1 || prompts[0].UserCode != "ABCD-WXYZ" || prompts[0].VerificationURL == "" {
		t.Fatalf("prompt payload wrong: %+v", prompts)
	}
	// Start request carries client_id, scope, and extra fields; poll
	// request is a proper RFC 8628 token request.
	f.mu.Lock()
	startForm, tokenForm := f.lastStartForm, f.lastTokenForm
	f.mu.Unlock()
	if startForm["client_id"] != "test-client-id" || startForm["scope"] == "" || startForm["referrer"] != "grok-build" {
		t.Fatalf("start form wrong: %v", startForm)
	}
	if tokenForm["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || tokenForm["device_code"] != "DEV-123" {
		t.Fatalf("poll form wrong: %v", tokenForm)
	}

	stored, ok := store.Get("xai/main")
	if !ok || stored.AccessToken != "at-1" {
		t.Fatalf("token not stored: %+v ok=%v", stored, ok)
	}
}

func TestDeviceFlowExpiryAndDenial(t *testing.T) {
	t.Run("device code expires", func(t *testing.T) {
		f := &fake8628{outcomes: []pollOutcome{{status: 400, err: "authorization_pending"}}}
		srv := f.server()
		defer srv.Close()
		mgr := NewManager(NewTokenStore("memory"))
		mgr.SetCheckEvery(time.Hour)
		start := time.Now()
		_, err := mgr.Login(context.Background(), AccountSpec{Key: "xai/a", Provider: xaiLike(srv.URL)}, nil)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("want ErrExpired, got %v", err)
		}
		// The fake advertises expires_in=2s: the loop must stop shortly
		// after the device code lifetime, not keep polling forever.
		if time.Since(start) > 5*time.Second {
			t.Fatalf("expiry took %v", time.Since(start))
		}
	})
	t.Run("user denies", func(t *testing.T) {
		f := &fake8628{outcomes: []pollOutcome{{status: 400, err: "access_denied"}}}
		srv := f.server()
		defer srv.Close()
		mgr := NewManager(NewTokenStore("memory"))
		mgr.SetCheckEvery(time.Hour)
		_, err := mgr.Login(context.Background(), AccountSpec{Key: "xai/a", Provider: xaiLike(srv.URL)}, nil)
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("want ErrDenied, got %v", err)
		}
	})
	t.Run("server_error and 5xx keep polling", func(t *testing.T) {
		f := &fake8628{outcomes: []pollOutcome{
			{status: 400, err: "server_error"},
			{status: 500, err: "oops"},
			{},
		}}
		srv := f.server()
		defer srv.Close()
		mgr := NewManager(NewTokenStore("memory"))
		mgr.SetCheckEvery(time.Hour)
		tok, err := mgr.Login(context.Background(), AccountSpec{Key: "xai/a", Provider: xaiLike(srv.URL)}, nil)
		if err != nil || tok.AccessToken != "at-1" {
			t.Fatalf("login after transient errors: tok=%+v err=%v", tok, err)
		}
	})
	t.Run("unknown error aborts", func(t *testing.T) {
		f := &fake8628{outcomes: []pollOutcome{{status: 400, err: "invalid_client"}}}
		srv := f.server()
		defer srv.Close()
		mgr := NewManager(NewTokenStore("memory"))
		mgr.SetCheckEvery(time.Hour)
		_, err := mgr.Login(context.Background(), AccountSpec{Key: "xai/a", Provider: xaiLike(srv.URL)}, nil)
		if err == nil || errors.Is(err, ErrPending) {
			t.Fatalf("unknown error must abort, got %v", err)
		}
	})
	t.Run("context canceled", func(t *testing.T) {
		f := &fake8628{outcomes: []pollOutcome{{status: 400, err: "authorization_pending"}}}
		srv := f.server()
		defer srv.Close()
		mgr := NewManager(NewTokenStore("memory"))
		mgr.SetCheckEvery(time.Hour)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := mgr.Login(ctx, AccountSpec{Key: "xai/a", Provider: xaiLike(srv.URL)}, nil)
		if err == nil {
			t.Fatal("want ctx error")
		}
	})
}

func TestRefreshBeforeExpiry(t *testing.T) {
	f := &fake8628{outcomes: []pollOutcome{
		{status: 400, err: "authorization_pending"},
		{},
	}}
	srv := f.server()
	defer srv.Close()

	store := NewTokenStore("memory")
	mgr := NewManager(store)
	mgr.SetCheckEvery(10 * time.Millisecond)
	mgr.SetLead(30 * time.Second) // token expires in 2s → inside the lead
	spec := AccountSpec{Key: "xai/main", Provider: xaiLike(srv.URL)}
	if err := store.Put(spec.Key, Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	mgr.Sync([]AccountSpec{spec})
	defer mgr.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tok, _ := store.Get(spec.Key); tok.AccessToken == "at-1" {
			// Refreshed: the refresh grant used the stored refresh token
			// and rotation persisted the NEW one.
			f.mu.Lock()
			form := f.lastRefreshForm
			f.mu.Unlock()
			if form["refresh_token"] != "rt-old" {
				t.Fatalf("refresh used wrong token: %v", form)
			}
			stored, _ := store.Get(spec.Key)
			if stored.RefreshToken != "rt-1" {
				t.Fatalf("rotated refresh token not persisted: %+v", stored)
			}
			if time.Until(stored.ExpiresAt) < 30*time.Minute {
				t.Fatalf("expiry not extended: %+v", stored)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("token was not refreshed before expiry")
}

func TestRefreshNotDueWhenFarFromExpiry(t *testing.T) {
	f := &fake8628{outcomes: []pollOutcome{
		{status: 400, err: "authorization_pending"},
		{},
	}}
	srv := f.server()
	defer srv.Close()

	store := NewTokenStore("memory")
	mgr := NewManager(store)
	mgr.SetCheckEvery(5 * time.Millisecond)
	mgr.SetLead(30 * time.Second)
	spec := AccountSpec{Key: "xai/main", Provider: xaiLike(srv.URL)}
	if err := store.Put(spec.Key, Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().Add(2 * time.Hour), // far outside lead
	}); err != nil {
		t.Fatal(err)
	}
	mgr.Sync([]AccountSpec{spec})
	defer mgr.Stop()
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	n := f.refreshes
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("refreshed %d times though token far from expiry", n)
	}
}

func TestRefreshFailureCoolsAndKeepsOldToken(t *testing.T) {
	store := NewTokenStore("memory")
	spec := AccountSpec{Key: "xai/main", Provider: Provider{
		Name:     "xai",
		ClientID: "test-client-id",
		TokenURL: "http://127.0.0.1:1/token", // refused: port 1 closed
	}}
	if err := store.Put(spec.Key, Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store)
	mgr.SetCheckEvery(10 * time.Millisecond)
	mgr.SetLead(30 * time.Second)

	var cooled atomic.Int64
	mgr.Cooler = func(key string) {
		if key == spec.Key {
			cooled.Add(1)
		}
	}
	mgr.Sync([]AccountSpec{spec})
	defer mgr.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && cooled.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if cooled.Load() == 0 {
		t.Fatal("failed refresh never cooled the account")
	}
	// The stale token stays: better one wrong request than dropping the
	// account entirely while the loop keeps retrying.
	if tok, _ := store.Get(spec.Key); tok.AccessToken != "at-old" {
		t.Fatalf("old token clobbered: %+v", tok)
	}
}

func TestRefreshSkippedWithoutRefreshToken(t *testing.T) {
	// Kilo-style token: no refresh token, no expiry — the refresher must
	// never touch it.
	store := NewTokenStore("memory")
	spec := AccountSpec{Key: "kilocode/main", Provider: Provider{
		Name:     "kilocode",
		TokenURL: "http://127.0.0.1:1/token",
	}}
	if err := store.Put(spec.Key, Token{AccessToken: "kc-1"}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store)
	mgr.SetCheckEvery(5 * time.Millisecond)
	mgr.SetLead(time.Hour)
	var cooled atomic.Int64
	mgr.Cooler = func(string) { cooled.Add(1) }
	mgr.Sync([]AccountSpec{spec})
	defer mgr.Stop()
	time.Sleep(60 * time.Millisecond)
	if cooled.Load() != 0 {
		t.Fatal("refresh attempted without a refresh token")
	}
	if tok, _ := store.Get(spec.Key); tok.AccessToken != "kc-1" {
		t.Fatal("token changed without refresh")
	}
}

func TestSingleFlightPerAccount(t *testing.T) {
	var inflight, maxInflight, refreshes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if cur <= m || maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inflight.Add(-1)
		refreshes.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-new", "refresh_token": "rt-new", "expires_in": 3600})
	}))
	defer srv.Close()

	store := NewTokenStore("memory")
	spec := AccountSpec{Key: "xai/main", Provider: Provider{Name: "xai", ClientID: "c", TokenURL: srv.URL}}
	if err := store.Put(spec.Key, Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().Add(time.Second), // within lead
	}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store)
	mgr.SetLead(time.Hour)
	// Fire many concurrent refresh triggers; the single-flight guard must
	// collapse them to one in-flight HTTP refresh per account.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.maybeRefresh(context.Background(), spec, time.Hour)
		}()
	}
	wg.Wait()
	if maxInflight.Load() != 1 {
		t.Fatalf("concurrent refreshes observed: max=%d", maxInflight.Load())
	}
	if refreshes.Load() != 1 {
		t.Fatalf("want exactly 1 refresh, got %d", refreshes.Load())
	}
}

func TestSyncStartsStopsAndReplacesLoops(t *testing.T) {
	f := &fake8628{outcomes: []pollOutcome{{status: 400, err: "authorization_pending"}}}
	srv := f.server()
	defer srv.Close()
	store := NewTokenStore("memory")
	mgr := NewManager(store)
	mgr.SetCheckEvery(time.Hour)

	specA := AccountSpec{Key: "xai/a", Provider: xaiLike(srv.URL)}
	specB := AccountSpec{Key: "xai/b", Provider: xaiLike(srv.URL)}
	mgr.Sync([]AccountSpec{specA, specB})
	if len(mgr.stop) != 2 {
		t.Fatalf("want 2 loops, got %d", len(mgr.stop))
	}
	// Same specs → no restart churn.
	mgr.Sync([]AccountSpec{specA, specB})
	if len(mgr.stop) != 2 {
		t.Fatalf("resync churned loops: %d", len(mgr.stop))
	}
	// Removing B stops its loop.
	mgr.Sync([]AccountSpec{specA})
	if len(mgr.stop) != 1 {
		t.Fatalf("want 1 loop after removal, got %d", len(mgr.stop))
	}
	if _, ok := mgr.specs["xai/b"]; ok {
		t.Fatal("removed spec still tracked")
	}
	// Changed provider profile restarts the loop with the new one.
	changed := specA
	changed.Provider.TokenURL += "/v2"
	mgr.Sync([]AccountSpec{changed})
	if mgr.specs["xai/a"].Provider.TokenURL != srv.URL+"/token/v2" {
		t.Fatal("changed spec not adopted")
	}
	mgr.Stop()
	if len(mgr.stop) != 0 {
		t.Fatalf("Stop left loops running: %d", len(mgr.stop))
	}
	mgr.Sync([]AccountSpec{specA}) // no-op after Stop
	if len(mgr.stop) != 0 {
		t.Fatal("Sync after Stop restarted loops")
	}
}

func TestTokenStorePersistence(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir)
	if err := store.Put("xai/main", Token{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("kilocode/main", Token{AccessToken: "kc-1"}); err != nil {
		t.Fatal(err)
	}
	// Reopen: tokens survive.
	reopened := NewTokenStore(dir)
	tok, ok := reopened.Get("xai/main")
	if !ok || tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Fatalf("reopen lost token: %+v ok=%v", tok, ok)
	}
	keys := reopened.Keys()
	if len(keys) != 2 || keys[0] != "kilocode/main" || keys[1] != "xai/main" {
		t.Fatalf("keys wrong: %v", keys)
	}
	if err := reopened.Delete("xai/main"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("xai/main"); ok {
		t.Fatal("delete did not persist")
	}
	if _, ok := reopened.Get("kilocode/main"); !ok {
		t.Fatal("delete removed the wrong entry")
	}
	// Token file must not be world-readable: it holds bearer credentials.
	info, err := os.Stat(reopened.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file perms %v, want 0600", perm)
	}
}

func TestKiloDialectEndToEnd(t *testing.T) {
	var starts, polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/device-auth/codes", func(w http.ResponseWriter, r *http.Request) {
		starts.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":            "KILO-CODE-1",
			"verificationUrl": "https://kilo.example/auth/KILO-CODE-1",
			"expiresIn":       60,
		})
	})
	mux.HandleFunc("GET /api/device-auth/codes/KILO-CODE-1", func(w http.ResponseWriter, r *http.Request) {
		switch n := polls.Add(1); n {
		case 1:
			w.WriteHeader(http.StatusAccepted) // pending
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":    "approved",
				"token":     "kc-token-1",
				"userEmail": "user@example.com",
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	kilo, _ := Lookup("kilocode")
	kilo.TokenURL = srv.URL + "/api/device-auth/codes"

	store := NewTokenStore("memory")
	mgr := NewManager(store)
	mgr.SetCheckEvery(time.Hour)
	var prompt DeviceStart
	tok, err := mgr.Login(context.Background(), AccountSpec{Key: "kilocode/main", Provider: kilo}, func(ds DeviceStart) {
		prompt = ds
	})
	if err != nil {
		t.Fatalf("kilo login: %v", err)
	}
	if tok.AccessToken != "kc-token-1" || tok.RefreshToken != "" {
		t.Fatalf("kilo token wrong: %+v", tok)
	}
	if prompt.VerificationURL == "" || prompt.DeviceCode != "KILO-CODE-1" {
		t.Fatalf("kilo prompt wrong: %+v", prompt)
	}
	if starts.Load() != 1 || polls.Load() < 2 {
		t.Fatalf("flow shape wrong: starts=%d polls=%d", starts.Load(), polls.Load())
	}
}

func TestKiloDeniedAndExpired(t *testing.T) {
	var mode atomic.Value // "deny" | "expire"
	mode.Store("deny")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/device-auth/codes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "K2", "verificationUrl": "https://kilo.example/K2", "expiresIn": 60})
	})
	mux.HandleFunc("GET /api/device-auth/codes/K2", func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load().(string) {
		case "deny":
			w.WriteHeader(http.StatusForbidden)
		case "expire":
			w.WriteHeader(http.StatusGone)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	kilo, _ := Lookup("kilocode")
	kilo.TokenURL = srv.URL + "/api/device-auth/codes"
	mgr := NewManager(NewTokenStore("memory"))
	mgr.SetCheckEvery(time.Hour)

	mode.Store("expire")
	if _, err := mgr.Login(context.Background(), AccountSpec{Key: "kilocode/m", Provider: kilo}, nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	mode.Store("deny")
	if _, err := mgr.Login(context.Background(), AccountSpec{Key: "kilocode/m", Provider: kilo}, nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
}

func TestLookupAndPollerSelection(t *testing.T) {
	p := PollerFor(xaiLike("http://127.0.0.1:1"), nil)
	if _, ok := p.(rfc8628); !ok {
		t.Fatalf("expected rfc8628 poller, got %T", p)
	}
	k, ok := Lookup("kilocode")
	if !ok {
		t.Fatal("Lookup(kilocode) failed")
	}
	kp := PollerFor(k, nil)
	if _, ok := kp.(kiloPoller); !ok {
		t.Fatalf("expected kiloPoller, got %T", kp)
	}
	for _, name := range []string{"xai", "kilocode"} {
		if _, ok := Lookup(name); !ok {
			t.Fatalf("Lookup(%q) failed", name)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("Lookup accepted unknown provider")
	}
	if p, ok := Lookup("xai"); !ok || p.ClientID == "" || p.DeviceCodeURL == "" || p.Scope == "" {
		t.Fatalf("xai profile incomplete: %+v", p)
	}
	if fmt.Sprint(Providers()) != "[kilocode xai]" {
		t.Fatalf("Providers() = %v", Providers())
	}
}
