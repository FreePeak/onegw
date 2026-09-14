package server

import (
	"fmt"
	"io"
	"sync"
	"time"

	"onegw/internal/translat"
)

// upstreamBodyIdleLimit bounds silence from an upstream that already sent
// response headers. Who it actually catches: half-open http/1.1 (or
// ALPN-downgraded) connections — a laptop suspend leaves those producing
// zero bytes and zero errors for minutes (no read timer in net/http; OS
// retransmit budget) — and live-but-frozen upstream applications. Dead h2
// connections are reaped by the transport's ping loop in ~45s; this is the
// backstop under that, and it bounds the buffered/aggregate body reads
// too. The timer must arm at headers, not at first byte: the post-suspend
// stall is precisely headers-then-never-anything.
//
// Trade-off (accepted): a healthy stream silent >90s mid-response (deep
// non-ping upstreams) is cut with a terminal error frame and one bounded
// retry, rather than pinning the budget reservation, the provider
// concurrency slot and possibly an idempotency entry behind it forever.
var upstreamBodyIdleLimit = 90 * time.Second

// idleBreakReader closes body when no byte has been read through it for
// upstreamBodyIdleLimit; every successful read re-arms the timer.
type idleBreakReader struct {
	mu    sync.Mutex
	r     io.Reader
	body  io.Closer
	timer *time.Timer // non-nil while armed; nil once stopped or fired
}

func newIdleBreak(r io.Reader, body io.Closer) io.Reader {
	i := &idleBreakReader{r: r, body: body}
	i.touch()
	return i
}

// kill is the AfterFunc callback: the window elapsed without a byte, so
// the (possibly half-open post-suspend) upstream body is force-closed —
// the blocked Read returns an error and the relay ends the stream.
func (i *idleBreakReader) kill() {
	i.mu.Lock()
	i.timer = nil
	i.mu.Unlock()
	_ = i.body.Close()
}

func (i *idleBreakReader) touch() {
	i.mu.Lock()
	if i.timer == nil {
		i.timer = time.AfterFunc(upstreamBodyIdleLimit, i.kill)
	} else {
		i.timer.Reset(upstreamBodyIdleLimit)
	}
	i.mu.Unlock()
}

func (i *idleBreakReader) stop() {
	i.mu.Lock()
	if i.timer != nil {
		i.timer.Stop()
		i.timer = nil
	}
	i.mu.Unlock()
}

func (i *idleBreakReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if err != nil {
		// Stream settled (EOF or upstream death): the relay's deferred
		// Close owns the body from here.
		i.stop()
		return n, err
	}
	i.touch()
	return n, err
}

// flushWriter flushes after every Write. The same-format passthrough must
// not hold live stream bytes in net/http's ~4KiB write buffer: a healthy
// trickle would then reach the client in lumps with minute-scale gaps, and
// the client's byte-level watchdog cannot tell that from a dead stream.
type flushWriter struct {
	w     io.Writer
	flush func()
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.flush()
	return n, err
}

// writeStreamTerminalError ends a committed SSE stream with an explicit
// error frame where the client format has one, so a mid-relay upstream
// death reads as a named failure instead of a silent truncation. Formats
// without a defined SSE error shape just get the close (their relays have
// already logged and returned an APIError).
func writeStreamTerminalError(w io.Writer, flush func(), f translat.Format, msg string) {
	switch f {
	case translat.FmtAnthropic:
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":%q}}\n\n", msg)
	case translat.FmtOpenAI:
		fmt.Fprintf(w, "data: {\"error\":{\"type\":\"upstream_error\",\"message\":%q}}\n\n", msg)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	flush()
}
