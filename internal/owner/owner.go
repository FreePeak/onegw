// Package owner records which gateway process owns the live service and
// its data_dir: pid, binary build stamp, config path + mtime, start time,
// listen address, argv. The owner.json file gives operators (and agents)
// a durable answer to "which instance is canonical" without process-table
// archaeology — including after a crash, when the stale pid is evidence —
// and /admin/health reports the same record live from memory.
//
// The file is written at startup and re-stamped on every successful
// SIGHUP reload (config mtime changes); it is deliberately NOT removed
// on exit. Issue #42: concurrent sessions made kill/start decisions
// against silently stale assumptions because nothing recorded who was
// running what.
package owner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"
)

// Build is the binary identity: what code is actually running.
type Build struct {
	GoVersion     string `json:"go_version"`
	ModuleVersion string `json:"module_version,omitempty"` // vN.N.N, or "(devel)"
	Revision      string `json:"revision,omitempty"`       // vcs.revision, when built inside a git tree
	Modified      bool   `json:"modified,omitempty"`       // vcs.modified: dirty tree at build time
}

// Info is one ownership record. StartedAt and ConfigMtime are RFC3339
// strings so the file diffs cleanly in review and survives JSON round
// trips without a custom marshaller.
type Info struct {
	PID         int      `json:"pid"`
	Listen      string   `json:"listen"`
	StartedAt   string   `json:"started_at"`
	ConfigPath  string   `json:"config_path"`
	ConfigMtime string   `json:"config_mtime,omitempty"`
	Argv        []string `json:"argv"`
	Build       Build    `json:"build"`
}

// Stamp extracts the build identity from the running binary. Outside a
// git tree (release/Docker builds) Revision is empty and ModuleVersion
// falls back to "(devel)" — exactly what `go version -m` reports.
func Stamp() Build {
	b := Build{GoVersion: fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	b.ModuleVersion = bi.Main.Version
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b
}

// Capture builds the ownership record for the running process. startedAt
// is the process start time (not capture time) so uptime in owner.json
// matches /admin/health's.
func Capture(listen, configPath string, startedAt time.Time) Info {
	info := Info{
		PID:        os.Getpid(),
		Listen:     listen,
		StartedAt:  startedAt.UTC().Format(time.RFC3339),
		ConfigPath: configPath,
		Argv:       os.Args,
		Build:      Stamp(),
	}
	if fi, err := os.Stat(configPath); err == nil {
		info.ConfigMtime = fi.ModTime().UTC().Format(time.RFC3339Nano)
	}
	return info
}

// file returns the owner.json path for a data dir. An empty dir or the
// store's "memory" sentinel means no on-disk data_dir: nothing to own on
// disk, and every write is a no-op (the in-memory record still powers
// /admin/health).
func file(dataDir string) string {
	if dataDir == "" || dataDir == "memory" {
		return ""
	}
	return filepath.Join(dataDir, "owner.json")
}

// Write persists the record as <data_dir>/owner.json, atomically
// (tmp + rename so a reader never sees a partial file). No-op without a
// real data_dir.
func Write(dataDir string, info Info) error {
	p := file(dataDir)
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("owner: mkdir %s: %w", dataDir, err)
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("owner: marshal: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("owner: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("owner: rename: %w", err)
	}
	return nil
}

// Read loads the record a previous (or current) instance wrote. A stale
// file with a dead pid is a valid result — liveness is the caller's
// check, not this package's.
func Read(dataDir string) (Info, error) {
	var info Info
	p := file(dataDir)
	if p == "" {
		return info, fmt.Errorf("owner: no data_dir")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, fmt.Errorf("owner: parse %s: %w", p, err)
	}
	return info, nil
}
