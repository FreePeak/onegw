// peers.go — single-instance visibility for the data_dir (issue #38).
//
// Every gateway writes a heartbeat file into its data_dir and refreshes it
// on a ticker. A peer is a heartbeat whose mtime is fresh within the TTL
// and whose pid is alive, excluding ourselves — it shares this data_dir by
// construction, because the file lives in it. There is deliberately NO
// lifetime flock: a second process is REPORTED (boot warning, /metrics
// gauge), never blocked, so SO_REUSEPORT overlap deploys (#37) keep
// working — during an overlap the two processes report each other until
// the old one drains.
//
// A best-effort process-table scan (ps) supplements heartbeat detection
// for instances old enough to predate heartbeats; it cannot prove a shared
// data_dir, so its peers carry their own Source label and the boot warning
// says so. Graceful exits remove the own heartbeat; crash leftovers age
// out via the TTL. POSIX-only, like the rest of the listener stack.
package server

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	peerHeartbeatPrefix = "onegw-peer-" // <pid>.json
	peerTTL             = 30 * time.Second
	peerRefresh         = 10 * time.Second
)

// Peer is one other live gateway process.
type Peer struct {
	PID    int    `json:"pid"`
	Listen string `json:"listen,omitempty"`
	// Source is "heartbeat" (file in THIS data_dir — authoritative) or
	// "process" (host process table; data_dir sharing unverified).
	Source string `json:"source"`
}

// ScanPeers returns the other live gateway processes for dataDir: fresh,
// alive heartbeats plus — deduplicated by pid — other onegw processes from
// the host process table. selfPID is excluded; pass 0 when the caller is
// not a gateway process. A missing or in-memory data_dir means no peers.
func ScanPeers(dataDir string, selfPID int, now time.Time) []Peer {
	if dataDir == "" || dataDir == "memory" {
		return nil
	}
	peers := scanHeartbeats(dataDir, selfPID, now)
	seen := make(map[int]bool, len(peers))
	for _, p := range peers {
		seen[p.PID] = true
	}
	for _, p := range scanProcessTable(selfPID) {
		if !seen[p.PID] {
			peers = append(peers, p)
		}
	}
	return peers
}

func heartbeatPath(dataDir string, pid int) string {
	return filepath.Join(dataDir, peerHeartbeatPrefix+strconv.Itoa(pid)+".json")
}

func scanHeartbeats(dataDir string, selfPID int, now time.Time) []Peer {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}
	var peers []Peer
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, peerHeartbeatPrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) >= peerTTL {
			continue // stale mtime: crash leftover or paused clock — not a peer
		}
		raw, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			continue
		}
		var doc heartbeatDoc
		if json.Unmarshal(raw, &doc) != nil || doc.PID <= 0 || doc.PID == selfPID || !pidAlive(doc.PID) {
			continue
		}
		peers = append(peers, Peer{PID: doc.PID, Listen: doc.Listen, Source: "heartbeat"})
	}
	return peers
}

// pidAlive probes pid existence with signal 0. EPERM counts as alive: the
// process exists but is owned by another user — still a gateway on a
// shared host.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// scanProcessTable lists other onegw processes on this host via ps(1).
// Best-effort: any error (missing ps, restricted view, non-POSIX) yields
// no peers.
func scanProcessTable(selfPID int) []Peer {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var peers []Peer
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidStr, cmd, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil || pid <= 0 || pid == selfPID || !looksLikeGateway(cmd) {
			continue
		}
		peers = append(peers, Peer{PID: pid, Source: "process"})
	}
	return peers
}

// looksLikeGateway reports whether a ps command line's argv[0] names an
// onegw binary ("onegw", "/tmp/onegw-live.new", ...). Arguments are
// ignored so an editor holding onegw.toml is not a peer.
func looksLikeGateway(cmd string) bool {
	argv0 := cmd
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		argv0 = cmd[:i]
	}
	return strings.Contains(filepath.Base(argv0), "onegw")
}

// livePeers caches the last scan result for /metrics; the watch loop is
// its only writer.
var livePeers atomic.Int64

// PeerCount reports the last observed number of other live gateways
// (0 before the first scan or without a PeerWatch).
func PeerCount() int { return int(livePeers.Load()) }

// PeerWatch refreshes this process's heartbeat file and the peer count.
type PeerWatch struct {
	stop    chan struct{}
	done    chan struct{}
	dataDir string
	selfPID int
}

// heartbeatDoc is the on-disk heartbeat payload. RefreshedAt is the
// refresh instant, not the process start.
type heartbeatDoc struct {
	PID         int       `json:"pid"`
	RefreshedAt time.Time `json:"refreshed_at"`
	Listen      string    `json:"listen"`
}

// StartPeerWatch writes the first heartbeat, takes the first peer count,
// and starts the refresh loop. Returns nil for in-memory data dirs
// ("", "memory"): no files to maintain, nothing to detect.
func StartPeerWatch(dataDir, listen string) *PeerWatch {
	if dataDir == "" || dataDir == "memory" {
		return nil
	}
	w := &PeerWatch{
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		dataDir: dataDir,
		selfPID: os.Getpid(),
	}
	w.writeHeartbeat(listen, time.Now())
	w.refresh()
	go w.loop(listen)
	return w
}

func (w *PeerWatch) loop(listen string) {
	defer close(w.done)
	ticker := time.NewTicker(peerRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case now := <-ticker.C:
			w.writeHeartbeat(listen, now)
			w.refresh()
		}
	}
}

// Stop halts the loop and removes the own heartbeat so a graceful exit
// leaves no file to age out.
func (w *PeerWatch) Stop() {
	if w == nil {
		return
	}
	close(w.stop)
	<-w.done
	_ = os.Remove(heartbeatPath(w.dataDir, w.selfPID))
}

func (w *PeerWatch) writeHeartbeat(listen string, now time.Time) {
	payload, err := json.Marshal(heartbeatDoc{PID: w.selfPID, RefreshedAt: now, Listen: listen})
	if err != nil {
		return
	}
	if err := os.MkdirAll(w.dataDir, 0o755); err != nil {
		log.Printf("onegw peers: heartbeat for data_dir %s: %v", w.dataDir, err)
		return
	}
	path := heartbeatPath(w.dataDir, w.selfPID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		log.Printf("onegw peers: heartbeat for data_dir %s: %v", w.dataDir, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("onegw peers: heartbeat for data_dir %s: %v", w.dataDir, err)
	}
}

func (w *PeerWatch) refresh() {
	livePeers.Store(int64(len(ScanPeers(w.dataDir, w.selfPID, time.Now()))))
}
