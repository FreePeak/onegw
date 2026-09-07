// Push ships flushed usage buckets to a remote aggregator after each local
// flush. Fire-and-forget by design: at most one retry, failures logged once
// per outage episode, and the flush loop never waits on the network — it
// only hands the buckets over and returns.
package usage

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// Pusher posts JSONL rows (RollupRow shape — Bucket fields plus "node") to
// the aggregator's import endpoint. Same wire format the
// /admin/usage/export and /admin/usage/import endpoints speak.
type Pusher struct {
	client *http.Client
	url    string
	apiKey string
	nodeID string

	mu       sync.Mutex
	inflight bool
	// pending holds buckets from windows that finished flushing while a
	// push was in flight, summed per key (memory O(distinct keys), like the
	// tracker itself). The delivery goroutine drains it after the current
	// push, so no window is ever dropped.
	pending   []rollupWire
	droppedAt time.Time
}

// NewPusher returns a Pusher posting JSONL rows to url with the given admin
// password (X-Admin-Password header) and node id. nil url disables it.
func NewPusher(url, adminPassword, nodeID string) *Pusher {
	if url == "" {
		return nil
	}
	return &Pusher{
		client: &http.Client{Timeout: 10 * time.Second},
		url:    url,
		apiKey: adminPassword,
		nodeID: nodeID,
	}
}

// AfterFlush hands the just-flushed buckets to the pusher. Non-blocking:
// work happens on the delivery goroutine, never inside the flush loop.
func (p *Pusher) AfterFlush(buckets []Bucket) {
	if p == nil || len(buckets) == 0 {
		return
	}
	rows := make([]rollupWire, len(buckets))
	for i := range buckets {
		rows[i] = rollupWire{Node: p.nodeID, Bucket: buckets[i]}
	}
	p.mu.Lock()
	if p.inflight {
		p.pending = sumRows(p.pending, rows)
		p.mu.Unlock()
		return
	}
	p.inflight = true
	p.mu.Unlock()
	go p.worker(rows)
}

// worker delivers batches back to back until no pending window remains.
func (p *Pusher) worker(rows []rollupWire) {
	for {
		p.deliver(rows)
		p.mu.Lock()
		if len(p.pending) == 0 {
			p.inflight = false
			p.mu.Unlock()
			return
		}
		rows = p.pending
		p.pending = nil
		p.mu.Unlock()
	}
}

// deliver POSTs one batch, retrying once. A final failure is logged once
// per outage episode: the first failure logs, later ones stay quiet until a
// push succeeds again.
func (p *Pusher) deliver(rows []rollupWire) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Header line first: tells the aggregator this is a delta stream (sum
	// per key+node) and attributes it to this node.
	if err := enc.Encode(pushHeader{Node: p.nodeID, Kind: "delta"}); err != nil {
		return
	}
	for i := range rows {
		if err := enc.Encode(rows[i]); err != nil {
			return // unreachable for this value shape; stay quiet
		}
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ { // one retry
		req, err := http.NewRequest(http.MethodPost, p.url, bytes.NewReader(buf.Bytes()))
		if err != nil {
			lastErr = err
			break
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		if p.apiKey != "" {
			req.Header.Set("X-Admin-Password", p.apiKey)
		}
		resp, err := p.client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				p.mu.Lock()
				p.droppedAt = time.Time{}
				p.mu.Unlock()
				return
			}
			lastErr = &statusError{code: resp.StatusCode}
			resp.Body.Close()
		} else {
			lastErr = err
		}
		if attempt == 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	p.mu.Lock()
	first := p.droppedAt.IsZero()
	p.droppedAt = time.Now()
	p.mu.Unlock()
	if first {
		log.Printf("usage push to %s failed, continuing without push until it recovers: %v", p.url, lastErr)
	}
}

// sumRows merges src counters into dst per (node, key).
func sumRows(dst, src []rollupWire) []rollupWire {
	idx := make(map[Key]int, len(dst))
	for i := range dst {
		idx[dst[i].Key] = i
	}
	for _, r := range src {
		if j, ok := idx[r.Key]; ok {
			dst[j].Requests += r.Requests
			dst[j].InputTokens += r.InputTokens
			dst[j].OutputTokens += r.OutputTokens
			dst[j].CacheRead += r.CacheRead
			dst[j].CacheWrite += r.CacheWrite
			dst[j].Reasoning += r.Reasoning
			dst[j].SavedTokens += r.SavedTokens
			if dst[j].Estimated != r.Estimated {
				dst[j].Estimated = true
			}
			if r.FirstSeen.After(dst[j].FirstSeen) {
				dst[j].FirstSeen = r.FirstSeen // keep widest window
			}
			if r.LastSeen.After(dst[j].LastSeen) {
				dst[j].LastSeen = r.LastSeen
			}
			continue
		}
		idx[r.Key] = len(dst)
		dst = append(dst, r)
	}
	return dst
}

// pushHeader is the first line of every pushed batch.
type pushHeader struct {
	Node string `json:"node"`
	Kind string `json:"kind"`
}

// statusError keeps the HTTP failure terse in logs.
type statusError struct{ code int }

func (e *statusError) Error() string {
	return http.StatusText(e.code)
}

// rollupWire mirrors store.RollupRow without importing the store package
// (store already imports usage — no cycle).
type rollupWire struct {
	Node string `json:"node"`
	Bucket
}
