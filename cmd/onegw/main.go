// Command onegw is the LLM gateway binary: one process fronting OpenAI,
// Anthropic, and Gemini-compatible surfaces with translation, token saving,
// fallback routing, and usage tracking — inside a 100 MB RAM envelope.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"onegw/internal/config"
	"onegw/internal/server"
)

func main() {
	cfgPath := flag.String("config", "", "path to onegw.toml (default ./onegw.toml, then $ONEGW_CONFIG)")
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

	applyMemoryTuning()

	srv, err := server.New(cfg)
	if err != nil {
		fatal("init server: %v", err)
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No global WriteTimeout: streams run for minutes.
		IdleTimeout: 120 * time.Second,
	}

	log.Printf("onegw listening on %s (data: %s, budget: %d MiB)",
		cfg.Server.Listen, cfg.Server.DataDir, cfg.Server.BufferCap>>20)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("serve: %v", err)
	}
}

// applyMemoryTuning sets a soft heap limit when the operator has not. The
// hard target is 100 MB RSS; GOMEMLIMIT at 90 MiB keeps the Go GC working
// before the process approaches the ceiling.
func applyMemoryTuning() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(90 << 20)
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "onegw: "+format+"\n", args...)
	os.Exit(1)
}
