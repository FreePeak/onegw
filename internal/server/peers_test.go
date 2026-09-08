package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writePeerHeartbeat plants a heartbeat file and returns its path.
func writePeerHeartbeat(t *testing.T, dir string, doc heartbeatDoc) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, peerHeartbeatPrefix+strconv.Itoa(doc.PID)+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanHeartbeatsLivePeer(t *testing.T) {
	dir := t.TempDir()
	self := os.Getpid()
	// getpid()+1000 is unallocated in a test run: alive-check must drop it.
	dead := self + 1000
	if pidAlive(dead) {
		t.Skipf("pid %d unexpectedly alive", dead)
	}
	writePeerHeartbeat(t, dir, heartbeatDoc{PID: dead, RefreshedAt: time.Now(), Listen: "127.0.0.1:9999"})
	if peers := scanHeartbeats(dir, self, time.Now()); len(peers) != 0 {
		t.Fatalf("dead pid must not be a peer, got %+v", peers)
	}

	writePeerHeartbeat(t, dir, heartbeatDoc{PID: self, RefreshedAt: time.Now(), Listen: "127.0.0.1:9999"})
	peers := scanHeartbeats(dir, 0, time.Now()) // selfPID=0: the live pid above counts
	if len(peers) != 1 {
		t.Fatalf("want exactly 1 heartbeat peer, got %+v", peers)
	}
	p := peers[0]
	if p.PID != self || p.Source != "heartbeat" || p.Listen != "127.0.0.1:9999" {
		t.Fatalf("wrong peer: %+v", p)
	}
}

func TestScanHeartbeatsSelfExcluded(t *testing.T) {
	dir := t.TempDir()
	self := os.Getpid()
	writePeerHeartbeat(t, dir, heartbeatDoc{PID: self, RefreshedAt: time.Now(), Listen: "127.0.0.1:8080"})
	if peers := scanHeartbeats(dir, self, time.Now()); len(peers) != 0 {
		t.Fatalf("own heartbeat must be excluded, got %+v", peers)
	}
}

func TestScanHeartbeatsStaleTTL(t *testing.T) {
	dir := t.TempDir()
	self := os.Getpid()
	path := writePeerHeartbeat(t, dir, heartbeatDoc{PID: self, RefreshedAt: time.Now().Add(-2 * peerTTL), Listen: "127.0.0.1:8080"})
	old := time.Now().Add(-2 * peerTTL)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	// selfPID=0 so the liveness check passes: this isolates the TTL rule.
	if peers := scanHeartbeats(dir, 0, time.Now()); len(peers) != 0 {
		t.Fatalf("stale heartbeat must not be a peer, got %+v", peers)
	}
}

func TestScanHeartbeatsIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"onegw.db", "onegw-peer-junk.txt", "onegw-peer-notapid.json", "usage.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{ not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if peers := scanHeartbeats(dir, os.Getpid(), time.Now()); len(peers) != 0 {
		t.Fatalf("foreign or malformed files must be ignored, got %+v", peers)
	}
}

func TestScanPeersEarlyReturns(t *testing.T) {
	for _, dir := range []string{"", "memory", filepath.Join(t.TempDir(), "does-not-exist")} {
		if peers := ScanPeers(dir, os.Getpid(), time.Now()); peers != nil {
			// A missing dir has no heartbeats; the process-table scan may
			// still find real gateways, so only assert no heartbeat peers.
			for _, p := range peers {
				if p.Source == "heartbeat" {
					t.Fatalf("ScanPeers(%q) must yield no heartbeat peers, got %+v", dir, peers)
				}
			}
		}
	}
}

// TestScanPeersDeduplicatesByPid plants a heartbeat and checks it appears
// exactly once in a full ScanPeers (heartbeat + process table combined).
func TestScanPeersDeduplicatesByPid(t *testing.T) {
	dir := t.TempDir()
	self := os.Getpid()
	writePeerHeartbeat(t, dir, heartbeatDoc{PID: self, RefreshedAt: time.Now(), Listen: "127.0.0.1:8080"})
	peers := ScanPeers(dir, 0, time.Now())
	hits := 0
	for _, p := range peers {
		if p.PID == self {
			hits++
			if p.Source != "heartbeat" {
				t.Fatalf("heartbeat entry must win for pid %d, got %+v", self, p)
			}
		}
	}
	if hits != 1 {
		t.Fatalf("planted heartbeat must appear exactly once, got %d (%+v)", hits, peers)
	}
}

func TestStartPeerWatchWritesAndRemovesHeartbeat(t *testing.T) {
	dir := t.TempDir()
	w := StartPeerWatch(dir, "127.0.0.1:18080")
	if w == nil {
		t.Fatal("watch must start for a real data dir")
	}
	if _, err := os.Stat(heartbeatPath(dir, os.Getpid())); err != nil {
		t.Fatalf("heartbeat must exist right after start: %v", err)
	}
	w.Stop()
	if _, err := os.Stat(heartbeatPath(dir, os.Getpid())); !os.IsNotExist(err) {
		t.Fatalf("graceful stop must remove the heartbeat, got %v", err)
	}
}

func TestStartPeerWatchMemoryNoop(t *testing.T) {
	if w := StartPeerWatch("", "127.0.0.1:1"); w != nil {
		t.Fatal("empty data dir must disable the watch")
	}
	if w := StartPeerWatch("memory", "127.0.0.1:1"); w != nil {
		t.Fatal("in-memory data dir must disable the watch")
	}
}

func TestPeerCountReflectsScan(t *testing.T) {
	dir := t.TempDir()
	w := StartPeerWatch(dir, "127.0.0.1:18081")
	if w == nil {
		t.Fatal("watch must start")
	}
	defer w.Stop()
	// Plant a heartbeat for an alive pid that is not the watch process:
	// a short-lived child. StartPeerWatch's synchronous refresh already
	// seeded PeerCount without this file, so the delta is deterministic.
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skip("cannot start a child process")
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	writePeerHeartbeat(t, dir, heartbeatDoc{PID: child.Process.Pid, RefreshedAt: time.Now(), Listen: "127.0.0.1:18082"})
	before := PeerCount()
	w.refresh()
	if after := PeerCount(); after != before+1 {
		t.Fatalf("planted heartbeat must raise PeerCount by 1: %d -> %d", before, after)
	}
}

func TestLooksLikeGateway(t *testing.T) {
	for _, cmd := range []string{"/tmp/onegw-live.new", "./onegw -config x.toml", "onegw"} {
		if !looksLikeGateway(cmd) {
			t.Fatalf("%q must look like a gateway", cmd)
		}
	}
	for _, cmd := range []string{"vim onegw.toml", "tail -f onegw.log", "grep onegw x"} {
		if looksLikeGateway(cmd) {
			t.Fatalf("%q must NOT look like a gateway", cmd)
		}
	}
}

func TestScanProcessTableIgnoresNonGateways(t *testing.T) {
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skip("cannot start a child process")
	}
	defer func() { _ = sleep.Process.Kill(); _ = sleep.Wait() }()

	for _, p := range scanProcessTable(os.Getpid()) {
		if p.PID == sleep.Process.Pid {
			t.Fatalf("sleep child %d must not be a gateway peer", p.PID)
		}
		if p.Source != "process" {
			t.Fatalf("process-table peers must carry Source=process, got %+v", p)
		}
	}
}
