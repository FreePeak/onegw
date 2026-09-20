package translat

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// junkBytes is the byte shape the live vendor spliced into its reasoning
// deltas (2026-09-20, free lane through Novita): a 2-byte head whose tail is
// not a continuation byte, a valid 3-byte head with the same defect, and a
// bare continuation byte. None of these can come out of a working decoder.
var junkBytes = []byte{0xc3, 0x28, 0xe2, 0x28, 0xa1}

// reasoningEvent mirrors the captured SSE shape (61 KB / 185 events): every
// reasoning delta carries an EMPTY content string plus the fragment in both
// "reasoning" and reasoning_details.
func reasoningEvent(frag []byte) []byte {
	s := string(frag)
	return []byte(`data: {"id":"1","choices":[{"delta":{"content":"","reasoning":"` +
		s + `","reasoning_details":[{"type":"reasoning.text","text":"` + s + `"}]}}]}` + "\n\n")
}

// chunkReader hands the stream out in fixed-size pieces so a rune can be
// split across reads, the way an upstream splits it across TCP chunks.
type chunkReader struct {
	data []byte
	size int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.size
	if n > len(c.data) {
		n = len(c.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// The live shape: empty-content reasoning filler that carries raw invalid
// bytes. Not one byte may reach the client — the guard holds the head and
// answers Prefetch with a failover-capable verdict (Router.Execute then
// falls through to the next combo target and the pair is benched).
func TestCorruptGuardFailsOverReasoningJunk(t *testing.T) {
	stream := append(reasoningEvent(junkBytes), reasoningEvent([]byte("clean"))...)
	g := NewCorruptGuard(bytes.NewReader(stream), 32<<10)

	err := g.Prefetch()
	if err == nil {
		t.Fatal("Prefetch() = nil, want a corrupt-stream verdict")
	}
	if err.Status != 502 || err.Type != corruptErrorType || !err.CorruptStream {
		t.Fatalf("verdict = %d/%s CorruptStream=%v, want 502/%s true",
			err.Status, err.Type, err.CorruptStream, corruptErrorType)
	}
	if g.Junk() == nil {
		t.Fatal("Junk() = nil, want the recorded verdict")
	}
}

// Empty content is filler, not output: a long run of it must keep holding the
// head, so junk that follows inside the same window is still caught before
// the client sees anything.
func TestCorruptGuardKeepsHoldingEmptyContentPrefix(t *testing.T) {
	var stream []byte
	for range 8 {
		stream = append(stream, reasoningEvent([]byte("still thinking"))...)
	}
	stream = append(stream, reasoningEvent(junkBytes)...)
	g := NewCorruptGuard(bytes.NewReader(stream), 32<<10)

	if err := g.Prefetch(); err == nil {
		t.Fatal("Prefetch() = nil, want the verdict after an empty-content prefix")
	}
}

// Once real output has been released the client is reading this stream: a
// later verdict is recorded for the metrics only and must not be returned as
// failover-capable (relayResponse keeps relaying the attempt's bytes).
func TestCorruptGuardDoesNotFailOverAfterRelease(t *testing.T) {
	prefix := []byte(`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n")
	g := NewCorruptGuard(io.MultiReader(
		bytes.NewReader(prefix),
		bytes.NewReader(reasoningEvent(junkBytes)),
	), 32<<10)

	if err := g.Prefetch(); err != nil {
		t.Fatalf("Prefetch() = %v, want nil once real content released the hold", err)
	}
	if _, err := io.ReadAll(g); err != nil {
		t.Fatal(err)
	}
	if j := g.Junk(); j == nil || !j.CorruptStream {
		t.Fatalf("Junk() = %v, want the post-release verdict recorded", j)
	}
}

// An upstream may split a character across chunks; the incomplete tail is
// carried into the next read, so a valid rune split byte-by-byte must not
// read as corrupt — and the relayed bytes must survive intact.
func TestCorruptGuardCarriesSplitRune(t *testing.T) {
	stream := reasoningEvent([]byte("chữ “nghiệm” 🙂"))
	g := NewCorruptGuard(&chunkReader{data: stream, size: 1}, 32<<10)

	if err := g.Prefetch(); err != nil {
		t.Fatalf("Prefetch() = %v, want nil for a valid stream", err)
	}
	got, err := io.ReadAll(g)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, stream) {
		t.Fatalf("relayed %q, want %q", got, stream)
	}
	if j := g.Junk(); j != nil {
		t.Fatalf("Junk() = %v, want nil for a valid stream", j)
	}
}

// hold == 0 is the scan-only mode the fast path and single-target routes use:
// no sibling target exists, so there is nothing to fail over to — the verdict
// is still recorded for the metrics.
func TestCorruptGuardScanOnlyRecordsWithoutFailover(t *testing.T) {
	g := NewCorruptGuard(bytes.NewReader(reasoningEvent(junkBytes)), 0)

	if err := g.Prefetch(); err != nil {
		t.Fatalf("Prefetch() = %v, want nil in scan-only mode", err)
	}
	if _, err := io.ReadAll(g); err != nil {
		t.Fatal(err)
	}
	if j := g.Junk(); j == nil || !j.CorruptStream {
		t.Fatalf("Junk() = %v, want the verdict recorded in scan-only mode", j)
	}
}

// errReader hands the payload out together with err, then reports EOF — the
// shape of a body whose connection died on its last chunk.
type errReader struct {
	data []byte
	err  error
	done bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, io.EOF
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	if len(e.data) == 0 {
		e.done = true
		return n, e.err
	}
	return n, nil
}

// A reader may return bytes AND an error in the same call. The guard must not
// swallow it: an upstream that dies mid-stream has to read as a failure, not
// as a clean end of stream (the relay reports truncation from that error).
func TestCorruptGuardPropagatesErrorWithBytes(t *testing.T) {
	stream := reasoningEvent([]byte("tail"))
	g := NewCorruptGuard(&errReader{data: stream, err: io.ErrUnexpectedEOF}, 0)

	got, err := io.ReadAll(g)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadAll error = %v, want io.ErrUnexpectedEOF", err)
	}
	if !bytes.Equal(got, stream) {
		t.Fatalf("relayed %d bytes, want %d", len(got), len(stream))
	}
}
