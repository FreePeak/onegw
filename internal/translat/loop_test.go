package translat

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// chunkedReader hands out at most n bytes per Read, mirroring the buffer
// io.Copy uses in relayResponse: the guard is tail-anchored (an ongoing
// loop is always at the tail), so a test that handed the whole stream over
// in one Read would measure a different thing.
type chunkedReader struct {
	data []byte
	n    int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(c.data) {
		n = len(c.data)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// sseChunk renders one OpenAI-style SSE delta, the shape the live captures
// carried (`dots-studio/dots-3-note-preview:free` on the `free` combo).
func sseChunk(content string) string {
	return fmt.Sprintf("data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q,\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n", content)
}

func runGuard(t *testing.T, stream string) *LoopBreaker {
	t.Helper()
	src := &chunkedReader{data: []byte(stream), n: 4096}
	lb := NewLoopBreaker(src, io.NopCloser(strings.NewReader("")))
	n, _ := io.Copy(io.Discard, lb)
	t.Logf("relayed %d of %d bytes", n, len(stream))
	return lb
}

// TestLoopBreakerTripsOnStrictCycle covers the first live capture: five
// distinct deltas in strict rotation, with a reasoning prelude in front.
func TestLoopBreakerTripsOnStrictCycle(t *testing.T) {
	block := []string{
		".\nBuild passes.",
		" Now let me update",
		" the tests.\nBuild",
		" passes. Now let",
		" me update the tests",
	}
	var b strings.Builder
	for i := range 14 {
		b.WriteString(sseChunk(fmt.Sprintf("unique opening line %d", i)))
	}
	for range 80 {
		for _, l := range block {
			b.WriteString(sseChunk(l))
		}
	}
	lb := runGuard(t, b.String())
	if !lb.Looped() {
		t.Fatal("guard did not trip on a strict 5-line cycle")
	}
	if e := lb.LoopError(); e == nil || e.Type != loopErrorType {
		t.Fatalf("want %s, got %+v", loopErrorType, e)
	}
	if e := lb.LoopError(); !e.StreamCommitted {
		t.Fatal("a tripped loop is always mid-stream: StreamCommitted must be true")
	}
}

// TestLoopBreakerTripsOnLooseCycle covers the second live capture, where
// the model shuffled ~50 short fragments instead of rotating a fixed
// block. No window repeats back to back at any period there, so only a
// diversity signal can see it.
func TestLoopBreakerTripsOnLooseCycle(t *testing.T) {
	const frags = 20
	block := make([]string, frags)
	for i := range frags {
		block[i] = fmt.Sprintf("fragment %d of the repeated reasoning", i)
	}
	var b strings.Builder
	for i := range 400 {
		// Deterministic scrambled order: every 20 lines are a permutation
		// of the block, so no fixed window repeats but the set does not grow.
		b.WriteString(sseChunk(block[(i*7)%frags]))
	}
	lb := runGuard(t, b.String())
	if !lb.Looped() {
		t.Fatal("guard did not trip on a loosely ordered 20-fragment loop")
	}
}

// TestLoopBreakerIgnoresNormalStream is the false-positive guard: a long,
// varied generation must never be aborted.
func TestLoopBreakerIgnoresNormalStream(t *testing.T) {
	var b strings.Builder
	for i := range 600 {
		b.WriteString(sseChunk(fmt.Sprintf("line %d: the model is writing distinct prose here, %d tokens in", i, i*7)))
	}
	if lb := runGuard(t, b.String()); lb.Looped() {
		t.Fatal("guard tripped on a normal varied stream")
	}
}

// TestLoopBreakerIgnoresSparseRepeat pins that ordinary duplicated lines
// (the shape of real code output) do not trip it.
func TestLoopBreakerIgnoresSparseRepeat(t *testing.T) {
	var b strings.Builder
	for i := range 400 {
		switch {
		case i%20 == 0:
			b.WriteString(sseChunk("}"))
		case i%13 == 0:
			b.WriteString(sseChunk("return nil"))
		default:
			b.WriteString(sseChunk(fmt.Sprintf("func handler%d(ctx context.Context) error {", i)))
		}
	}
	if lb := runGuard(t, b.String()); lb.Looped() {
		t.Fatal("guard tripped on normally duplicated code lines")
	}
}

// TestLoopBreakerNeedsMinimumEvidence pins the floor: a short stream that
// happens to repeat is left alone, so a preamble cannot abort a response.
func TestLoopBreakerNeedsMinimumEvidence(t *testing.T) {
	var b strings.Builder
	for range 40 {
		b.WriteString(sseChunk("Build passes. Now let me update the tests."))
	}
	if lb := runGuard(t, b.String()); lb.Looped() {
		t.Fatal("guard tripped below the minimum-evidence floor")
	}
}

// TestLoopBreakerClosesUpstreamOnTrip proves the upstream is torn down: the
// point of the guard is to stop the model burning output, not merely to
// notice afterwards.
func TestLoopBreakerClosesUpstreamOnTrip(t *testing.T) {
	closed := false
	body := closerFunc(func() error { closed = true; return nil })
	var b strings.Builder
	for range 200 {
		b.WriteString(sseChunk("Build passes. Now let me update the tests."))
	}
	lb := NewLoopBreaker(&chunkedReader{data: []byte(b.String()), n: 4096}, body)
	_, _ = io.Copy(io.Discard, lb)
	if !lb.Looped() {
		t.Fatal("guard did not trip")
	}
	if !closed {
		t.Fatal("upstream body was not closed on trip")
	}
}

// TestLoopBreakerCarriesPartialLines pins that a chunk boundary inside a
// line does not split it into two bogus ones.
func TestLoopBreakerCarriesPartialLines(t *testing.T) {
	src := &chunkedReader{data: []byte("data: one\n\ndata: two\n\ndata: three\n\n"), n: 7}
	lb := NewLoopBreaker(src, io.NopCloser(strings.NewReader("")))
	_, _ = io.Copy(io.Discard, lb)
	want := []string{"data: one", "data: two", "data: three"}
	if len(lb.lines) != len(want) {
		t.Fatalf("want %d lines %q, got %d %q", len(want), want, len(lb.lines), lb.lines)
	}
	for i := range want {
		if lb.lines[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i, want[i], lb.lines[i])
		}
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
