package usage

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"onegw/internal/types"
)

// syncBuffer is a goroutine-safe log sink: the pusher logs from its own
// goroutine while the test polls the text.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An unreachable aggregator must never slow the flush loop: flushes keep
// their cadence while every push fails, and the failure is logged once per
// outage episode (not once per window).
func TestPushFailureDoesNotSlowFlushLoop(t *testing.T) {
	sink := &sinkStub{}
	tr := New(sink, 20*time.Millisecond, NewPusher("http://127.0.0.1:1/admin/usage/import", "pw", "node-x"))
	defer tr.Stop()

	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(log.Default().Writer())

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		tr.Observe(Key{Provider: "p", Model: "m", APIKey: "k"}, types.Usage{InputTokens: 10, OutputTokens: 4}, 0)
		time.Sleep(20 * time.Millisecond)
	}
	if flushes := sink.count(); flushes < 10 {
		t.Fatalf("flush loop slowed by failing push: only %d flushes in 400ms", flushes)
	}
	// The pusher retries once after 500ms and only logs after the retry —
	// poll for it beyond the flush-cadence window.
	logDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(logDeadline) && !strings.Contains(logBuf.String(), "usage push") {
		time.Sleep(50 * time.Millisecond)
	}
	if n := strings.Count(logBuf.String(), "usage push"); n != 1 {
		t.Fatalf("push failures must log once per outage episode, got %d lines: %q",
			n, logBuf.String())
	}
}

// A success between failures must reset the once-per-episode suppression so
// a later outage is visible in the log again.
func TestPushLogResetsAfterSuccess(t *testing.T) {
	hits := make(chan struct{}, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(200)
	}))

	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(log.Default().Writer())

	p := NewPusher(up.URL, "pw", "node-x")
	k := Key{Provider: "p", Model: "m", APIKey: "k", Day: "2026-09-07", Hour: "10"}
	p.AfterFlush([]Bucket{{Key: k, Requests: 1}})
	select {
	case <-hits:
	case <-time.After(2 * time.Second):
		t.Fatal("push never reached the aggregator")
	}

	// Kill the aggregator and flush again: connection refused now, and the
	// failure must be logged despite the earlier episode being suppressed.
	up.Close()
	p.AfterFlush([]Bucket{{Key: k, Requests: 2}})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "usage push") {
		time.Sleep(25 * time.Millisecond)
	}
	if !strings.Contains(logBuf.String(), "usage push") {
		t.Fatal("post-success failure must be logged again")
	}
}

// Wire format: pushed rows are flat JSONL, node-attributed, admin-gated.
func TestPushBodyIsNodeAttributedJSONL(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	var ctype, pw string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		body = append(body[:0], buf[:n]...)
		ctype = r.Header.Get("Content-Type")
		pw = r.Header.Get("X-Admin-Password")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer up.Close()

	p := NewPusher(up.URL, "pw", "node-x")
	p.AfterFlush([]Bucket{{
		Key:      Key{Provider: "p", Model: "m", APIKey: "k", Day: "2026-09-07", Hour: "10"},
		Requests: 2, InputTokens: 30,
	}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(body)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(body) == 0 {
		t.Fatal("no push body captured")
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("header + one bucket line, got %d: %q", len(lines), body)
	}
	if ctype != "application/x-ndjson" {
		t.Fatalf("content type = %q", ctype)
	}
	if pw != "pw" {
		t.Fatalf("admin password header = %q", pw)
	}
	if !strings.Contains(lines[0], `"kind":"delta"`) || !strings.Contains(lines[0], `"node":"node-x"`) {
		t.Fatalf("header line wrong: %s", lines[0])
	}
	for _, want := range []string{`"day":"2026-09-07"`, `"hour":"10"`, `"requests":2`, `"provider":"p"`} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("row line missing %s: %s", want, lines[1])
		}
	}
}
