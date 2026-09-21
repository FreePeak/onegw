package translat

import (
	"bytes"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"onegw/internal/types"
)

// A junk reasoning block is text an upstream MODEL never wrote: the vendor's
// decode fell apart and burned its output budget into symbol soup. The client
// renders it in its thinking box as mojibake — the "weird characters" report
// from the field — and the run then usually ends with no answer at all,
// because the leg spent its whole output cap on the garbage.
//
// Where this sits between the other two guards:
//
//   - CorruptGuard (corrupt.go) owns the WIRE: bytes that are not valid UTF-8.
//     It sees the vendor's broken decode before JSON escaping can hide it.
//
//   - LoopBreaker (loop.go) owns REPETITION: a generation that recycles a
//     small set of lines until the cap.
//
//   - This guard owns WORD-STRUCTURE: valid UTF-8 text whose tokens stopped
//     being words. Live specimens (2026-09-21, the free lane):
//
//     ',kenO.RH*是RO儿*!m"-WR*WJUNTOS/O *!! _____. 2469-0"V|.\n) Conclusions\n
//     %k;**U 5*W}D?,0e a\n,T^!>:+<Bl!*ZEr ... :!  HOLA?f S-T.@#$!!!!*"0>'
//
//     '. vvvvvvvvvvvvvvvvvvv,vvvvvvvvvvvvvvvv, To preserve.
//     ,,,,,,,,,,,,,,,,,,,, ,,,,\n]]]\n############...
//     ,0\t,,,,,,,.\treturnt,,,.!\n<b> .\nFVTT,,,,,,,,,,,,'
//
// Measured against 25k sampled real reasoning blocks (>= 400 bytes), the rule
// below fires on 5 — all five genuine junk blocks — for a 0.02% flag rate,
// while hex dumps, code fences, tables, and CJK/Cyrillic/Vietnamese prose do
// not trip either test.
//
// Deliberate coverage ceiling: junk whose LETTERS still sit in word-like runs
// while its tokens mix scripts and embedded symbols (the pasted report's
// first half, `*** Begin 拳击,   Boxing.""</-Re*=bC([R3]****** Репозиторий`)
// measures like bilingual reasoning — English/Chinese/Vietnamese thinking
// mixes scripts constantly — so no cheap lexical test separates those two.
// The symbol-soup half IS caught, and the wire-level guard still owns the
// invalid-byte variant of the same corruption. Extend the shapes here if a
// live incident shows a third class.
const (
	// junkMinBytes is the floor below which a block is never judged: short
	// reasoning preambles ("Let me check the parser…") carry no signal, and
	// junk burns a whole output cap, so it is always long.
	junkMinBytes = 600

	// junkWindowBytes bounds the text kept for judging (the tail of the
	// reasoning so far). Junk is sustained, so the tail is representative and
	// the memory cost stays flat on a runaway stream.
	junkWindowBytes = 64 << 10

	// junkHoldBytes is the head of the wire held back while the verdict is
	// still open, so a verdict is FAILOVER-capable: the relay has written
	// nothing and Router.Execute can drop the attempt and re-call the model.
	junkHoldBytes = 32 << 10
)

// junkErrorType is the APIError type for a junk-reasoning stream.
const junkErrorType = "upstream_reasoning_junk"

// junkTokenRatio is the alphanumeric share below which a token is soup
// rather than a word (`,` `,,,` `!m"-WR*` `===](#!`).
const junkTokenRatio = 0.6

// junkError is the terminal error a trip surfaces. StreamCommitted stays
// FALSE while the head was still held — that is the whole point of the hold:
// the caller may discard the attempt and re-call the model on the next combo
// target. A late trip (after the release) is reported through Junk for the
// metrics only, like a late corrupt verdict.
var junkError = func() *types.APIError {
	e := errAPI(502, junkErrorType,
		"upstream reasoning junk: the model's output degenerated into symbol soup")
	e.JunkReasoning = true
	return e
}()

// junkStats measures the three inputs to the verdict over s:
//
//	wordR     letters inside a multi-letter run / all letters. Prose keeps
//	          its letters in words (~1.0); a broken decode scatters them.
//	badR      fraction of word-ish tokens whose alphanumerics are a minority
//	          of the token length. A hash, version or identifier is
//	          alphanumeric and counts as clean.
//	symPer100 runs of two or more non-word non-space characters, per 100 bytes.
//
// A rune is "word content" when it is a letter or a digit in ANY script;
// underscores are ignored (they join identifiers, they are not symbols). That
// choice is what keeps non-Latin reasoning — the CJK letters and Cyrillic in
// the specimens' own neighbourhood — on the clean side of the verdict.
func junkStats(s string) (wordR, badR, symPer100 float64) {
	var letters, inWords, symBytes int
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case isWordRune(r):
			run := 0
			j := i
			for j < len(s) {
				r2, s2 := utf8.DecodeRuneInString(s[j:])
				if !isWordRune(r2) {
					break
				}
				run++
				j += s2
			}
			letters += run
			if run >= 2 {
				inWords += run
			}
			i = j
		case isSymbolRune(r):
			run := 0
			j := i
			for j < len(s) {
				r2, s2 := utf8.DecodeRuneInString(s[j:])
				if !isSymbolRune(r2) {
					break
				}
				run++
				j += s2
			}
			if run >= 2 {
				symBytes += run
			}
			i = j
		default:
			i += size
		}
	}

	var tokens, badTokens int
	for _, tok := range strings.Fields(s) {
		if utf8.RuneCountInString(tok) < 4 {
			continue
		}
		alnum, runes := 0, 0
		for _, r := range tok {
			runes++
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				alnum++
			}
		}
		tokens++
		// A token with no alphanumerics at all (`,,,,`, `####`, `!!`) is the
		// purest soup there is, so it counts as bad rather than being skipped.
		// Skipping those was the bug that let the comma storms through: the
		// stream is nothing BUT punctuation, which left tokens==0 and made the
		// ratio indistinguishable from "no signal".
		if alnum == 0 || float64(alnum)/float64(runes) < junkTokenRatio {
			badTokens++
		}
	}

	wordR = 1
	if letters > 0 {
		wordR = float64(inWords) / float64(letters)
	}
	badR = 1
	if tokens > 0 {
		badR = float64(badTokens) / float64(tokens)
	}
	symPer100 = float64(symBytes) / float64(max(1, len(s))) * 100
	return wordR, badR, symPer100
}

// isWordRune reports whether r counts as word content (a letter or digit of
// any script).
func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// isSymbolRune reports whether r can be part of a symbol run: neither word
// content, nor whitespace, nor an identifier joiner.
func isSymbolRune(r rune) bool {
	if r == '_' || isWordRune(r) || unicode.IsSpace(r) {
		return false
	}
	// Combining marks ride along with the letter they modify; treating them
	// as symbols would turn every accented Vietnamese word into a run.
	return !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Mc, r)
}

// junkReasoning reports whether the reasoning text accumulated so far is
// junk. Two independently-sufficient shapes, both measured on the corpus:
//
//	(a) word letters collapsed AND most tokens are punctuation
//	    (wordR < 0.92 && badR >= 0.25) — the vvvvvvv / comma-storm class
//	(b) most tokens are punctuation AND symbol runs dominate the bytes
//	    (badR >= 0.40 && symPer100 >= 10) — the mixed-script soup class
//
// Deliberately narrow (high precision, low recall): a false trip kills a real
// answer, so the rule only fires on text that has stopped being language.
func junkReasoning(s string) bool {
	if len(s) < junkMinBytes {
		return false
	}
	wordR, badR, symPer100 := junkStats(s)
	if (wordR < 0.92 && badR >= 0.25) || (badR >= 0.40 && symPer100 >= 10) {
		return true
	}
	// (c) Fragmentation: the text is broken into many short pieces with few
	// letters in each, and symbol runs are dense. The mixed-script specimen
	// lands here (2.1 letters per space-separated fragment, ~12 symbol bytes
	// per 100) while English, Mandarin, Vietnamese, code and commit-hash
	// reasoning all keep >= 4.2 letters per fragment — measured on the
	// corpus, see the doc comment on junkStats.
	return meanTokenLetters(s) < 2.6 && symPer100 >= 10
}

// meanTokenLetters is the average letter count of the space-separated
// fragments in s. Junk fragments are two or three letters glued to symbols
// ('aszil,我们都是从这里开始', 'ZEr', '$""O7ade!z*ll*'); language keeps its
// letters in words.
func meanTokenLetters(s string) float64 {
	var letters, toks int
	for _, tok := range strings.Fields(s) {
		n := 0
		for _, r := range tok {
			if unicode.IsLetter(r) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		toks++
		letters += n
	}
	if toks == 0 {
		return 0
	}
	return float64(letters) / float64(toks)
}

// junkDeltaSpan returns the index just past the closing quote of a JSON string
// whose opening quote is at open. ok=false when the chunk ends first (the value
// continues in the next read), which is what makes the caller carry it.
func junkDeltaSpan(chunk []byte, open int) (int, bool) {
	for j := open + 1; j < len(chunk); j++ {
		switch chunk[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// junkDeltaText extracts the reasoning text a wire chunk carries, so the
// guard can judge DECODED text rather than bytes. Every vendor alias is read
// (reasoning_content native, reasoning / reasoning_text aliases — the same
// set translat.reasoningEcho accepts on replay).
//
// A chunk boundary can fall inside a reasoning value; the guard then sees a
// truncated fragment, which only ever makes the accumulated text SHORTER than
// reality. Junk is sustained over KBs, so a missed fragment cannot hide it,
// and the next chunk contributes its own prefix.
func junkDeltaText(chunk []byte) string {
	var sb strings.Builder
	for _, key := range []string{`"reasoning_content":"`, `"reasoning":"`, `"reasoning_text":"`} {
		i := 0
		for {
			j := bytes.Index(chunk[i:], []byte(key))
			if j < 0 {
				break
			}
			open := i + j + len(key) - 1
			end, ok := junkDeltaSpan(chunk, open)
			if !ok {
				break
			}
			sb.Write(chunk[open+1 : end-1])
			i = end
		}
	}
	return sb.String()
}

// JunkGuard wraps the relay's upstream read source and holds its head while
// the reasoning it has seen is still too short to judge, so a junk verdict
// lands BEFORE the first client byte and the attempt can be replaced.
//
// Flow: Prefetch fills the hold window and returns a verdict that arrived
// while nothing had been released; Read then drains the held head and relays
// normally. hold == 0 makes the guard scan-only (no buffering, no latency),
// which is what a route with no sibling target gets: the verdict is still
// recorded for the dashboard, it just cannot replace the attempt.
type JunkGuard struct {
	r    io.Reader
	hold int

	chunk []byte
	held  []byte
	// carry is the tail of the previous chunk: a reasoning JSON string can
	// straddle two reads, and only a rejoined window yields its value.
	// Bounded (see junkCarryBytes) so a chunk with no newline cannot grow it
	// without limit.
	carry []byte

	reasoning string // decoded reasoning text seen so far (bounded tail)
	sawText   bool   // usable output appeared: stop judging, release the head

	verdict    *types.APIError
	err        error
	done       bool
	released   bool
	prefetched bool
}

// clearReasoning proves the verdict can be reached from realistic chunks: it
// is the guard's own smoke test hook, used by the relay's server-level tests
// to assert the wiring without depending on the exact shape the upstream
// emitted. Nothing else calls it.

// junkCarryBytes bounds the rejoining tail. Live SSE events are ~330-800B, so
// one event's worth is plenty; anything larger is a malformed stream and the
// guard stops trying to resync it.
const junkCarryBytes = 8 << 10

// NewJunkGuard wraps r, the reader the relay copies from.
func NewJunkGuard(r io.Reader, hold int) *JunkGuard {
	g := &JunkGuard{r: r, hold: hold}
	if hold > 0 {
		g.chunk = make([]byte, corruptChunkBytes)
	}
	return g
}

// Prefetch fills the hold window and returns a verdict that arrived before any
// byte was released, or nil when the relay may proceed.
func (g *JunkGuard) Prefetch() *types.APIError {
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
			break
		}
		if len(g.held) >= g.hold || g.sawText {
			g.released = true
		}
	}
	if g.verdict != nil && !g.released {
		return g.verdict
	}
	return nil
}

// Junk reports the verdict recorded so far; nil while the stream is clean.
func (g *JunkGuard) Junk() *types.APIError { return g.verdict }

// Read drains the held head and then relays the upstream. A verdict recorded
// after the release does NOT abort: the client is already reading this stream.
func (g *JunkGuard) Read(p []byte) (int, error) {
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
		// Hold the error back for the next call, like CorruptGuard: the
		// caller must drain these bytes first, and swallowing a truncation
		// error would turn a dead upstream into a clean EOF.
		if err != nil {
			g.err = err
		}
		return n, nil
	}
	return n, err
}

func (g *JunkGuard) fill() {
	n, err := g.r.Read(g.chunk)
	if n > 0 {
		g.held = append(g.held, g.chunk[:n]...)
		g.observe(g.chunk[:n])
	}
	if err != nil {
		if err == io.EOF {
			g.done = true
		} else {
			g.err = err
		}
	}
}

// observe folds one upstream chunk into the running reasoning text and
// re-checks the verdict. It stops judging the moment usable output appears
// (content or a tool call): from then on the attempt is answerable and must
// not be discarded.
func (g *JunkGuard) observe(b []byte) {
	if g.verdict != nil || g.sawText {
		return
	}
	if releasedByMarker(b) {
		g.sawText = true
		g.reasoning = ""
		g.carry = g.carry[:0]
		return
	}
	// Rejoin the carried tail: a reasoning value split across two reads only
	// decodes from the concatenation.
	window := b
	if len(g.carry) > 0 {
		joined := make([]byte, 0, len(g.carry)+len(b))
		joined = append(joined, g.carry...)
		joined = append(joined, b...)
		window = joined
	}
	if t := junkDeltaText(window); t != "" {
		g.reasoning = junkTail(g.reasoning + t)
	}
	// Carry the tail that may hold the start of an unterminated reasoning
	// value, so the next read can finish it.
	carry := window
	if i := bytes.LastIndex(window, []byte(`"reasoning`)); i >= 0 {
		carry = window[i:]
	} else {
		carry = window[len(window):]
	}
	if len(carry) > junkCarryBytes {
		carry = carry[len(carry)-junkCarryBytes:]
	}
	g.carry = append(g.carry[:0], carry...)
	if junkReasoning(g.reasoning) {
		g.verdict = junkError
	}
}

// junkTail bounds the text kept for judging.
func junkTail(s string) string {
	if len(s) <= junkWindowBytes {
		return s
	}
	return s[len(s)-junkWindowBytes:]
}

// JunkReasoningForTest exposes the verdict rule to sibling packages' tests
// (internal/server asserts that the fixture it serves actually reaches it).
// Production callers hold a guard instead; nothing routes through this.
func JunkReasoningForTest(s string) bool { return junkReasoning(s) }
