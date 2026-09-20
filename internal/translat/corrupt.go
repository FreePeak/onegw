package translat

import (
	"bytes"
	"fmt"
	"io"
	"unicode/utf8"

	"onegw/internal/types"
)

// A corrupt upstream stream carries bytes no working model decoder can emit:
// the vendor splices invalid UTF-8 inside its SSE JSON strings, so Go's
// encoding/json substitutes U+FFFD on decode and the client renders mojibake
// (the "weird characters" a thinking box shows). Live incident 2026-09-20:
// the `free` lane (kilocode -> openrouter -> Novita, serving
// inclusionai/ling-3.0-flash-vl:free) put raw invalid bytes into 4 of 4
// persisted junk reasoning blocks — 14 U+FFFD in one 288-char block, 7 in a
// 3343-char block, plus script-soup text around them. The mechanism is the
// VENDOR's own decode (a mismatched tokenizer emits byte garbage), so the
// corruption is visible at the exact byte the wire carries.
//
// Why bytes and not decoded text: every client in the path (the gateway and
// its callers) silently substitutes U+FFFD, so a text-level scan cannot tell
// a corrupt wire from a model that deliberately emitted U+FFFD. A byte-level
// UTF-8 test CAN: no working provider emits invalid bytes, a rune split
// across HTTP chunks is reassembled by the carry below, and JSON's own
// escaping means a rune split across SSE *events* never reaches the wire as a
// half sequence.
//
// Deliberate coverage ceiling (see the ponytail note on CorruptGuard): junk
// that is VALID UTF-8 — a model emitting U+FFFD tokens of its own — does not
// trip this guard. The observed class is a broken upstream decode, which is
// always invalid bytes; the repetition class is LoopBreaker's job (loop.go).

// corruptErrorType is the APIError type for a corrupt upstream stream.
const corruptErrorType = "upstream_stream_corrupt"

// corruptChunkBytes is the read size the guard fills its hold window with.
// One read per ~16KB keeps the pre-release cost to a couple of reads even
// when the upstream dribbles single SSE events.
const corruptChunkBytes = 16 << 10

// releaseMarkers end the hold early: the attempt has produced output the
// client can use, so a later verdict could no longer replace the attempt
// anyway. They are the JSON keys/type tags of translated text deltas across
// the wire formats this gateway fronts (OpenAI tool calls, Anthropic
// text_delta/input_json_delta). Reasoning deltas deliberately do NOT appear
// here: every one of the vendor's junk events carried empty `"content":""`
// plus a `reasoning_details` entry, so a reasoning-only prefix must keep
// holding — that shape is exactly what a corrupt stream looks like.
var releaseMarkers = [][]byte{
	[]byte(`"tool_calls"`),
	[]byte(`"text_delta"`),
	[]byte(`"partial_json"`),
}

// contentKey is the OpenAI-wire content key. The trailing quote is what makes
// the marker below precise: `"content":""` is the empty filler the corrupt
// vendor emits on every reasoning event, while `"content":"x` is real output.
var contentKey = []byte(`"content":"`)

// releasedByMarker reports whether held already holds usable output.
func releasedByMarker(held []byte) bool {
	for _, m := range releaseMarkers {
		if bytes.Contains(held, m) {
			return true
		}
	}
	// `"content":"` followed by anything but a quote is a NON-EMPTY content
	// string. The colon+quote requirement also keeps `"content":null`,
	// `"content":{...}` and `"content_filter_results"` out.
	for i := 0; ; {
		j := bytes.Index(held[i:], contentKey)
		if j < 0 {
			return false
		}
		k := i + j + len(contentKey)
		if k >= len(held) {
			// The chunk ended mid-key: the value's first byte has not
			// arrived, so the marker is undecided. Keep holding.
			return false
		}
		if held[k] != '"' {
			return true
		}
		i = k
	}
}

// CorruptGuard wraps the relay's upstream read source, validating that the
// byte stream is UTF-8 and holding its head back until either the attempt
// proves it produced usable output or the hold window (hold bytes) is spent.
//
// A verdict reached while the head is still held is FAILOVER-capable: not one
// byte has been written to the client, so the caller may discard the attempt
// and let Router.Execute fall through to the next combo target. Once the hold
// is released (marker, window spent, or upstream end) a later verdict is
// recorded on Junk for the metrics only — the bytes are already on the wire
// and the relay must not abort a stream the client is reading.
//
// hold == 0 makes the guard scan-only: no buffering, no delay, verdicts go to
// Junk but never reach Prefetch. The fast path and single-target routes use
// that mode: with no sibling target to fall through to, holding the head
// would buy latency and nothing else.
//
// ponytail: the guard validates the raw wire, so junk that is valid UTF-8
// (a model emitting U+FFFD of its own, or a provider that pre-decodes bytes
// before framing them) is not caught — extend with a decoded-text check if a
// live incident ever shows that shape. A gzip/binary response body would also
// read as "invalid UTF-8", which is why relayResponse only wraps text
// responses (see textResponse there).
type CorruptGuard struct {
	r    io.Reader
	hold int

	chunk   []byte
	scratch []byte
	held    []byte
	carry   []byte

	scanned int64 // bytes of r consumed by observe
	verdict *types.APIError
	dead    bool

	err        error
	done       bool
	released   bool
	prefetched bool
}

// NewCorruptGuard wraps r. r must be the reader the relay copies from, so the
// guard sees exactly the bytes the client would receive.
func NewCorruptGuard(r io.Reader, hold int) *CorruptGuard {
	g := &CorruptGuard{r: r, hold: hold}
	if hold > 0 {
		g.chunk = make([]byte, corruptChunkBytes)
	}
	return g
}

// Prefetch fills the hold window and returns a verdict that arrived before any
// byte was released, or nil when the relay may proceed. It is idempotent; a
// non-nil result means the caller must NOT write a response body and should
// return the error as this attempt's failure (Router.Execute turns it into a
// combo fall-through).
func (g *CorruptGuard) Prefetch() *types.APIError {
	if g.prefetched {
		if g.verdict != nil && !g.released {
			return g.verdict
		}
		return nil
	}
	g.prefetched = true
	if g.hold <= 0 {
		return nil
	}
	for !g.released && g.verdict == nil {
		if g.done || g.err != nil {
			g.released = true // upstream ended inside the window
			break
		}
		g.fill()
		if g.verdict != nil {
			break // verdict wins: the window was never released
		}
		if len(g.held) >= g.hold || releasedByMarker(g.held) {
			g.released = true
		}
	}
	if g.verdict != nil && !g.released {
		return g.verdict
	}
	return nil
}

// Junk reports the verdict the guard recorded, whether or not it arrived in
// time to fail over. nil while the stream is clean.
func (g *CorruptGuard) Junk() *types.APIError { return g.verdict }

// Read drains the held head and then relays the upstream. A verdict recorded
// after the release does NOT abort: the client is already reading this stream.
func (g *CorruptGuard) Read(p []byte) (int, error) {
	if !g.prefetched {
		if v := g.Prefetch(); v != nil {
			return 0, v
		}
	}
	if len(g.held) > 0 {
		n := copy(p, g.held)
		g.held = g.held[n:]
		return n, nil
	}
	if g.err != nil {
		err := g.err
		g.err = nil
		return 0, err
	}
	if g.done {
		return 0, io.EOF
	}
	n, err := g.r.Read(p)
	if n > 0 {
		g.observe(p[:n])
		// Hold the error back for the next call instead of dropping it:
		// the caller must drain these bytes first (io.Reader contract),
		// and swallowing a truncation error would turn a dead upstream
		// into a clean EOF.
		if err != nil {
			g.err = err
		}
		return n, nil
	}
	return n, err
}

// fill reads one upstream chunk, validates it and appends it to the hold
// window. It reports nothing: done/err carry the terminal state.
func (g *CorruptGuard) fill() {
	n, err := g.r.Read(g.chunk)
	if n > 0 {
		g.observe(g.chunk[:n])
		g.held = append(g.held, g.chunk[:n]...)
	}
	if err != nil {
		if err == io.EOF {
			g.done = true
		} else {
			g.err = err
		}
	}
}

// observe validates a raw chunk of upstream bytes and records the absolute
// offset of the first invalid byte. Bytes of an incomplete trailing rune are
// carried into the next call: an upstream is free to split a character across
// TCP chunks.
func (g *CorruptGuard) observe(b []byte) {
	if g.dead {
		return
	}
	start := g.scanned - int64(len(g.carry)) // absolute offset of scratch[0]
	g.scratch = append(append(g.scratch[:0], g.carry...), b...)
	g.scanned += int64(len(b))
	i := 0
	for i < len(g.scratch) {
		r, size := utf8.DecodeRune(g.scratch[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		// r == RuneError && size == 1: an invalid byte, or the head of a
		// sequence whose remaining bytes have not arrived yet.
		if incompleteRune(g.scratch[i:]) {
			break
		}
		g.verdict = &types.APIError{
			Status:        502,
			Type:          corruptErrorType,
			Message:       fmt.Sprintf("upstream emitted invalid UTF-8 at byte %d of the response stream", start+int64(i)),
			CorruptStream: true,
		}
		g.dead = true
		g.carry = g.carry[:0]
		return
	}
	// Keep at most the incomplete tail (<= 3 bytes: incompleteRune only
	// reports that when fewer bytes than one sequence are left).
	g.carry = append(g.carry[:0], g.scratch[i:]...)
}

// incompleteRune reports whether b is the valid head of a UTF-8 sequence that
// has not fully arrived yet, as opposed to genuinely invalid bytes. Go's
// DecodeRune already rejects overlong forms, surrogates and > U+10FFFF, so a
// sequence of full length that DecodeRune refused is invalid, not incomplete.
func incompleteRune(b []byte) bool {
	need := 0
	switch c := b[0]; {
	case c >= 0xC2 && c <= 0xDF:
		need = 2
	case c >= 0xE0 && c <= 0xEF:
		need = 3
	case c >= 0xF0 && c <= 0xF4:
		need = 4
	default:
		return false // 0x80-0xC1, 0xF5-0xFF are never a valid head
	}
	if len(b) >= need {
		return false
	}
	for _, c := range b[1:] {
		if c&0xC0 != 0x80 {
			return false
		}
	}
	return true
}
