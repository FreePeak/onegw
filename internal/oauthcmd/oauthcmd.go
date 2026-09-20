// Package oauthcmd implements the OAuth device-flow CLI (issue #2): log in,
// list, and force-refresh the subscription accounts whose tokens live in the
// gateway's data dir.
//
// It is reachable two ways and both must behave identically, so the code lives
// here rather than in a main:
//
//	onegw oauth login -provider xai -account you@example.com   # the gateway binary
//	onegw-oauth login -provider xai -account you@example.com   # standalone binary
//
// The second form is why this package exists: the container image ships the
// gateway binary, so `docker exec -it onegw onegw-oauth …` found no such
// executable, and `onegw oauth …` had no subcommand and silently started a
// SECOND gateway on the SO_REUSEPORT port instead of reporting the typo.
//
// -provider is the [[providers]] name from onegw.toml — that is the token
// store key (provider/account), the same key the running gateway resolves.
// -service selects the OAuth profile when it differs from the provider name
// (e.g. -provider grok -service xai).
package oauthcmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"onegw/internal/config"
	"onegw/internal/oauth"
)

// Run executes one CLI invocation and returns the process exit code:
// 0 ok, 1 the flow failed, 2 usage/argument error.
func Run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	switch args[0] {
	case "login":
		return cmdLogin(args[1:])
	case "list":
		return cmdList(args[1:])
	case "refresh":
		return cmdRefresh(args[1:])
	case "-h", "-help", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "onegw oauth: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `onegw oauth — OAuth device-flow login for onegw subscription providers

Usage:
  onegw oauth login   -provider <name> [-service <xai|kilocode>] [-account main]
                      [-data-dir DIR] [-config FILE]
                      [-device-url URL] [-token-url URL] [-client-id ID] [-scope S]
  onegw oauth list    [-data-dir DIR] [-config FILE]
  onegw oauth refresh -provider <name> [-service <xai>] [-account main] [-data-dir DIR]

The standalone binary speaks the same commands: onegw-oauth login …

Login opens the sign-in page in your default browser when the host has one
($BROWSER, else "open" on macOS / "xdg-open" on Linux). The URL + code are
always printed too, so headless and container hosts lose nothing.

-provider is the [[providers]] name from onegw.toml: the token is stored
under "provider/account", the exact key the running gateway resolves.

-data-dir defaults to the data dir the gateway itself would use — the config's
server.data_dir when a config file is found (-config, $ONEGW_CONFIG, or
./onegw.toml), then $ONEGW_DATA_DIR, then ~/.onegw. In the container that is
/data, so this needs no path at all:

  docker exec -it onegw onegw oauth login -provider xai -account <you>

The token is stored under <data-dir>/oauth-tokens.json. Point a provider
account at it in onegw.toml:

  [[providers]]
  name = "xai"
  kind = "openai"
  base_url = "https://api.x.ai"
    [[providers.accounts]]
    name = "main"

  [[oauth.accounts]]
  provider = "xai"
  account  = "main"
  service  = "xai"

The account name IS the join key: it must match the [[providers.accounts]] name
exactly, or the gateway has nothing to attach the token to.
`)
}

// opts holds the flags shared by login/refresh/list.
type opts struct {
	provider  string // [[providers]] name = token-store key (gateway-side)
	service   string // oauth service profile (defaults to provider)
	account   string
	dataDir   string
	cfgPath   string // config used to resolve the default data dir
	cfg       *config.Config
	deviceURL string // endpoint overrides for self-hosted IdPs / staging
	tokenURL  string
	clientID  string
	scope     string
}

func newOpts(name string, args []string) (*opts, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	o := &opts{}
	fs.StringVar(&o.provider, "provider", "", "provider name from onegw.toml [[providers]]")
	fs.StringVar(&o.service, "service", "", "oauth service profile: xai | kilocode (default: -provider)")
	fs.StringVar(&o.account, "account", "default", "account name matching [[providers.accounts]]")
	fs.StringVar(&o.dataDir, "data-dir", "", "onegw data dir (default: the config's server.data_dir, then $ONEGW_DATA_DIR, then ~/.onegw)")
	fs.StringVar(&o.cfgPath, "config", "", "onegw.toml used to resolve the default data dir (default ./onegw.toml, then $ONEGW_CONFIG)")
	fs.StringVar(&o.deviceURL, "device-url", "", "override the service device-code URL")
	fs.StringVar(&o.tokenURL, "token-url", "", "override the service token URL")
	fs.StringVar(&o.clientID, "client-id", "", "override the OAuth client id")
	fs.StringVar(&o.scope, "scope", "", "override the requested scope")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	o.cfg = loadConfig(o.cfgPath)
	o.dataDir = resolveDataDir(o.dataDir, o.cfgPath)
	return o, nil
}

// loadConfig reads the config the way resolveDataDir does, returning nil when
// there is none or it does not load (the data-dir resolver reports the error
// once; a missing config is normal for the standalone CLI).
func loadConfig(cfgPath string) *config.Config {
	cfg, err := config.Load(configPath(cfgPath))
	if err != nil {
		return nil
	}
	return cfg
}

// configEntry returns the [[oauth.accounts]] entry for this provider/account,
// if the config declares one.
func (o *opts) configEntry() (config.OAuthAccount, bool) {
	if o.cfg == nil || o.provider == "" {
		return config.OAuthAccount{}, false
	}
	for _, a := range o.cfg.OAuthAccounts() {
		if a.Provider == o.provider && a.Account == o.account {
			return a, true
		}
	}
	return config.OAuthAccount{}, false
}

// resolveDataDir answers "which directory does the GATEWAY read its tokens
// from?", because that — not a CLI default — is where a login has to land. The
// precedence mirrors config.Load: an explicit -data-dir, then the config file's
// server.data_dir (itself defaulting to $ONEGW_DATA_DIR, then ~/.onegw).
//
// The container is the case that matters: the image sets ONEGW_DATA_DIR=/data
// and mounts the volume there, so `docker exec -it onegw onegw oauth login …`
// with no flags must write /data/oauth-tokens.json.
func resolveDataDir(flagDir, cfgPath string) string {
	if d := strings.TrimSpace(flagDir); d != "" {
		return d
	}
	if cfg, err := config.Load(configPath(cfgPath)); err == nil && cfg.Server.DataDir != "" {
		return cfg.Server.DataDir
	} else if err != nil && configExists(configPath(cfgPath)) {
		fmt.Fprintf(os.Stderr, "onegw oauth: ignoring %s (%v)\n", configPath(cfgPath), err)
	}
	if v := os.Getenv("ONEGW_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".onegw"
	}
	return home + "/.onegw"
}

// configPath is where the config lives: the explicit flag, $ONEGW_CONFIG, or
// ./onegw.toml — the same order the gateway itself resolves.
func configPath(flagPath string) string {
	if p := strings.TrimSpace(flagPath); p != "" {
		return p
	}
	if v := os.Getenv("ONEGW_CONFIG"); v != "" {
		return v
	}
	return "onegw.toml"
}

func configExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// key is the token-store key: identical on the CLI and gateway sides
// (provider/account, gateway's provider name).
func (o *opts) key() (string, error) {
	if o.provider == "" {
		return "", fmt.Errorf("-provider is required")
	}
	return o.provider + "/" + o.account, nil
}

// profile resolves the OAuth service profile: the service profile itself, then
// the config entry's endpoint overrides, then the explicit flags — the same
// order the dashboard's sign-in uses (server.spec). Without the config step a
// mounted config that redirects the flow (self-hosted IdP, staging, tests) was
// silently ignored: the CLI still hit the vendor, which is exactly the kind of
// surprise that turns a test into a real login.
func (o *opts) profile() (oauth.Provider, error) {
	entry, haveEntry := o.configEntry()
	svc := o.service
	if svc == "" && haveEntry {
		svc = entry.Service
	}
	if svc == "" {
		svc = o.provider
	}
	p, ok := oauth.Lookup(svc)
	if !ok {
		return oauth.Provider{}, fmt.Errorf("unknown oauth service %q (known: %v)", svc, oauth.Providers())
	}
	if haveEntry {
		if entry.DeviceURL != "" {
			p.DeviceCodeURL = entry.DeviceURL
		}
		if entry.TokenURL != "" {
			p.TokenURL = entry.TokenURL
		}
		if entry.ClientID != "" {
			p.ClientID = entry.ClientID
		}
		if entry.Scope != "" {
			p.Scope = entry.Scope
		}
	}
	if o.deviceURL != "" {
		p.DeviceCodeURL = o.deviceURL
	}
	if o.tokenURL != "" {
		p.TokenURL = o.tokenURL
	}
	if o.clientID != "" {
		p.ClientID = o.clientID
	}
	if o.scope != "" {
		p.Scope = o.scope
	}
	return p, nil
}

// openBrowser launches the operator's default browser at url so a re-auth
// needs one glance, not a transcription. $BROWSER wins when set (an explicit
// choice, and the test hook); otherwise macOS uses `open` and Linux
// `xdg-open`. Any failure is returned for the caller to print over —
// headless hosts and the container have no opener, and the URL stays on
// stdout regardless, so the login never depends on this working.
func openBrowser(url string) error {
	name := os.Getenv("BROWSER")
	if name == "" {
		switch runtime.GOOS {
		case "darwin":
			name = "open"
		case "linux":
			name = "xdg-open"
		default:
			return fmt.Errorf("no browser opener for %s", runtime.GOOS)
		}
	}
	return exec.Command(name, url).Run()
}

func cmdLogin(args []string) int {
	o, err := newOpts("login", args)
	if err != nil {
		return 2
	}
	key, err := o.key()
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw oauth login: %v\n", err)
		return 2
	}
	p, err := o.profile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw oauth login: %v\n", err)
		return 2
	}

	mgr := oauth.NewManager(oauth.NewTokenStore(o.dataDir))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Print the resolved store up front: the gateway reads
	// <config data_dir>/oauth-tokens.json, so a config that overrides
	// data_dir needs a matching -data-dir. Seeing the path before approving
	// beats a login that "succeeds" into a file the gateway never reads.
	// Name the destination explicitly: a login that reaches the real vendor
	// instead of the stub/local IdP the config names is the one mistake that
	// cannot be undone (xAI keeps a single active device session), and the
	// endpoint is the only thing that distinguishes them before approval.
	fmt.Printf("Signing in to %s (store: %s/oauth-tokens.json, device: %s).\nOpen this URL and enter the code:\n\n",
		p.Name, o.dataDir, p.DeviceCodeURL)
	tok, err := mgr.Login(ctx, oauth.AccountSpec{Key: key, Provider: p}, func(ds oauth.DeviceStart) {
		url := ds.VerificationURLComplete
		if url == "" {
			url = ds.VerificationURL
		}
		if ds.UserCode != "" && ds.UserCode != ds.DeviceCode {
			fmt.Printf("  %s\n  code: %s\n\n", url, ds.UserCode)
		} else {
			// Kilo dialect: the URL carries the code; no typing needed.
			fmt.Printf("  %s\n\n", url)
		}
		// A re-login on a workstation should open a page, not assign a
		// copy-paste chore; a failure here is noise, never fatal (see openBrowser).
		if url != "" {
			if err := openBrowser(url); err != nil {
				fmt.Printf("Could not open a browser (%v) — open the URL above yourself.\n", err)
			}
		}
		fmt.Println("Waiting for authorization (Ctrl-C to cancel)…")
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw oauth: login failed: %v\n", err)
		return 1
	}
	expiry := "no expiry reported"
	if !tok.ExpiresAt.IsZero() {
		expiry = "expires " + tok.ExpiresAt.Local().Format(time.RFC3339)
	}
	refresh := "no"
	if tok.RefreshToken != "" {
		refresh = "yes (auto-refresh enabled)"
	}
	fmt.Printf("Stored token for %s in %s (%s; refresh: %s)\n",
		key, o.dataDir+"/oauth-tokens.json", expiry, refresh)
	if tok.RefreshToken == "" {
		fmt.Println("This service does not issue refresh tokens — re-run login when it expires.")
	}
	return 0
}

func cmdList(args []string) int {
	o, err := newOpts("list", args)
	if err != nil {
		return 2
	}
	mgr := oauth.NewManager(oauth.NewTokenStore(o.dataDir))
	keys := mgr.Store().Keys()
	if len(keys) == 0 {
		fmt.Printf("no oauth tokens stored in %s/oauth-tokens.json\n", o.dataDir)
		return 0
	}
	for _, k := range keys {
		tok, ok := mgr.Store().Get(k)
		if !ok {
			continue
		}
		state := "valid"
		if !tok.ExpiresAt.IsZero() {
			switch d := time.Until(tok.ExpiresAt); {
			case d <= 0:
				state = "EXPIRED"
			case d < time.Hour:
				state = "expires soon"
			}
		}
		if tok.RefreshToken == "" && !tok.ExpiresAt.IsZero() && time.Until(tok.ExpiresAt) <= 0 {
			state += " (re-login required)"
		}
		fmt.Printf("%-30s %s\n", k, state)
	}
	return 0
}

func cmdRefresh(args []string) int {
	o, err := newOpts("refresh", args)
	if err != nil {
		return 2
	}
	key, err := o.key()
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw oauth refresh: %v\n", err)
		return 2
	}
	p, err := o.profile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw oauth refresh: %v\n", err)
		return 2
	}
	mgr := oauth.NewManager(oauth.NewTokenStore(o.dataDir))
	mgr.Sync([]oauth.AccountSpec{{Key: key, Provider: p}})
	defer mgr.Stop()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mgr.RefreshNow(ctx, key); err != nil {
		fmt.Fprintf(os.Stderr, "onegw oauth: refresh failed: %v\n", err)
		return 1
	}
	fmt.Printf("Refreshed token for %s\n", key)
	return 0
}
