package main

// --bg: start the gateway detached, so closing the terminal does not take the
// proxy down with it. Every agent session on this box dials 127.0.0.1:8080, so
// "start it and walk away" must not become "start a SECOND one and walk away":
// SO_REUSEPORT happily binds the same port for two independent processes, each
// with its own rate windows, usage buffers and OAuth token store, and the
// kernel then splits new connections between them. That is not a crash — it is
// worse, because both look healthy. main.go's unknown-command guard exists for
// the same failure seen in the container (2026-09-13); this is the same failure
// reached from the other direction, so --bg refuses to bind where something
// already answers.

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"onegw/internal/config"
)

// bgReadiness bounds how long --bg waits for the child to start listening
// before declaring the launch failed. A config that no longer loads surfaces
// here (the child exits at once), so the operator reads the reason instead of
// discovering an empty port later.
const bgReadiness = 15 * time.Second

// childArgv builds the child's command line: it drops the daemonising flag and
// any -config the parent already resolved, then prepends the absolute config
// path. Stripping --bg is load-bearing for `onegw update`, whose handoff
// re-execs the RUNNING process's own argv (apply.go: argv = os.Args) while the
// old gateway is still bound — a surviving --bg would make the replacement hit
// this command's own busy-port refusal, and every self-update of a --bg-started
// gateway would roll back. Dropping -config keeps exactly one in argv: both
// forms parse, but a doubled flag is what `ps` and every supervisor match on.
func childArgv(args []string, cfgAbs string) []string {
	out := make([]string, 0, len(args)+2)
	skipValue := false
	for _, a := range args {
		if skipValue {
			skipValue = false
			continue
		}
		name, _, hasValue := strings.Cut(a, "=")
		switch name {
		case "-bg", "--bg":
			continue
		case "-config", "--config":
			if !hasValue {
				skipValue = true // the path is the next argv token
			}
			continue
		}
		out = append(out, a)
	}
	if cfgAbs != "" {
		out = append([]string{"-config", cfgAbs}, out...)
	}
	return out
}

func backgroundLogPath(dataDir string) string {
	if dataDir ***REMOVED*** "" || dataDir ***REMOVED*** "memory" {
		return "onegw.log"
	}
	return filepath.Join(dataDir, "onegw.log")
}

// alreadyListening reports whether something accepts TCP connections at addr.
// A dial, not a bind: a second gateway CAN bind this port (that is what
// SO_REUSEPORT is for), so only the connect attempt answers the question that
// matters — is a gateway already serving here?
func alreadyListening(addr string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// startBackground spawns the detached gateway and waits for it to answer on its
// listen address, returning the parent's exit code. cfgAbs is the config the
// parent resolved, already absolute (possibly ""): forwarding it stops an
// inherited cwd from changing which file the service runs on.
func startBackground(cfg *config.Config, cfgAbs string, args []string) int {
	addr := cfg.Server.Listen
	if alreadyListening(addr, 500*time.Millisecond) {
		fmt.Fprintf(os.Stderr, "onegw: something is already listening on %s — refusing to start a second gateway.\n"+
			"Two processes on one port do not crash each other, they SPLIT TRAFFIC (SO_REUSEPORT), each\n"+
			"keeping its own rate windows, usage buffers and OAuth token store, while both look healthy.\n"+
			"Find it: lsof -nP -iTCP:%s -sTCP:LISTEN — then stop that pid, or restart it, rather than starting another.\n",
			addr, portOf(addr))
		return 2
	}

	logPath := backgroundLogPath(cfg.Server.DataDir)
	if dir := filepath.Dir(logPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fatal("--bg log dir %s: %v", dir, err)
		}
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fatal("--bg log %s: %v", logPath, err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		_ = logFile.Close()
		fatal("--bg stdin: %v", err)
	}

	argv := childArgv(args, cfgAbs)

	cmd := exec.Command(executablePath(), argv...)
	cmd.Dir = workingDir()
	cmd.Stdin = devNull
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Setsid puts the child in a new session: the terminal's SIGHUP on close
	// reaches the foreground group of the OLD session, never the gateway.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		fatal("--bg spawn: %v", err)
	}
	pid := cmd.Process.Pid

	// Reap in the background: an exit status is the only evidence of an early
	// death, and the parent must not block on a long-lived child.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	_ = logFile.Close() // the child holds its own dup; the parent needs nothing

	deadline := time.NewTimer(bgReadiness)
	defer deadline.Stop()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case werr := <-exited:
			fmt.Fprintf(os.Stderr, "onegw: --bg child (pid %d) exited before it bound %s: %v\nlog tail (%s):\n%s\n",
				pid, addr, werr, logPath, tailOfFile(logPath, 1200))
			return 1
		case <-deadline.C:
			fmt.Fprintf(os.Stderr, "onegw: --bg child (pid %d) is not listening on %s after %s.\nlog tail (%s):\n%s\n",
				pid, addr, bgReadiness, logPath, tailOfFile(logPath, 1200))
			return 1
		case <-tick.C:
			if alreadyListening(addr, 300*time.Millisecond) {
				fmt.Printf("onegw running in the background: pid %d, listening on %s\n"+
					"log:     %s\n"+
					"stop:    kill -TERM %d      (drains in-flight requests)\n"+
					"restart: kill -TERM %d && onegw --bg\n",
					pid, addr, logPath, pid, pid)
				return 0
			}
		}
	}
}

// executablePath prefers the running binary itself over PATH, so a dev build or
// a scratch install re-execs itself instead of some other onegw.
func executablePath() string {
	if exe, err := os.Executable(); err ***REMOVED*** nil {
		return exe
	}
	return os.Args[0]
}

func workingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err ***REMOVED*** nil {
		return port
	}
	return addr
}

// tailOfFile reads the last n bytes of the log for failure reporting: a child
// that dies at config load must fail loudly, not silently.
func tailOfFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	off := maxInt64(0, st.Size()-n)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "(unreadable)"
	}
	buf := make([]byte, st.Size()-off)
	read, _ := io.ReadFull(f, buf)
	return strings.TrimSpace(string(buf[:read]))
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
