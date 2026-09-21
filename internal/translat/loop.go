package translat

import (
	"io"
	"strings"

	"onegw/internal/types"
)

// A degenerate generation ("reasoning loop") repeats a handful of blocks
// until the upstream's output cap ends it: free-tier models do it under
// load, the upstream never stops, and the client's only signal is a 32K
// `stopReason: length` wall of near-identical text.
//
// # What the live captures actually look like
//
// Two captures taken 2026-09-21 through the `free` combo
// (`dots-studio/dots-3-note-preview:free`, 327KB and 469KB) both burned
// the full 4000-token cap on a loop, but their repetition is NOT a clean
// cycle:
//
//   - The first repeats five deltas in strict rotation (~185x), so a
//     strict period scan finds it.
//   - The second shuffles ~50 short reasoning fragments for ~940 lines,
//     so at NO period does a window repeat 20 times back to back — yet in
//     its last 256 lines only 46 lines are distinct and the top eight
//     cover 84%. It is just as degenerate.
//
// Cycle detection therefore cannot be the signal: the robust one is
// DIVERSITY. A normal generation is ~all-distinct; a loop collapses to a
// small set of lines repeated many times. The guard examines the last
// loopWindow lines and trips when the window holds at least loopDistinct
// times more lines than distinct ones.
//
// Measured on the captures and on adversarial non-loops (a legitimate
// 300-row table, a line repeated 15x, 100 identical keepalive frames, a
// status line alternating with distinct prose): the threshold below fires
// at 23% and 53% of the two looping streams and never on the non-loops.
//
// ponytail: the window is the raw upstream wire split on newlines, so the
// signal assumes a line is a stable unit — true for SSE and NDJSON, where
// the JSON envelope's `id`/`created` are constant for the whole response.
// A vendor that varies a per-chunk field would raise the distinct count
// and hide the loop; the upgrade path is to decode each event through
// newStreamDecoder and window the text deltas instead.
//
// Historical note: the guard shipped in #122 never fired. It had zero
// callers (so it was dead code), its hashUnit ignored its offset argument
// (so only the first window was ever computed), it probed periods
// {1,2,4,8,16} (the live cycle is 5), and it counted stride-1 neighbours,
// which no multi-line cycle can ever satisfy.
const (
	loopWindow   = 128 // lines examined at the tail
	loopDistinct = 4   // window must be >= this many times the distinct count
	loopMinBytes = 8 << 10
)

// loopErrorType is the APIError type for a detected reasoning loop.
const loopErrorType = "upstream_reasoning_loop"

// LoopError is the APIError returned when the guard trips. StreamCommitted
// is always true: a loop only becomes visible after KBs of repeated
// output, by which time headers and text are already on the wire.
var LoopError = func() *types.APIError {
	e := errAPI(502, loopErrorType, "upstream reasoning loop detected: the same line(s) repeated past the threshold")
	e.StreamCommitted = true
	return e
}()

// Looped reports whether the guard has tripped.
func (b *LoopBreaker) Looped() bool { return b.looped != nil }

// LoopError returns the terminal error the guard will surface; nil until
// the guard trips.
func (b *LoopBreaker) LoopError() *types.APIError { return b.looped }

// LoopBreaker watches the upstream wire for a degenerate repeating
// generation and closes the upstream body when it trips, so a looping
// model stops burning output into a client that has already been handed a
// wall of repeats.
type LoopBreaker struct {
	r       io.Reader
	body    io.Closer
	pending string // trailing partial line, carried into the next Read
	lines   []string
	bytes   int
	counts  map[string]int // reused per check
	looped  *types.APIError
}

// NewLoopBreaker wraps r, which must be the reader the relay copies from;
// body is the upstream response body, closed to abort a looping stream.
func NewLoopBreaker(r io.Reader, body io.Closer) *LoopBreaker {
	return &LoopBreaker{r: r, body: body}
}

func (b *LoopBreaker) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.observe(string(p[:n]))
	}
	if err != nil {
		return n, err
	}
	if b.looped != nil {
		return n, io.EOF
	}
	return n, nil
}

// observe appends the chunk's complete lines to the history and re-checks
// the tail. A trailing partial line is carried into the next chunk so a
// line is never cut in half.
func (b *LoopBreaker) observe(chunk string) {
	if b.looped != nil {
		return
	}
	s := b.pending + chunk
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		b.pending = s[i+1:]
		s = s[:i]
	} else {
		b.pending = s
		return
	}
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			b.lines = append(b.lines, line)
			b.bytes += len(line) + 1
		}
	}
	b.tryTrip()
}

// tryTrip trips when the tail window has collapsed to a small set of
// heavily repeated lines. An ongoing loop is always at the tail, so the
// scan stays bounded and can run on every chunk.
func (b *LoopBreaker) tryTrip() {
	n := len(b.lines)
	if b.bytes < loopMinBytes || n < loopWindow {
		return
	}
	if b.counts == nil {
		b.counts = make(map[string]int, loopWindow)
	} else {
		clear(b.counts)
	}
	for _, line := range b.lines[n-loopWindow:] {
		b.counts[line]++
	}
	if len(b.counts)*loopDistinct <= loopWindow {
		b.looped = LoopError
		_ = b.body.Close()
	}
}

// errAPI is the server-level APIError constructor (declared here so the
// guard package stays free of an import cycle with internal/server).
func errAPI(status int, typ, msg string) *types.APIError {
	return &types.APIError{Status: status, Type: typ, Message: msg}
}
