//go:build unix

package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// ErrUpToDate means the running version is already the newest release.
var ErrUpToDate = errors.New("onegw is up to date")

// Spawned is a detached child gateway. Exit receives the wait error
// exactly once (buffered), so the updater can tell "still starting" from
// "already dead" — a fast crash must fail the update before the old
// gateway is drained, never after.
type Spawned struct {
	Pid  int
	Exit <-chan error
	p    *os.Process
}

// Kill terminates the spawned process (rollback path).
func (s *Spawned) Kill() error {
	if s == nil || s.p == nil {
		return nil
	}
	return s.p.Kill()
}

// Opt configures one update run. Zero values select the sane defaults for
// a self-update of the current process.
type Opt struct {
	Repo           string        // owner/name; default DefaultRepo
	Base           string        // API base override (tests / mirrors)
	Force          bool          // reinstall even when versions match
	CheckOnly      bool          // report, never touch the disk
	AdminPassword  string        // /admin/update probe credential for the handoff poll
	Current        string        // version to compare against; default this binary's
	Out            io.Writer     // progress log; default stderr
	HandoffTimeout time.Duration // takeover wait; default 60s

	// Self-update context. When Apply runs inside the gateway, these stay
	// zero and are derived from the process. When a CLI updates a gateway
	// that is running elsewhere, the caller fills them from owner.json.
	ExecPath  string   // binary to replace (default: this executable)
	Argv      []string // argv of the running gateway (default: this argv)
	OldPID    int      // pid to drain after handoff (default: this pid)
	Listen    string   // address the gateway serves (probe target)
	NoHandoff bool     // stage the binary only; never spawn or signal

	// Test seams.
	Spawn    func(path string, argv []string, env []string) (*Spawned, error)
	ProbeURL func(listen string) string
	Signal   func(pid int) error // drain signal; default SIGTERM
}

// DefaultRepo is the GitHub repository release CI publishes.
const DefaultRepo = "FreePeak/onegw"

// Run performs one update attempt: check, then (download + swap +
// handoff). It returns the release it acted on, or ErrUpToDate / a
// failure. The old gateway keeps serving until the new process proves —
// through an authenticated /admin/update answer naming its own pid — that
// it is the one bound to the port; only then is the old pid drained.
func Run(ctx context.Context, opt Opt) (*Release, error) {
	out := opt.Out
	if out == nil {
		out = os.Stderr
	}
	repo := opt.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	cur := opt.Current
	if cur == "" {
		cur = Version()
	}
	client := &Client{Base: opt.Base, Repo: repo}
	rel, err := client.Latest(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "onegw %s: latest release %s (%s)\n", cur, rel.Tag, repo)
	switch {
	case Compare(rel.Tag, cur) <= 0 && !opt.Force:
		fmt.Fprintln(out, "already up to date")
		return rel, ErrUpToDate
	case InContainer():
		// The filesystem belongs to the image; never self-apply here.
		fmt.Fprintln(out, ContainerGuidance(rel))
		return rel, nil
	case opt.CheckOnly:
		fmt.Fprintln(out, "update available: run `onegw update` to apply")
		return rel, nil
	}

	execPath := opt.ExecPath
	if execPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("update: cannot resolve own binary path: %w", err)
		}
		execPath = exe
	}
	asset, err := rel.SelectAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "downloading %s (%s)\n", asset.Name, fmtBytes(asset.Size))
	tmp := filepath.Join(filepath.Dir(execPath), ".onegw.new")
	if err := asset.Download(ctx, tmp); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := smokeRun(tmp); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	fmt.Fprintln(out, "download verified")

	if opt.NoHandoff {
		if err := swap(tmp, execPath); err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "staged %s; restart onegw to run %s\n", execPath, rel.Tag)
		return rel, nil
	}

	listen := opt.Listen
	if listen == "" {
		listen = os.Getenv("ONEGW_LISTEN")
	}
	if listen == "" {
		return nil, fmt.Errorf("update: cannot determine the listen address to hand off (set [server] listen or ONEGW_LISTEN)")
	}
	if opt.AdminPassword == "" {
		return nil, fmt.Errorf("update: refusing the handoff without an admin password to verify the new process with; binary left unstaged")
	}
	argv := opt.Argv
	if len(argv) == 0 {
		argv = os.Args
	}
	oldPID := opt.OldPID
	if oldPID == 0 {
		oldPID = os.Getpid()
	}
	spawn := opt.Spawn
	if spawn == nil {
		spawn = spawnDetached
	}
	probeURL := opt.ProbeURL
	if probeURL == nil {
		probeURL = defaultProbeURL
	}
	handoffTimeout := 60 * time.Second
	if opt.HandoffTimeout > 0 {
		handoffTimeout = opt.HandoffTimeout
	}

	// Swap the binary on disk, then start the new build. The old process
	// keeps serving until the new one proves it owns the port (SO_REUSEPORT
	// overlap), so a failed start rolls back with zero downtime.
	if err := swap(tmp, execPath); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	backup := execPath + ".old"
	spawned, err := spawn(execPath, append([]string{execPath}, argv[1:]...), os.Environ())
	if err != nil {
		_ = os.Remove(execPath)
		_ = os.Rename(backup, execPath)
		return nil, fmt.Errorf("update: start %s: %w", rel.Tag, err)
	}
	fmt.Fprintf(out, "started %s (pid %d), waiting for it to take over %s\n", rel.Tag, spawned.Pid, listen)
	if !waitServing(ctx, spawned, probeURL(listen), opt.AdminPassword, handoffTimeout) {
		_ = spawned.Kill()
		_ = os.Rename(execPath, execPath+".bad")
		_ = os.Rename(backup, execPath)
		return nil, fmt.Errorf("update: new binary never took over %s; rolled back to %s (old gateway still running)", listen, cur)
	}
	signal := opt.Signal
	if signal == nil {
		signal = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
	}
	if err := signal(oldPID); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintf(out, "update: signal old pid %d: %v (new gateway is serving; drain it manually)\n", oldPID, err)
	} else {
		fmt.Fprintf(out, "drained old pid %d\n", oldPID)
	}
	fmt.Fprintf(out, "updated to %s\n", rel.Tag)
	return rel, nil
}

// swap moves the freshly downloaded binary onto the live path, keeping the
// previous build at <path>.old for rollback. Renames within one directory
// are atomic; a running binary can be renamed on Linux and macOS (the
// process holds the inode), so the swap never disturbs the serving
// process.
func swap(newPath, execPath string) error {
	backup := execPath + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(execPath, backup); err != nil {
		return fmt.Errorf("update: cannot move %s aside (%w) — is the directory writable by this user?", execPath, err)
	}
	if err := os.Rename(newPath, execPath); err != nil {
		_ = os.Rename(backup, execPath) // restore the serving binary
		return fmt.Errorf("update: cannot install %s: %w", execPath, err)
	}
	return nil
}

// smokeRun executes the downloaded binary's version subcommand: it must
// start, print a version, and exit 0. A corrupt or wrong-platform
// download fails here, before anything on disk moves.
func smokeRun(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("update: downloaded binary failed its smoke run: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultProbeURL maps a listen address to the /admin/update probe URL.
func defaultProbeURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		host = "127.0.0.1"
	}
	if err != nil { // no port — nothing sensible to probe
		return ""
	}
	return fmt.Sprintf("http://%s:%s/admin/update", host, port)
}

// waitServing polls until the spawned process proves it is serving:
// /admin/update must answer 200 WITH the spawned pid in the body. Any
// other answer — 404 from an old gateway still holding the port, 200
// naming the OLD pid during the SO_REUSEPORT overlap, connection refused
// while the new mux boots — is not evidence and keeps the loop running.
// If the spawned process exits first, the loop fails immediately.
func waitServing(ctx context.Context, spawned *Spawned, url, password string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hc := &http.Client{Timeout: 2 * time.Second, Transport: probeTransport()}
	for {
		// Fail fast on a dead child: never sit out the full timeout.
		select {
		case <-spawned.Exit:
			return false
		case <-ctx.Done():
			return false
		default:
		}
		if probeServing(hc, url, password, spawned.Pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-spawned.Exit:
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func probeServing(hc *http.Client, url, password string, wantPid int) bool {
	if url == "" {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Admin-Password", password)
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Pid int `json:"pid"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return false
	}
	return body.Pid == wantPid
}

// probeTransport disables keep-alive: a pooled connection stays pinned
// to whichever SO_REUSEPORT listener accepted it first, which would make
// every probe answer with the OLD gateway's pid forever. A fresh
// connection per probe reaches the newest binder — the process we are
// actually waiting for.
func probeTransport() *http.Transport {
	return &http.Transport{DisableKeepAlives: true}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
