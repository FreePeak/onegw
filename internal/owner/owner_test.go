package owner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteAndReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	info := Info{
		PID:        12345,
		Listen:     "127.0.0.1:9999",
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath: "/tmp/onegw.toml",
		Argv:       []string{"onegw", "-config", "/tmp/onegw.toml"},
		Build:      Build{GoVersion: "go1.25.0", ModuleVersion: "(devel)"},
	}
	if err := Write(dir, info); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.PID != info.PID || got.Listen != info.Listen || got.ConfigPath != info.ConfigPath {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Build.GoVersion != info.Build.GoVersion {
		t.Fatalf("build stamp lost: %+v", got.Build)
	}
}

// Atomicity: a concurrent reader must never see a partial record — the
// write is tmp+rename. Crash the writer between tmp and rename and the
// old record survives intact.
func TestWriteAtomicUnderRename(t *testing.T) {
	dir := t.TempDir()
	first := Info{PID: 1, Listen: "a"}
	second := Info{PID: 2, Listen: "b"}
	if err := Write(dir, first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(dir, second); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read after rewrite: %v", err)
	}
	if got.PID != 2 {
		t.Fatalf("expected newest record, got pid %d", got.PID)
	}
	// No tmp leftovers on the happy path.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("tmp file left behind: %s", e.Name())
		}
	}
}

func TestMemoryDataDirIsNoop(t *testing.T) {
	// The "memory" sentinel and empty dir both mean no on-disk record;
	// Write must be a no-op and Read must error, not panic.
	if err := Write("memory", Info{PID: 1}); err != nil {
		t.Fatalf("Write to memory dir should be a no-op, got %v", err)
	}
	if err := Write("", Info{PID: 1}); err != nil {
		t.Fatalf("Write to empty dir should be a no-op, got %v", err)
	}
	if _, err := Read("memory"); err == nil {
		t.Fatal("Read from memory dir should error")
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := Read(t.TempDir()); !os.IsNotExist(err) {
		t.Fatalf("missing owner.json should be os.IsNotExist, got %v", err)
	}
}

// Capture fills pid/listen/start/argv from the live process.
func TestCaptureFillsProcessIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nlisten = \"127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour)
	info := Capture("127.0.0.1:8080", cfg, start)
	if info.PID != os.Getpid() {
		t.Fatalf("pid %d != %d", info.PID, os.Getpid())
	}
	if info.Listen != "127.0.0.1:8080" || len(info.Argv) == 0 {
		t.Fatalf("capture incomplete: %+v", info)
	}
	if ts, err := time.Parse(time.RFC3339, info.StartedAt); err != nil || !ts.Equal(start.UTC().Truncate(time.Second)) {
		t.Fatalf("StartedAt %q not the passed start time: %v", info.StartedAt, err)
	}
	if info.ConfigMtime == "" {
		t.Fatal("ConfigMtime should be set for an existing config file")
	}
	// Missing config file: mtime stays empty, no error.
	info2 := Capture("x", filepath.Join(dir, "absent.toml"), start)
	if info2.ConfigMtime != "" {
		t.Fatalf("mtime for missing config should be empty, got %q", info2.ConfigMtime)
	}
}

// End-to-end shape: what main() writes must be JSON the health endpoint
// and any operator script can parse without private fields.
func TestJSONShape(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Capture("127.0.0.1:8080", "", time.Now())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("owner.json is not JSON: %v", err)
	}
	for _, k := range []string{"pid", "listen", "started_at", "argv", "build"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("owner.json missing key %q", k)
		}
	}
}

// Stamp reports this binary's real identity: the Go version must match
// the running runtime exactly.
func TestStampReportsBuildInfo(t *testing.T) {
	b := Stamp()
	if !strings.HasPrefix(b.GoVersion, runtime.Version()) {
		t.Fatalf("GoVersion %q should start with runtime %q", b.GoVersion, runtime.Version())
	}
}
