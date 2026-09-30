package oauthcmd

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"onegw/internal/oauth"
)

// The container is the case that matters: the image sets ONEGW_DATA_DIR=/data
// and mounts the volume there, while the config it ships says data_dir="/data".
// A login that lands anywhere else writes a token the gateway never reads, so
// the resolution order is pinned here.
func TestResolveDataDirPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\ndata_dir = \""+filepath.Join(dir, "from-config")+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("explicit flag wins", func(t *testing.T) {
		t.Setenv("ONEGW_DATA_DIR", filepath.Join(dir, "from-env"))
		if got := resolveDataDir(filepath.Join(dir, "flag"), cfgPath); got != filepath.Join(dir, "flag") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("config wins over the env override", func(t *testing.T) {
		// config.Load resolves server.data_dir itself (config, then env, then
		// ~/.onegw), so the CLI must ask it rather than re-implement the order:
		// an explicit data_dir in the file is what the gateway actually uses.
		t.Setenv("ONEGW_DATA_DIR", filepath.Join(dir, "from-env"))
		if got := resolveDataDir("", cfgPath); got != filepath.Join(dir, "from-config") {
			t.Fatalf("got %q, want the config's data_dir", got)
		}
	})

	t.Run("env when there is no config", func(t *testing.T) {
		t.Setenv("ONEGW_DATA_DIR", filepath.Join(dir, "from-env"))
		missing := filepath.Join(dir, "absent.toml")
		if got := resolveDataDir("", missing); got != filepath.Join(dir, "from-env") {
			t.Fatalf("got %q, want the env value", got)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("ONEGW_DATA_DIR", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home dir")
		}
		if got := resolveDataDir("", filepath.Join(dir, "absent.toml")); got != home+"/.onegw" {
			t.Fatalf("got %q, want ~/.onegw", got)
		}
	})
}

// The re-auth promise: `onegw oauth login` pops the sign-in page. $BROWSER
// must be the opener and receive the (complete) URL verbatim — that is both
// the user's explicit-choice override and the only writable probe on a CI
// box. A broken $BROWSER must surface as an error (the CLI prints the URL
// fallback then), never a hang or a swallowed failure.
func TestOpenBrowserHonoursBROWSER(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stub")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "opened")
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$1\" > \""+marker+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", stub)

	const url = "https://accounts.x.ai/oauth2/device?user_code=DFEV-A6SX"
	if err := openBrowser(url); err != nil {
		t.Fatalf("openBrowser: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("browser was never opened: %v", err)
	}
	if strings.TrimSpace(string(got)) != url {
		t.Fatalf("browser opened %q, want %q", got, url)
	}

	t.Setenv("BROWSER", filepath.Join(dir, "no-such-opener"))
	if err := openBrowser(url); err == nil {
		t.Fatal("missing opener must error, not claim success")
	}
}

// A missing subcommand must not silently do something else.
func TestRunRejectsUnknownCommand(t *testing.T) {
	if code := Run([]string{"logni"}); code != 2 {
		t.Fatalf("typo exit code = %d, want 2", code)
	}
	if code := Run(nil); code != 2 {
		t.Fatalf("no command exit code = %d, want 2", code)
	}
	if code := Run([]string{"help"}); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
}

// A mounted config that redirects the device flow (self-hosted IdP, staging,
// tests) must be honoured by the CLI exactly as it is by the dashboard's
// sign-in — otherwise `onegw oauth login` silently reaches the real vendor
// while the config says otherwise, and a "test" login rotates the operator's
// live session.
func TestProfileHonoursConfigEntryOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "onegw.toml")
	cfg := `[server]
data_dir = "` + dir + `"

[[providers]]
name = "grokbuild2"
kind = "openai"
base_url = "http://up.invalid"
models = ["m1"]

[[providers.accounts]]
name = "main"

[[oauth.accounts]]
provider   = "grokbuild2"
account    = "main"
service    = "xai"
device_url = "http://127.0.0.1:9/device"
token_url  = "http://127.0.0.1:9/token"
client_id  = "from-config"
scope      = "api:access"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	o, err := newOpts("login", []string{"-provider", "grokbuild2", "-account", "main", "-config", cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	p, err := o.profile()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "xai" {
		t.Fatalf("profile = %q, want the entry's service profile", p.Name)
	}
	if p.DeviceCodeURL != "http://127.0.0.1:9/device" || p.TokenURL != "http://127.0.0.1:9/token" {
		t.Fatalf("config endpoints ignored: %s / %s", p.DeviceCodeURL, p.TokenURL)
	}
	if p.ClientID != "from-config" || p.Scope != "api:access" {
		t.Fatalf("config client/scope ignored: %q %q", p.ClientID, p.Scope)
	}
	if o.dataDir != dir {
		t.Fatalf("data dir = %q, want the config's", o.dataDir)
	}

	// Explicit flags still win: staging overrides without editing the file.
	o2, err := newOpts("login", []string{"-provider", "grokbuild2", "-account", "main",
		"-config", cfgPath, "-device-url", "http://flag.invalid/device", "-service", "kilocode"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := o2.profile()
	if err != nil {
		t.Fatal(err)
	}
	if p2.Name != "kilocode" || p2.DeviceCodeURL != "http://flag.invalid/device" {
		t.Fatalf("-service/-device-url must win: %q %s", p2.Name, p2.DeviceCodeURL)
	}
}

// `onegw oauth login -provider codex` must take the BROWSER path, not the
// device path: ChatGPT has no device grant, so a device attempt is a dead
// login. Prove the whole CLI round trip against a stub token endpoint:
// authorize URL opened, loopback callback accepted, token stored under the
// account key the gateway resolves.
func TestBrowserLoginStoresCodexToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stub")
	}
	var gotForm url.Values
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"at-codex","refresh_token":"rt-codex","expires_in":3600}`))
	}))
	defer idp.Close()

	// A free loopback port the stub profile can register: browserLogin binds
	// the profile's own port, which for a real codex login is 1455.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()

	dir := t.TempDir()
	marker := filepath.Join(dir, "opened")
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$1\" > \""+marker+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", stub)

	p := oauth.Provider{
		Name: "codex", AuthURL: idp.URL + "/oauth/authorize", TokenURL: idp.URL,
		ClientID: "app_test", Scope: "openid offline_access", CallbackPort: port,
		CallbackPath: "/auth/callback", Nonce: true,
	}
	mgr := oauth.NewManager(oauth.NewTokenStore(dir))
	defer mgr.Stop()
	done := make(chan int, 1)
	go func() { done <- browserLogin(t.Context(), mgr, &opts{dataDir: dir}, p, "codex/me") }()

	// The opener records the authorize URL; replay its `state` back to the
	// loopback listener exactly as the vendor's redirect would.
	var authURL string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
			authURL = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if authURL == "" {
		t.Fatal("the CLI never opened the authorize URL")
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorize url %q: %v", authURL, err)
	}
	if u.Query().Get("code_challenge") == "" || u.Query().Get("redirect_uri") == "" {
		t.Fatalf("authorize url is not a PKCE challenge: %s", authURL)
	}
	cb := "http://127.0.0.1:" + strconv.Itoa(port) + "/auth/callback?code=CODE-1&state=" +
		url.QueryEscape(u.Query().Get("state"))
	resp, err := http.Get(cb) //nolint:gosec // loopback stub
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("login exit code = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("login did not finish after the callback")
	}
	tok, ok := mgr.Store().Get("codex/me")
	if !ok || tok.AccessToken != "at-codex" {
		t.Fatalf("token not stored under the account key: %+v", tok)
	}
	if gotForm.Get("grant_type") != "authorization_code" || gotForm.Get("code") != "CODE-1" {
		t.Fatalf("exchange form = %v", gotForm)
	}
	if gotForm.Get("code_verifier") == "" {
		t.Error("exchange carried no PKCE verifier")
	}
}
