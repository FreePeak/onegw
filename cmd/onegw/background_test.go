package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChildArgvIsExactlyOneConfigAndNoDaemonFlag(t *testing.T) {
	// --bg must vanish (a surviving flag makes the update handoff refuse to
	// bind), and -config must appear exactly once: the parent resolves the
	// path, and a doubled flag is what `ps` and supervisors match on.
	got := childArgv([]string{"--bg", "-config", "/rel/onegw.toml", "--quiet"}, "/abs/onegw.toml")
	want := []string{"-config", "/abs/onegw.toml", "--quiet"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("childArgv = %v, want %v", got, want)
	}
	// -config=path form and a leading --bg both go; unrelated flags stay put.
	got = childArgv([]string{"-bg", "-config=/x.toml", "-h=0", "extra"}, "/y.toml")
	want = []string{"-config", "/y.toml", "-h=0", "extra"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("equals-form handling = %v, want %v", got, want)
	}
	// No config to forward (defaults path): argv keeps the user's other flags.
	if got := childArgv([]string{"--bg"}, ""); len(got) != 0 {
		t.Errorf("childArgv = %v, want empty", got)
	}
	if n := strings.Count(strings.Join(childArgv([]string{"--bg", "-config", "a", "-config", "b"}, "/z"), " "), "-config"); n != 1 {
		t.Errorf("config flags = %d, want exactly 1", n)
	}
}

func TestBackgroundLogPath(t *testing.T) {
	if got := backgroundLogPath("/Users/x/.onegw/data"); got != filepath.Join("/Users/x/.onegw/data", "onegw.log") {
		t.Errorf("data dir: got %q", got)
	}
	// An in-memory data dir must not try to write to a directory called
	// "memory"; the working directory is the honest fallback.
	if got := backgroundLogPath("memory"); got != "onegw.log" {
		t.Errorf("memory: got %q", got)
	}
	if got := backgroundLogPath(""); got != "onegw.log" {
		t.Errorf("empty: got %q", got)
	}
}

func TestAlreadyListeningOnlyAnswersTheQuestionThatMatters(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !alreadyListening(ln.Addr().String(), 200*time.Millisecond) {
		t.Fatal("a live listener must be detected: this is the guard that stops a second gateway splitting traffic on one port")
	}
	addr := ln.Addr().String()
	ln.Close()
	if alreadyListening(addr, 200*time.Millisecond) {
		t.Error("a closed listener must not read as busy")
	}
}

func TestPortOf(t *testing.T) {
	if got := portOf("127.0.0.1:8080"); got != "8080" {
		t.Errorf("portOf = %q, want 8080 (the lsof hint must name the port, not the socket path)", got)
	}
}

func TestTailOfFileReportsChildFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "onegw.log")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 4000)+"\nload config: provider cline unknown kind \"cline\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailOfFile(p, 200)
	if !strings.Contains(got, "unknown kind") {
		t.Errorf("tail = %q, must carry the child's actual failure", got)
	}
	if len(got) > 200 {
		t.Errorf("tail length = %d, must stay bounded", len(got))
	}
	if !strings.Contains(tailOfFile(filepath.Join(t.TempDir(), "missing"), 100), "unreadable") {
		t.Error("an unreadable log must say so rather than reporting success")
	}
}
