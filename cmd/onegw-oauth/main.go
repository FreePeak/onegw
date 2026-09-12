// Command onegw-oauth performs OAuth device-flow logins for onegw's
// subscription providers (issue #2): it prints the verification URL + user
// code, polls until the user authorizes, and stores the token in the
// gateway's data dir where the running onegw picks it up (no config edit,
// no restart needed — the provider account just references the service).
//
// Usage:
//
//	onegw-oauth login   -provider xai [-account main] [-data-dir ~/.onegw]
//	onegw-oauth list    [-data-dir ~/.onegw]
//	onegw-oauth refresh -provider xai [-account main] [-data-dir ~/.onegw]
//
// -provider is the [[providers]] name from onegw.toml — that is the token
// store key (provider/account), the same name the gateway resolves.
// -service selects the OAuth profile when it differs from the provider
// name (e.g. -provider grok -service xai).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"onegw/internal/oauth"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		usage()
		return 2
	}
	switch os.Args[1] {
	case "login":
		return cmdLogin(os.Args[2:])
	case "list":
		return cmdList(os.Args[2:])
	case "refresh":
		return cmdRefresh(os.Args[2:])
	case "-h", "-help", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "onegw-oauth: unknown command %q\n\n", os.Args[1])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `onegw-oauth — OAuth device-flow login for onegw

Usage:
  onegw-oauth login   -provider <name> [-service <xai|kilocode>] [-account main]
                      [-data-dir DIR] [-device-url URL] [-token-url URL]
                      [-client-id ID] [-scope S]
  onegw-oauth list    [-data-dir DIR]
  onegw-oauth refresh -provider <name> [-service <xai>] [-account main] [-data-dir DIR]

-provider is the [[providers]] name from onegw.toml: the token is stored
under "provider/account", the exact key the running gateway resolves.

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
`)
}

// opts holds the flags shared by login/refresh/list.
type opts struct {
	provider  string // [[providers]] name = token-store key (gateway-side)
	service   string // oauth service profile (defaults to provider)
	account   string
	dataDir   string
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
	fs.StringVar(&o.dataDir, "data-dir", dataDirDefault(), "onegw data dir (token store location)")
	fs.StringVar(&o.deviceURL, "device-url", "", "override the service device-code URL")
	fs.StringVar(&o.tokenURL, "token-url", "", "override the service token URL")
	fs.StringVar(&o.clientID, "client-id", "", "override the OAuth client id")
	fs.StringVar(&o.scope, "scope", "", "override the requested scope")
	return o, fs.Parse(args)
}

// key is the token-store key: identical on the CLI and gateway sides
// (provider/account, gateway's provider name).
func (o *opts) key() (string, error) {
	if o.provider == "" {
		return "", fmt.Errorf("-provider is required")
	}
	return o.provider + "/" + o.account, nil
}

// profile resolves the OAuth service profile with the endpoint overrides
// applied. The service defaults to the provider name.
func (o *opts) profile() (oauth.Provider, error) {
	svc := o.service
	if svc == "" {
		svc = o.provider
	}
	p, ok := oauth.Lookup(svc)
	if !ok {
		return oauth.Provider{}, fmt.Errorf("unknown oauth service %q (known: %v)", svc, oauth.Providers())
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

func dataDirDefault() string {
	if v := os.Getenv("ONEGW_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".onegw"
	}
	return home + "/.onegw"
}

func cmdLogin(args []string) int {
	o, err := newOpts("login", args)
	if err != nil {
		return 2
	}
	key, err := o.key()
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw-oauth login: %v\n", err)
		return 2
	}
	p, err := o.profile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw-oauth login: %v\n", err)
		return 2
	}

	mgr := oauth.NewManager(oauth.NewTokenStore(o.dataDir))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Print the resolved store up front: the gateway reads
	// <config data_dir>/oauth-tokens.json, so a config that overrides
	// data_dir needs a matching -data-dir. Seeing the path before approving
	// beats a login that "succeeds" into a file the gateway never reads.
	fmt.Printf("Signing in to %s (store: %s/oauth-tokens.json).\nOpen this URL and enter the code:\n\n", p.Name, o.dataDir)
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
		fmt.Println("Waiting for authorization (Ctrl-C to cancel)…")
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw-oauth: login failed: %v\n", err)
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
		fmt.Println("no oauth tokens stored")
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
		fmt.Fprintf(os.Stderr, "onegw-oauth refresh: %v\n", err)
		return 2
	}
	p, err := o.profile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw-oauth refresh: %v\n", err)
		return 2
	}
	mgr := oauth.NewManager(oauth.NewTokenStore(o.dataDir))
	mgr.Sync([]oauth.AccountSpec{{Key: key, Provider: p}})
	defer mgr.Stop()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mgr.RefreshNow(ctx, key); err != nil {
		fmt.Fprintf(os.Stderr, "onegw-oauth: refresh failed: %v\n", err)
		return 1
	}
	fmt.Printf("Refreshed token for %s\n", key)
	return 0
}
