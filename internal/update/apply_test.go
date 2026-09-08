package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeGatewayScript is a "binary" that passes the smoke run (`<bin>
// version` exits 0) on unix.
const fakeGatewayScript = "#!/bin/sh\nif [ \"$1\" = version ]; then echo fake-gateway; exit 0; fi\necho serving\nsleep 60\n"

// applyFixture wires a full fake update: a fake releases API publishing
// the new "binary" as the platform asset, a fake /admin/update that
// reports a chosen pid, and recorded spawn/signal calls. execPath starts
// out as OLD-BINARY.
type applyFixture struct {
	execPath   string
	newContent string
	probePid   int // pid the fake admin endpoint reports
	spawnArgv  []string
	spawnErr   error
	spawnExits error // delivered on Spawned.Exit immediately
	signaled   []int
	release    *httptest.Server
	probeSrv   *httptest.Server
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	f := &applyFixture{
		execPath:   filepath.Join(t.TempDir(), "onegw"),
		newContent: fakeGatewayScript,
		probePid:   4242,
	}
	if err := os.WriteFile(f.execPath, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		asset := "onegw-" + runtime.GOOS + "-" + runtime.GOARCH
		_, _ = fmt.Fprintf(w, `{"tag_name":"v9.9.9","name":"v9.9.9","assets":[{"name":%q,"url":%q,"size":%d}]}`,
			asset, f.release.URL+"/asset", len(f.newContent))
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(f.newContent))
	})
	f.release = httptest.NewServer(mux)
	f.probeSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Password") != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"pid":%d,"current":"v0.0.1"}`, f.probePid)
	}))
	t.Cleanup(func() { f.release.Close(); f.probeSrv.Close() })
	return f
}

func (f *applyFixture) opt(t *testing.T) Opt {
	t.Helper()
	return Opt{
		Repo:           "r",
		Base:           f.release.URL,
		AdminPassword:  "pw",
		ExecPath:       f.execPath,
		Argv:           []string{f.execPath, "-config", "/etc/onegw.toml"},
		OldPID:         4321,
		Listen:         "127.0.0.1:8080",
		HandoffTimeout: 2 * time.Second,
		Out:            io.Discard,
		Spawn: func(path string, argv []string, env []string) (*Spawned, error) {
			f.spawnArgv = argv
			exit := make(chan error, 1)
			if f.spawnExits != nil {
				exit <- f.spawnExits
			}
			return &Spawned{Pid: 4242, Exit: exit}, f.spawnErr
		},
		ProbeURL: func(listen string) string { return f.probeSrv.URL },
		Signal: func(pid int) error {
			f.signaled = append(f.signaled, pid)
			return nil
		},
	}
}

func TestApplyHandoffSuccess(t *testing.T) {
	f := newApplyFixture(t)
	rel, err := Run(context.Background(), f.opt(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rel.Tag != "v9.9.9" {
		t.Fatalf("tag = %s", rel.Tag)
	}
	// The binary on disk must now be the new one, with the old preserved.
	got, err := os.ReadFile(f.execPath)
	if err != nil || string(got) != fakeGatewayScript {
		t.Fatalf("exec path not swapped: %v (%q)", err, got)
	}
	old, err := os.ReadFile(f.execPath + ".old")
	if err != nil || string(old) != "OLD-BINARY" {
		t.Fatalf("old binary not preserved: %v (%q)", err, old)
	}
	// The old pid must have been drained, and the new process started
	// with the gateway's argv (config path included).
	if len(f.signaled) != 1 || f.signaled[0] != 4321 {
		t.Fatalf("signaled = %v, want [4321]", f.signaled)
	}
	if len(f.spawnArgv) != 3 || f.spawnArgv[0] != f.execPath || f.spawnArgv[2] != "/etc/onegw.toml" {
		t.Fatalf("spawn argv = %v", f.spawnArgv)
	}
}

func TestApplyRollbackOnFailedTakeover(t *testing.T) {
	f := newApplyFixture(t)
	f.probePid = 999 // never names the spawned pid: takeover never proves
	_, err := Run(context.Background(), f.opt(t))
	if err == nil || !strings.Contains(err.Error(), "never took over") {
		t.Fatalf("want takeover failure, got %v", err)
	}
	// The old binary must be back on the path and no drain may have
	// happened: the serving gateway is untouched.
	got, err := os.ReadFile(f.execPath)
	if err != nil || string(got) != "OLD-BINARY" {
		t.Fatalf("old binary not restored: %v (%q)", err, got)
	}
	if len(f.signaled) != 0 {
		t.Fatalf("old pid must not be drained on failure, signaled = %v", f.signaled)
	}
	if _, err := os.Stat(f.execPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf("no backup must remain after rollback, stat err = %v", err)
	}
}

func TestApplyRollbackOnFastCrash(t *testing.T) {
	f := newApplyFixture(t)
	f.spawnExits = errors.New("exit status 1") // child dies instantly
	_, err := Run(context.Background(), f.opt(t))
	if err == nil || !strings.Contains(err.Error(), "never took over") {
		t.Fatalf("want takeover failure on crash, got %v", err)
	}
	got, _ := os.ReadFile(f.execPath)
	if string(got) != "OLD-BINARY" {
		t.Fatalf("old binary not restored after crash, got %q", got)
	}
	if len(f.signaled) != 0 {
		t.Fatalf("old pid must not be drained after crash, signaled = %v", f.signaled)
	}
}

func TestApplyRollbackOnSpawnError(t *testing.T) {
	f := newApplyFixture(t)
	f.spawnErr = errors.New("permission denied")
	_, err := Run(context.Background(), f.opt(t))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want spawn failure, got %v", err)
	}
	got, _ := os.ReadFile(f.execPath)
	if string(got) != "OLD-BINARY" {
		t.Fatalf("old binary not restored, got %q", got)
	}
}

func TestApplyRefusesHandoffWithoutPassword(t *testing.T) {
	f := newApplyFixture(t)
	o := f.opt(t)
	o.AdminPassword = ""
	_, err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "admin password") {
		t.Fatalf("want password refusal, got %v", err)
	}
	// Nothing moved.
	got, _ := os.ReadFile(f.execPath)
	if string(got) != "OLD-BINARY" {
		t.Fatalf("binary must be untouched, got %q", got)
	}
}

func TestApplyCheckOnlyLeavesDisk(t *testing.T) {
	f := newApplyFixture(t)
	o := f.opt(t)
	o.CheckOnly = true
	rel, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("check-only Run: %v", err)
	}
	if rel.Tag != "v9.9.9" {
		t.Fatalf("tag = %s", rel.Tag)
	}
	got, _ := os.ReadFile(f.execPath)
	if string(got) != "OLD-BINARY" {
		t.Fatalf("check-only must not touch the binary, got %q", got)
	}
}
