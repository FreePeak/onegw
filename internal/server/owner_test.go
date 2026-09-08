package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"onegw/internal/config"
)

// ownerTestToml renders a minimal valid config whose data_dir points at
// dir. Password in-file so the admin gate is exercised end to end.
func ownerTestToml(dir string) string {
	return `[server]
data_dir = "` + dir + `"
admin_password = "pw-test"

[auth]
keys = ["key-a"]
`
}

// /admin/health must answer "who is running what" (#42): the owner block
// carries pid, listen, start time, config path + mtime, and build stamp,
// behind the same admin gate as the rest of health.
func TestHealthReportsOwner(t *testing.T) {
	dataDir := t.TempDir()
	srv, h, cfgPath := newTestServerFromFile(t, ownerTestToml(dataDir))
	srv.StampOwner() // main() stamps once at startup; simulate it here

	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	req.Header.Set("X-Admin-Password", "pw-test")
	w := do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("health with password: got %d", w.Code)
	}
	var resp struct {
		Owner struct {
			PID        int    `json:"pid"`
			Listen     string `json:"listen"`
			StartedAt  string `json:"started_at"`
			ConfigPath string `json:"config_path"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("health body: %v", err)
	}
	if resp.Owner.PID != os.Getpid() {
		t.Fatalf("owner pid %d, want %d", resp.Owner.PID, os.Getpid())
	}
	if resp.Owner.ConfigPath != cfgPath {
		t.Fatalf("owner config path %q, want %q", resp.Owner.ConfigPath, cfgPath)
	}
	if _, err := time.Parse(time.RFC3339, resp.Owner.StartedAt); err != nil {
		t.Fatalf("started_at %q not RFC3339: %v", resp.Owner.StartedAt, err)
	}

	// The same record must be on disk for crash forensics.
	b, err := os.ReadFile(filepath.Join(dataDir, "owner.json"))
	if err != nil {
		t.Fatalf("owner.json: %v", err)
	}
	var disk struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(b, &disk); err != nil || disk.PID != os.Getpid() {
		t.Fatalf("owner.json pid %d err %v, want %d", disk.PID, err, os.Getpid())
	}
}

// A successful reload is the "config changed underneath you" event: the
// ownership record must re-stamp with the new config mtime (#42).
func TestReloadRestampsOwner(t *testing.T) {
	dataDir := t.TempDir()
	srv, h, cfgPath := newTestServerFromFile(t, ownerTestToml(dataDir))
	srv.StampOwner() // main() stamps once at startup; simulate it here
	mtime := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dataDir, "owner.json"))
		if err != nil {
			t.Fatalf("owner.json: %v", err)
		}
		var o struct {
			ConfigMtime string `json:"config_mtime"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatalf("parse owner.json: %v", err)
		}
		return o.ConfigMtime
	}

	before := mtime()
	time.Sleep(10 * time.Millisecond) // filesystem mtime granularity
	if err := os.WriteFile(cfgPath, []byte(ownerTestToml(dataDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load updated config: %v", err)
	}
	srv.Reload(cfg)

	after := mtime()
	if after == before {
		t.Fatalf("owner.json not re-stamped on reload: config_mtime %q unchanged", after)
	}
	// Health keeps serving the fresh record.
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	req.Header.Set("X-Admin-Password", "pw-test")
	w := do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("health after reload: got %d", w.Code)
	}
	var resp struct {
		Owner struct {
			ConfigMtime string `json:"config_mtime"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Owner.ConfigMtime != after {
		t.Fatalf("health owner mtime %q != on-disk %q", resp.Owner.ConfigMtime, after)
	}
}

// A rejected reload (invalid config) must NOT re-stamp: the ownership
// record keeps describing the config that is actually serving.
func TestRejectedReloadKeepsOwnerStamp(t *testing.T) {
	dataDir := t.TempDir()
	srv, _, cfgPath := newTestServerFromFile(t, ownerTestToml(dataDir))
	srv.StampOwner() // main() stamps once at startup; simulate it here
	readMtime := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dataDir, "owner.json"))
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			ConfigMtime string `json:"config_mtime"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatal(err)
		}
		return o.ConfigMtime
	}
	before := readMtime()
	time.Sleep(10 * time.Millisecond)
	// Valid TOML, but a non-loopback listen with no auth keys: rejected
	// by apply's fail-closed gate, so Reload must not re-stamp.
	broken := `[server]
listen = "0.0.0.0:9999"
data_dir = "` + dataDir + `"
`
	if err := os.WriteFile(cfgPath, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load open-bind config: %v", err)
	}
	srv.Reload(cfg) // apply rejects; no state swap, no re-stamp
	if got := readMtime(); got != before {
		t.Fatalf("rejected reload must not re-stamp: %q -> %q", before, got)
	}
}
