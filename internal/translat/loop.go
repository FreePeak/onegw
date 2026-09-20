package translat

import (
	"fmt"
	"io"
	"strings"

	"hash/maphash"

	"onegw/internal/types"
)

// A degenerate generation ("reasoning loop") repeats the same one or two
// lines until the upstream's output cap ends it: free-tier models do it
// under load, the upstream never stops, and the client's only signal is a
// 32K `stopReason: length` wall of identical sentences. Two live incidents
// repeated `Build passes. Now let me update the tests.` 2721x (120KB) and a
// 2-line comment block 1057x (157KB).
//
// The repetition is visible ONLY in the decoded text. Upstream chunks cut
// it into sub-word deltas (`Build` / ` passes` / `.`), and a captured
// 2.29MB stream of the exact incident contained zero repeated byte
// sequences and zero repeated consecutive events — so neither a byte-level
// scan nor a per-event scan can see it. Everything below therefore
// accumulates decoded text and looks for a line cycle.
//
// Tunables. loopMinReps and loopMinBytes make a trip mean "the
// model has already burned multiple KB re-emitting one block".
// ponytail: sub-word fragmentation yields tiny cycles — 2 lines.
const (
	loopMinReps  = 20
	loopMinUnit  = 24
	loopMinBytes = 8 << 10
	loopMaxPeriod = 16
)

// loopErrorType is the APIError type for a detected reasoning loop.
const loopErrorType = "upstream_reasoning_loop"

// IsLoopError reports whether err is a detected-reasoning-loop APIError.
func IsLoopError(err error) bool {
	if err == nil {
		return false
	}
	apiErr, ok := err.(*types.APIError)
	return ok && apiErr.Type == loopErrorType
}

// LoopError is the APIError returned when the guard trips. Its
// StreamCommitted field is always true (headers and a wall of repeated
// text are already on the wire by the time the guard fires).
var LoopError = func() *types.APIError {
	e := errAPI(502, loopErrorType, "upstream reasoning loop detected: the same line(s) repeated past the threshold")
	e.StreamCommitted = true
	return e
}()

// Looped reports whether the guard has tripped on b's decoded output.
func (b *LoopBreaker) Looped() bool { return b.looped != nil }

// LoopError returns the terminal error the guard will surface; nil until the
// guard trips.
func (b *LoopBreaker) LoopError() *types.APIError { return b.looped }

// textSample is one observation channel of decoded text for cycle detection.
type textSample struct {
	seed   maphash.Seed
	lines  []string
	ndjson bool // command-code NDJSON: lines are JSON objects, split on \n
}

// LoopBreaker watches decoded text for a repeating cycle of lines: free-tier
// reasoning loops repeat the same one or two sentences until the upstream
// output cap ends them. It wraps the relay's read source so it sees exactly
// what the client receives, and closes the upstream body when it trips so the
// upstream stops burning output into a client that has already been told.
type LoopBreaker struct {
	r      io.Reader
	body   io.Closer
	dec    streamDecoder
	ndjson bool // command-code NDJSON: lines are JSON objects, split on \n

	text  textSample // decoded text (split into lines, full history)
	think textSample // full line buffer (raw line text)

	looped *types.APIError
}

// NewLoopBreaker wraps r, which must be the reader the relay copies from;
// body is the upstream response body, closed to abort a looping stream.
func NewLoopBreaker(r io.Reader, body io.Closer, from Format) *LoopBreaker {
	dec, _ := newStreamDecoder(from)
	b := &LoopBreaker{r: r, body: body, dec: dec, ndjson: from == FmtCommandCode}
	b.text.seed = maphash.MakeSeed()
	b.think.seed = maphash.MakeSeed()
	b.text.ndjson = b.ndjson
	b.think.ndjson = b.ndjson
	return b
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

// observe feeds a raw chunk of upstream bytes to the guard. Decoded text is
// split into lines and hashed; if any window of 1..loopMaxPeriod consecutive
// identical lines repeats loopMinReps times (enough bytes, min unit length),
// the guard trips and closes the upstream body.
func (b *LoopBreaker) observe(chunk string) {
	lines := strings.Split(chunk, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		b.think.push(line)
		b.text.push(line)
	}
	b.tryTrip()
}

func (s *textSample) push(line string) {
	if s.ndjson {
		for _, l := range strings.Split(line, "\n") {
			if l != "" {
				s.pushRaw(l)
			}
		}
		return
	}
	s.pushRaw(line)
}

func (s *textSample) pushRaw(line string) {
	s.lines = append(s.lines, line)
}

func (b *LoopBreaker) tryTrip() {
	lines := b.text.lines
	if len(lines) < 32 {
		return
	}
	bytes := 0
	for _, l := range lines {
		bytes += len(l)
	}
	if bytes < loopMinBytes {
		return
	}
	// Check every window length 1..loopMaxPeriod for a long run of
	// identical windows. For each length, hash the first window, then
	// scan the rest of the history counting consecutive matches.
	for _, period := range []int{1, 2, 4, 8, 16} {
		if period > len(lines)/loopMinReps {
			continue
		}
		unit := hashUnit(b.text.seed, lines, 0, period)
		if unit == 0 {
			continue
		}
		reps := countCycle(b.text.seed, lines, period, unit)
		if reps >= loopMinReps {
			b.looped = LoopError
			_ = b.body.Close()
			return
		}
	}
}

// hashUnit returns a normalized key for the window of `period` lines
// starting at offset 0 in lines.
func hashUnit(seed maphash.Seed, lines []string, offset, period int) uint64 {
	h := maphash.Hash{}
	h.SetSeed(seed)
	for i := 0; i < period; i++ {
		h.Write([]byte(lines[i]))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// countCycle scans the full history for the longest run of consecutive
// windows of `period` lines that hash to `unit`.
func countCycle(seed maphash.Seed, lines []string, period int, unit uint64) int {
	if len(lines) < period {
		return 0
	}
	best := 0
	cur := 0
	limit := len(lines) - period + 1
	for i := 0; i < limit; i++ {
		h := maphash.Hash{}
		h.SetSeed(seed)
		for j := 0; j < period; j++ {
			h.Write([]byte(lines[i+j]))
			h.Write([]byte{0})
		}
		if h.Sum64() == unit {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return best
}

// errAPI is the server-level APIError constructor (declared here so the
// guard package stays free of an import cycle with internal/server).
func errAPI(status int, typ, msg string) *types.APIError {
	return &types.APIError{Status: status, Type: typ, Message: msg}
}

var _ = fmt.Sprintf
