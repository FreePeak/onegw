package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"onegw/internal/config"
	"onegw/internal/owner"
	"onegw/internal/update"
)

// runVersion implements `onegw version`: the stamped release (or module
// version), the Go toolchain, and the git revision when built in-tree.
func runVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	fs.Parse(args)
	b := owner.Stamp()
	rev := ""
	if b.Revision != "" {
		rev = ", " + shortRev(b.Revision)
		if b.Modified {
			rev += "+dirty"
		}
	}
	mode := ""
	if update.InContainer() {
		mode = ", container"
	}
	fmt.Printf("onegw %s (%s%s%s)\n", update.Version(), b.GoVersion, rev, mode)
	return 0
}

func shortRev(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// runUpdate implements `onegw update`: check GitHub for the newest release
// and, unless --check, apply it. When a gateway is running (owner.json
// names a live pid) the update is applied BY that gateway through
// POST /admin/update — it swaps its own binary and hands the port to the
// new process with zero dropped requests. With no gateway running, the
// binary in hand is replaced in place and the next start runs the new
// version. Inside a container the command only checks and prints the
// host-side pull/recreate commands.
func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to onegw.toml (default ./onegw.toml, then $ONEGW_CONFIG)")
	check := fs.Bool("check", false, "report the newest release without applying")
	force := fs.Bool("force", false, "reinstall even when already on the latest release")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.Parse(args)

	cfg := loadConfigForCLI(*cfgPath)
	ctx := context.Background()

	inf, alive := runningGateway(cfg)
	if alive {
		return updateViaAdmin(ctx, cfg, inf, *check, *force, *yes)
	}

	opt := update.Opt{
		Repo:      cfg.Update.Repo,
		Base:      os.Getenv("ONEGW_RELEASE_API"),
		Force:     *force,
		CheckOnly: *check,
		// No gateway to hand off to: stage the binary only. A handoff
		// here would spawn a server from the CLI and signal the CLI's
		// own pid.
		NoHandoff: true,
	}
	if !*check && !confirm(*yes, "no gateway is running; replace this binary with the latest release?") {
		return 1
	}
	_, err := update.Run(ctx, opt)
	if err != nil {
		if err == update.ErrUpToDate {
			return 0
		}
		fmt.Fprintf(os.Stderr, "onegw update: %v\n", err)
		return 1
	}
	return 0
}

// updateViaAdmin drives the running gateway's own apply path so the
// handoff happens inside the serving process (correct exec path, argv,
// listen address, and the SIGTERM drain). Version judgments come from the
// GATEWAY's own status — never from this CLI binary's stamp, which may be
// anything.
func updateViaAdmin(ctx context.Context, cfg *config.Config, inf owner.Info, check, force, yes bool) int {
	pw := cfg.Server.AdminPassword
	url := adminURL(inf.Listen, "/admin/update")
	st, err := fetchStatus(ctx, url, pw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "onegw update: gateway at %s: %v\n", inf.Listen, err)
		return 1
	}
	if check {
		printStatus(st)
		return 0
	}
	if st.Latest == "" && st.Current != "" {
		// The gateway has not checked recently (interval off): ask IT to
		// check with its own repo/env, then re-read the status.
		if err := postCheck(ctx, url, pw); err != nil {
			fmt.Fprintf(os.Stderr, "onegw update: gateway check: %v\n", err)
			return 1
		}
		st, err = fetchStatus(ctx, url, pw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "onegw update: gateway at %s: %v\n", inf.Listen, err)
			return 1
		}
	}
	if st.Latest == "" || st.Current == "" {
		// Old gateway without the update endpoint (404 → empty status):
		// fall back to staging the binary at its on-disk path.
		return stageForOwner(cfg, inf, force, yes)
	}
	if !st.Outdated && !force {
		fmt.Printf("gateway pid %d is on %s; latest release %s — already up to date\n",
			inf.PID, st.Current, st.Latest)
		return 0
	}
	fmt.Printf("gateway pid %d serving %s on %s; latest release %s\n",
		inf.PID, st.Current, inf.Listen, st.Latest)
	if update.InContainer() {
		fmt.Println(update.ContainerGuidance(&update.Release{Tag: st.Latest}))
		return 0
	}
	if !confirm(yes, fmt.Sprintf("hand off pid %d to %s with zero dropped requests?", inf.PID, st.Latest)) {
		return 1
	}
	if err := postApply(ctx, url, pw, force); err != nil {
		fmt.Fprintf(os.Stderr, "onegw update: %v\n", err)
		return 1
	}
	fmt.Println("update started; waiting for the new process to take over...")
	return watchHandoff(ctx, url, pw, inf.PID)
}

// watchHandoff polls the admin endpoint until a DIFFERENT pid answers
// (the new process owns the port) or the old process disappears without
// a successor (rollback — the old gateway kept serving).
func watchHandoff(ctx context.Context, url, pw string, oldPID int) int {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	for {
		st, err := fetchStatus(ctx, url, pw)
		if err == nil && st.Pid != 0 && st.Pid != oldPID {
			fmt.Printf("updated to %s (new pid %d)\n", st.Current, st.Pid)
			return 0
		}
		if err == nil && st.LastApply != "" {
			fmt.Fprintf(os.Stderr, "onegw update: %s\n", st.LastApply)
			return 1
		}
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "onegw update: timed out waiting for the new process; the old gateway is still serving")
			return 1
		}
		time.Sleep(time.Second)
	}
}

// stageForOwner replaces the running gateway's binary on disk (safe: the
// live process holds its inode) when the gateway predates the update
// endpoint, and tells the operator to restart it. Force: the swap
// decision was already made from the gateway's own status above, so the
// CLI's own version stamp must not veto it.
func stageForOwner(cfg *config.Config, inf owner.Info, force, yes bool) int {
	if len(inf.Argv) == 0 || inf.Argv[0] == "" {
		fmt.Fprintln(os.Stderr, "onegw update: the running gateway predates the update endpoint and owner.json records no binary path; restart it once and re-run")
		return 1
	}
	exe := inf.Argv[0]
	if !confirm(yes, "the running gateway predates the update endpoint; replace its binary on disk (restart applies it)?") {
		return 1
	}
	_, err := update.Run(context.Background(), update.Opt{
		Repo: cfg.Update.Repo, Base: os.Getenv("ONEGW_RELEASE_API"),
		Force: true, ExecPath: exe, NoHandoff: true,
	})
	if err != nil && err != update.ErrUpToDate {
		fmt.Fprintf(os.Stderr, "onegw update: %v\n", err)
		return 1
	}
	fmt.Printf("restart the gateway (pid %d) to run the new binary\n", inf.PID)
	return 0
}

// runningGateway finds the serving instance via owner.json: a live pid
// that is not this process.
func runningGateway(cfg *config.Config) (owner.Info, bool) {
	inf, err := owner.Read(cfg.Server.DataDir)
	if err != nil || inf.PID <= 0 || inf.PID == os.Getpid() {
		return owner.Info{}, false
	}
	if err := syscall.Kill(inf.PID, 0); err != nil {
		return inf, false
	}
	return inf, true
}

// adminGet is the shared authenticated request for the admin endpoint.
func adminDo(ctx context.Context, method, url, pw string, body []byte) (*http.Response, error) {
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Admin-Password", pw)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return adminHTTP().Do(req)
}

// adminHTTP is a fresh client with keep-alives disabled: during a
// SO_REUSEPORT handoff a pooled connection stays pinned to whichever
// listener accepted it, and the CLI must be able to observe the NEW
// process answer.
func adminHTTP() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

func fetchStatus(ctx context.Context, url, pw string) (update.Status, error) {
	resp, err := adminDo(ctx, http.MethodGet, url, pw, nil)
	if err != nil {
		return update.Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return update.Status{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return update.Status{}, fmt.Errorf("HTTP %d (wrong admin password?)", resp.StatusCode)
	}
	var st update.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return update.Status{}, err
	}
	return st, nil
}

// postCheck asks the gateway for an immediate release check (empty POST).
func postCheck(ctx context.Context, url, pw string) error {
	resp, err := adminDo(ctx, http.MethodPost, url, pw, []byte("{}"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func postApply(ctx context.Context, url, pw string, force bool) error {
	body := map[string]any{"apply": true}
	if force {
		body["force"] = true
	}
	b, _ := json.Marshal(body)
	resp, err := adminDo(ctx, http.MethodPost, url, pw, b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway refused the update: HTTP %d", resp.StatusCode)
	}
	return nil
}

func printStatus(st update.Status) {
	fmt.Printf("current:  %s\n", st.Current)
	fmt.Printf("latest:   %s\n", orNone(st.Latest))
	fmt.Printf("outdated: %v\n", st.Outdated)
	if st.LastCheck != nil {
		fmt.Printf("checked:  %s\n", st.LastCheck.Format(time.RFC3339))
	}
	if st.LastError != "" {
		fmt.Printf("error:    %s\n", st.LastError)
	}
	if st.InContainer {
		fmt.Println("mode:     container (self-update disabled; update the image on the host)")
	}
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func confirm(yes bool, prompt string) bool {
	if yes {
		return true
	}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return true // non-interactive (agent/CI): proceed
	}
	if devNull, derr := os.Stat(os.DevNull); derr == nil && os.SameFile(fi, devNull) {
		return true // redirected /dev/null: scripted run
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var ans string
	fmt.Scanln(&ans)
	return ans == "y" || ans == "Y" || ans == "yes"
}

// adminURL builds an http URL for a listen address, normalizing wildcard
// hosts to loopback.
func adminURL(listen, path string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		host = "127.0.0.1"
	}
	if err != nil {
		port = strings.Trim(listen, ":[]*")
	}
	return "http://" + net.JoinHostPort(host, port) + path
}

// loadConfigForCLI mirrors the daemon's config resolution so `onegw
// update` reads the same admin password, data_dir, and [update] settings
// as the running gateway.
func loadConfigForCLI(flagPath string) *config.Config {
	path := flagPath
	if path == "" {
		if v := os.Getenv("ONEGW_CONFIG"); v != "" {
			path = v
		} else {
			path = "onegw.toml"
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) && flagPath == "" && os.Getenv("ONEGW_CONFIG") == "" {
			cfg = &config.Config{}
			cfg.Defaults()
			// The gateway booted the same way stores its generated password
			// under the data dir — probe with that, never generate one here.
			cfg.UseStoredAdminPassword()
		} else {
			fmt.Fprintf(os.Stderr, "onegw update: %v\n", err)
			os.Exit(1)
		}
	}
	return cfg
}
