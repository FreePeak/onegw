// Command onegw is the LLM gateway binary: one process fronting OpenAI,
// Anthropic, and Gemini-compatible surfaces with translation, token saving,
// fallback routing, and usage tracking — inside a 100 MB RAM envelope.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"onegw/internal/config"
	"onegw/internal/oauthcmd"
	"onegw/internal/server"
	"onegw/internal/update"
)

func main() {
	if len(os.Args) > 1 {
		switch arg := os.Args[1]; arg {
		case "version":
			os.Exit(runVersion(os.Args[2:]))
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "oauth":
			os.Exit(oauthcmd.Run(os.Args[2:]))
		case "-h", "--help", "help":
			usage(os.Stdout)
			os.Exit(0)
		default:
			if classifyArg(arg) == argUnknown {
				// An unknown bare word used to fall through to runGateway: a
				// typo — or the `onegw oauth login …` form before this
				// subcommand existed — started a SECOND gateway process, which
				// SO_REUSEPORT happily binds on the same port as the live one
				// (observed in the container, 2026-09-13). Fail loudly instead.
				fmt.Fprintf(os.Stderr, "onegw: unknown command %q\n\n", arg)
				usage(os.Stderr)
				os.Exit(2)
			}
		}
	}
	runGateway()
}

// argKind classifies os.Args[1] so the dispatcher can tell "start the gateway"
// (flags) from a subcommand from a typo.
type argKind int

const (
	argFlag    argKind = iota // starts with '-': the gateway's own flags
	argCommand                // a known subcommand
	argUnknown                // a bare word that is not a subcommand
)

func classifyArg(arg string) argKind {
	switch arg {
	case "version", "update", "oauth", "help", "-h", "--help":
		return argCommand
	}
	if strings.HasPrefix(arg, "-") {
		return argFlag
	}
	return argUnknown
}

// usage is the one place the command surface is described.
func usage(w io.Writer) {
	fmt.Fprint(w, `onegw — LLM gateway (OpenAI / Anthropic / Gemini surfaces)

Usage:
  onegw [-config FILE]                 run the gateway (default 127.0.0.1:8080)
  onegw --bg                           same, detached: survives the terminal closing
                                       (log <data_dir>/onegw.log; refuses to start
                                       if something already listens on the port)
  onegw version                        print the build stamp
  onegw update [--check] [--force] [--yes]
                                       check for, or apply, a release
  onegw oauth <login|list|refresh> …   subscription (device-flow) accounts
  onegw help                           this text

Flags are read from the environment too: ONEGW_CONFIG, ONEGW_LISTEN,
ONEGW_KEYS, ONEGW_ADMIN_PASSWORD, ONEGW_PROVIDER_<NAME>_KEY, ONEGW_DATA_DIR.
ONEGW_LISTEN, ONEGW_KEYS and ONEGW_ADMIN_PASSWORD override the config file;
ONEGW_DATA_DIR only supplies a default when the config sets no data_dir.
`)
}

func runGateway() {
	cfgPath := flag.String("config", "", "path to onegw.toml (default ./onegw.toml, then $ONEGW_CONFIG)")
	bg := flag.Bool("bg", false, "detach and keep serving after the terminal closes; stdout/stderr go to <data_dir>/onegw.log")
	flag.Parse()

	path := *cfgPath
	if path == "" {
		if v := os.Getenv("ONEGW_CONFIG"); v != "" {
			path = v
		} else {
			path = "onegw.toml"
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) && *cfgPath == "" && os.Getenv("ONEGW_CONFIG") == "" {
			// No config file given and none present: run with defaults
			// (still serves /admin; providers come from env or later edits).
			cfg = &config.Config{}
			cfg.Defaults()
		} else {
			fatal("load config: %v", err)
		}
	}
	// --bg: hand the service to a detached child and free this terminal. Runs
	// before any side effect (password minting, memory tuning, the listener): the
	// child performs all of it and its output lands in the log file, so a first-run
	// admin password is still recoverable from there. The path is forwarded only
	// when the file exists, so the child repeats this function's own "no config
	// present -> defaults" resolution verbatim.
	if *bg {
		// Forward an absolute path only when the file really exists, so the
		// child repeats this function's own "no config present -> defaults"
		// resolution instead of failing on a path that vanished.
		forward := ""
		if st, serr := os.Stat(path); serr == nil && st.Mode().IsRegular() {
			if abs, aerr := filepath.Abs(path); aerr == nil {
				forward = abs
			}
		}
		os.Exit(startBackground(cfg, forward, os.Args[1:]))
	}

	// First boot with no admin_password anywhere: mint one, persist it under
	// the data dir, and print it once (below) so the operator can sign in.
	if _, pwErr := cfg.GenerateAndStoreAdminPassword(); pwErr != nil {
		log.Printf("onegw: generated admin password could NOT be persisted (%v) — it changes on every restart; fix %s or set admin_password", pwErr, cfg.Server.DataDir)
	}
	logAdminPassword(cfg)

	applyMemoryTuning(cfg)

	srv, err := server.New(cfg)
	if err != nil {
		fatal("init server: %v", err)
	}
	defer srv.Close()
	srv.SetConfigPath(path) // powers /admin/config* (masked view, reload, keys/aliases)
	srv.StampOwner()        // writes <data_dir>/owner.json and fills /admin/health's owner block (#42); re-stamped by every successful reload

	// SO_REUSEPORT lets a replacement binary bind the same port while this
	// process is still serving, enabling zero-drop rolling restarts (start
	// the new process, health-check it, then signal this one to drain).
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if cerr := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); cerr != nil {
				return cerr
			}
			return serr
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", cfg.Server.Listen)
	if err != nil {
		fatal("listen %s: %v", cfg.Server.Listen, err)
	}

	// The update service re-reads [update] settings on every wake; the
	// closure keeps a pointer to the CURRENT config (replaced on SIGHUP)
	// so auto-apply always hands off with the live listen address and
	// admin password, never a stale snapshot.
	var curCfg atomic.Pointer[config.Config]
	curCfg.Store(cfg)
	upd := update.NewService(func() update.Settings {
		c := curCfg.Load()
		return update.Settings{
			Interval: int64(c.UpdateEvery() / time.Second),
			Auto:     c.Update.Auto,
			Repo:     c.Update.Repo,
			Listen:   c.Server.Listen,
			Password: c.Server.AdminPassword,
		}
	})
	upd.Start()
	srv.SetUpdater(upd) // dashboard Settings card reads GET/POST /admin/api/v1/update (#61)
	// Dashboard-driven reloads (PUT /admin/config/reload) swap the server
	// state without a signal; the hook keeps this outer-mux update
	// handler's config copy in sync so its credential never goes stale (#63),
	// and moves the GC soft limit with a reloaded buffered-byte budget.
	srv.SetOnConfigReload(newReloadHook(&curCfg))
	defer upd.Stop()

	// srv.Handler() wraps its mux (recovery), so /admin/update mounts on
	// an outer mux that delegates everything else inward; the
	// method-specific pattern outranks the "/" catch-all.
	outer := http.NewServeMux()
	outer.Handle("/admin/update", updateHandler(upd, curCfg.Load))
	outer.Handle("/", srv.Handler())

	httpSrv := &http.Server{
		Handler:           outer,
		ReadHeaderTimeout: 10 * time.Second,
		// No global WriteTimeout: streams run for minutes.
		// Loopback agent clients (omp/hermes/opencode) hold pooled
		// keep-alive conns across long think/tool gaps; closing at 120s
		// makes their next POST reuse a dead socket ("socket connection
		// was closed unexpectedly", live 2026-09-11). 30m covers the
		// longest agentic gaps while still reclaiming abandoned conns.
		IdleTimeout: 30 * time.Minute,
	}

	log.Printf("onegw listening on %s (data: %s, budget: %d MiB, memlimit: %d MiB)",
		cfg.Server.Listen, cfg.Server.DataDir, cfg.Server.BufferCap>>20, debug.SetMemoryLimit(-1)>>20)

	// Signal loop: SIGTERM/SIGINT drain in-flight requests and exit;
	// SIGHUP hot-reloads the config — providers, combos, auth keys, saver
	// toggle, admin password, body cap, flush interval. A bad file is
	// rejected and the previous config keeps serving.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for sig := range sigc {
			if sig != syscall.SIGHUP {
				log.Printf("onegw draining on %v", sig)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := httpSrv.Shutdown(ctx); err != nil {
					log.Printf("onegw drain: %v", err)
				}
				cancel()
				return
			}
			fresh, err := config.Load(path)
			if err != nil {
				log.Printf("onegw reload rejected (%s): %v", path, err)
				continue
			}
			srv.Reload(fresh)
			log.Printf("onegw config reloaded: %d providers, %d combos, %d auth keys",
				len(fresh.Providers), len(fresh.Combos), len(fresh.Auth.KeyList))
			curCfg.Store(fresh) // update service + admin routes read the new settings
		}
	}()

	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fatal("serve: %v", err)
	}
}

// logAdminPassword prints the first-run credential exactly once: a gateway
// installed with no admin_password (bare `onegw`, or the docker image whose
// default config ships the key empty) would otherwise fall back to the
// guessable "admin" with the operator none the wiser. The generated value is
// persisted at <data_dir>/admin_password, so this banner is the only place
// it is ever displayed; restarts and reloads keep the stored value silently.
func logAdminPassword(cfg *config.Config) {
	if !cfg.AdminPasswordGenerated() {
		return
	}
	log.Printf("onegw: FIRST-RUN ADMIN PASSWORD (no admin_password was configured): %s", cfg.Server.AdminPassword)
	log.Printf("onegw: sign in at /admin with it, or set admin_password in your config to choose your own — stored in %s/%s",
		cfg.Server.DataDir, config.AdminPasswordFile)
}

// applyMemoryTuning sets a soft heap limit when the operator has not. The limit
// MUST clear the configured buffered-byte budget: bytes the budget permits but
// the GC fights are throughput bought with nothing, and a reservation is held
// for the whole upstream round-trip. The default 48 MiB budget keeps the
// historic 90 MiB ceiling (100 MB RSS envelope).
func applyMemoryTuning(cfg *config.Config) {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(heapLimitBytes(cfg.Server.BufferCap))
	}
	if os.Getenv("GOGC") == "" {
		// Slightly more aggressive GC than the default 100 keeps the heap
		// tight; throughput impact is negligible for a proxy workload.
		debug.SetGCPercent(60)
	}
	// Cap OS threads to avoid thread explosion under many concurrent streams.
	if os.Getenv("GOMAXPROCS") == "" {
		if n := runtime.NumCPU(); n > 4 {
			runtime.GOMAXPROCS(4)
		}
	}
}

// heapLimitBytes is the tuned soft heap limit for a buffered-byte budget: the
// budget plus a quarter for the handler work around it, never below the 90 MiB
func heapLimitBytes(bufferCap int64) int64 {
	if need := bufferCap + bufferCap/4; need > 90<<20 {
		return need
	}
	return 90 << 20
}

// newReloadHook is the ONE definition of what a config reload does outside
// server.apply: mirror the live config for the outer-mux /admin/update handler
// (#63) and re-apply the memory tuning for the reloaded buffered-byte budget.
// runGateway and the reload regression tests share it, so a test cannot pass
// on a private copy of the wiring while the shipped closure loses a line.
func newReloadHook(curCfg *atomic.Pointer[config.Config]) func(*config.Config) {
	return func(fresh *config.Config) {
		curCfg.Store(fresh)
		applyMemoryTuning(fresh)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "onegw: "+format+"\n", args...)
	os.Exit(1)
}
