package saver

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"onegw/internal/translat"
)

// Loss-behavior regression suite for the RTK-style compressor.
//
// Two groups, both backed by runtime probes against the real filters
// (2026-09-07 deep-dive, PRD analysis):
//   - "Safe" tests pin guarantees: the generic path never truncates
//     well-formed distinct lines, dedup runs before truncate so unique
//     mid-file errors survive identical-line logs, and every filter
//     never grows its input.
//
//   - "Loss profile" tests pin the documented lossy behavior: truncating
//     filters (log/grep/tree) keep head+tail and replace the middle with
//     a labeled elision marker, and long lines are cut at max_line with
//     a byte-count marker. The elided content is unrecoverable — that is
//     the accepted RTK trade. If filter routing changes, update these
//     intentionally, not silently.

func lossFixtureLogDistinct(n int) string {
	var sb strings.Builder
	for i := range n {
		sb.WriteString(fmt.Sprintf("2026-09-07T12:%02d:%02dZ INFO request id=%d processed bytes=%d latency=%dms\n", i/60, i%60, i, i*17, i%9))
	}
	lines := strings.Split(strings.TrimSuffix(sb.String(), "\n"), "\n")
	lines[n/2] = "2026-09-07T12:04:10Z ERROR connection pool exhausted for user=u77"
	return strings.Join(lines, "\n")
}

func lossFixtureLogIdentical(n int) string {
	var sb strings.Builder
	for range n {
		sb.WriteString("2026-09-07T12:00:00Z INFO worker tick ok\n")
	}
	lines := strings.Split(strings.TrimSuffix(sb.String(), "\n"), "\n")
	lines[n/2] = "2026-09-07T12:04:11Z ERROR database connection pool exhausted"
	return strings.Join(lines, "\n")
}

func lossFixtureGrepLongLine() string {
	var sb strings.Builder
	for i := range 50 {
		sb.WriteString(fmt.Sprintf("pkg/f%d.go:%d: short hit %d\n", i, i, i))
	}
	sb.WriteString("pkg/big.go:1: " + strings.Repeat(`{"field":"value"},`, 30) + `,"fatal":"disk full"` + "\n")
	return sb.String()
}

func lossFixtureTree(n int) string {
	var sb strings.Builder
	sb.WriteString("total " + fmt.Sprint(n) + "\n")
	for i := range n {
		sb.WriteString(fmt.Sprintf("drwxr-xr-x 2 u g 4096 Sep  7 12:0%d dir_%03d\n", i%10, i))
	}
	return sb.String()
}

func lossFixtureStatus(n int) string {
	var sb strings.Builder
	sb.WriteString("On branch main\nYour branch is up to date with 'origin/main'.\n\n")
	for range n {
		sb.WriteString("\tmodified:   pkg/a.go\n")
	}
	return sb.String()
}

func lossFixtureJSON(keys int, errorKey int) string {
	var sb strings.Builder
	sb.WriteString("{\n")
	for i := range keys {
		if i == errorKey {
			sb.WriteString(fmt.Sprintf("  \"key_%03d\": \"FATAL: quota exceeded\",\n", i))
		} else {
			sb.WriteString(fmt.Sprintf("  \"key_%03d\": \"value with some text %d\",\n", i, i))
		}
	}
	sb.WriteString("  \"done\": true\n}\n")
	return sb.String()
}

// --- Safe guarantees ------------------------------------------------------

// The generic path must not truncate distinct, well-formed lines: an
// important fact in the middle of unrecognized tool output survives.
func TestSafeGenericPathLossless(t *testing.T) {
	s := New(Config{Enabled: true})
	var lines []string
	for range 300 {
		lines = append(lines, fmt.Sprintf("step %d ok", len(lines)))
	}
	lines[260] = "FATAL: migration failed: duplicate column users.email"
	in := strings.Join(lines, "\n")
	if got := s.Compress(in); got != in {
		t.Fatalf("generic path mutated distinct lines: in=%d out=%d", len(in), len(got))
	}
}

// Dedup runs BEFORE truncate in the log filter, so a unique error line
// between two identical runs survives even in a huge log.
func TestSafeDedupLogKeepsMidFileError(t *testing.T) {
	s := New(Config{Enabled: true})
	in := lossFixtureLogIdentical(400)
	out := s.Compress(in)
	if !strings.Contains(out, "ERROR database connection pool exhausted") {
		t.Fatalf("mid-file unique error lost:\n%s", out)
	}
	if !strings.Contains(out, "×") {
		t.Fatalf("identical run not collapsed with count:\n%s", out)
	}
	if len(out) >= len(in) {
		t.Fatalf("no savings: in=%d out=%d", len(in), len(out))
	}
}

// Trailing matches are the common case for grep output (last hits = most
// recent); the tail window must keep them.
func TestSafeGrepKeepsTailMatch(t *testing.T) {
	s := New(Config{Enabled: true})
	var sb strings.Builder
	for i := range 300 {
		sb.WriteString(fmt.Sprintf("pkg/file%d.go:%d: return value%d\n", i, i*3, i))
	}
	sb.WriteString("pkg/secret.go:42: API_KEY=sk-live-abcdef\n301 matches\n")
	in := sb.String()
	out := s.Compress(in)
	if !strings.Contains(out, "API_KEY=sk-live-abcdef") {
		t.Fatalf("tail match lost:\n%s", out)
	}
	if len(out) >= len(in) {
		t.Fatalf("no savings: in=%d out=%d", len(in), len(out))
	}
}

// Status output keeps the branch summary and collapses the repeated
// entries to one line + count.
func TestSafeStatusCollapse(t *testing.T) {
	s := New(Config{Enabled: true})
	in := lossFixtureStatus(300)
	out := s.Compress(in)
	if !strings.Contains(out, "On branch main") {
		t.Fatalf("branch summary lost:\n%s", out)
	}
	if !strings.Contains(out, "×") {
		t.Fatalf("repeated entries not collapsed:\n%s", out)
	}
	if len(out) >= len(in) {
		t.Fatalf("no savings: in=%d out=%d", len(in), len(out))
	}
}

// --- Documented loss profile ----------------------------------------------

// Truncating filters replace the middle with a labeled marker. The elided
// content (here: a mid-file error in a distinct-line log) is unrecoverable
// from the compressed view — known RTK trade. What the contract guarantees
// is that the loss is LABELED and bounded to the middle window.
func TestLossProfileTruncationIsLabeled(t *testing.T) {
	s := New(Config{Enabled: true})
	cases := map[string]string{
		"log":  lossFixtureLogDistinct(400),
		"tree": lossFixtureTree(500),
	}
	for name, in := range cases {
		out := s.Compress(in)
		if len(out) >= len(in) {
			t.Fatalf("%s: no savings: in=%d out=%d", name, len(in), len(out))
		}
		if !strings.Contains(out, "lines elided]") {
			t.Fatalf("%s: middle elided without marker:\n%s", name, out)
		}
	}
	// First and last entries must survive (head+tail windows).
	treeOut := s.Compress(cases["tree"])
	if !strings.Contains(treeOut, "dir_000") || !strings.Contains(treeOut, "dir_499") {
		t.Fatalf("head/tail entries lost:\n%s", treeOut)
	}
}

// Long lines are cut at max_line bytes with a byte-count marker; anything
// past the cut point is unrecoverable (e.g. the tail of a one-line JSON
// payload). The contract: the cut is always labeled with the elided size.
func TestLossProfileLongLineCutIsLabeled(t *testing.T) {
	s := New(Config{Enabled: true})
	in := lossFixtureGrepLongLine()
	out := s.Compress(in)
	if len(out) >= len(in) {
		t.Fatalf("no savings: in=%d out=%d", len(in), len(out))
	}
	if !strings.Contains(out, "[+") {
		t.Fatalf("long-line cut without byte marker:\n%s", out)
	}
}

// Pretty JSON sniffs as grep (colon-heavy) and takes the truncating path;
// only keys inside the head+tail windows survive. Pinned so a routing fix
// flips this test deliberately.
func TestLossProfileJSONTakesTruncatingPath(t *testing.T) {
	s := New(Config{Enabled: true})
	in := lossFixtureJSON(200, 100) // error key inside the head window
	out := s.Compress(in)
	if len(out) >= len(in) {
		t.Fatalf("no savings: in=%d out=%d", len(in), len(out))
	}
	if !strings.Contains(out, "key_100") {
		t.Fatalf("head-window key lost:\n%s", out)
	}
	if !strings.Contains(out, "lines elided]") {
		t.Fatalf("middle elided without marker:\n%s", out)
	}
}

// --- Cross-cutting invariants ----------------------------------------------

// Every filter must be idempotent and deterministic: the client replays the
// full history every turn and the gateway re-compresses it, so turn N and
// turn N+1 must produce byte-identical tool results (also what keeps the
// upstream prompt-cache prefix stable).
func TestStableAcrossTurns(t *testing.T) {
	s := New(Config{Enabled: true})
	fixtures := map[string]string{
		"log-distinct":  lossFixtureLogDistinct(400),
		"log-identical": lossFixtureLogIdentical(400),
		"grep-long":     lossFixtureGrepLongLine(),
		"tree":          lossFixtureTree(500),
		"status":        lossFixtureStatus(300),
		"json":          lossFixtureJSON(200, 100),
	}
	for name, in := range fixtures {
		once := s.Compress(in)
		twice := s.Compress(once)
		if once != twice {
			t.Fatalf("%s: not idempotent", name)
		}
		if again := s.Compress(in); again != once {
			t.Fatalf("%s: not deterministic", name)
		}
	}
}

// No filter may ever grow its input, whatever the content.
func TestNeverGrowsAllFilters(t *testing.T) {
	s := New(Config{Enabled: true})
	fixtures := map[string]string{
		"log-distinct":  lossFixtureLogDistinct(400),
		"log-identical": lossFixtureLogIdentical(400),
		"grep-long":     lossFixtureGrepLongLine(),
		"tree":          lossFixtureTree(500),
		"status":        lossFixtureStatus(300),
		"json":          lossFixtureJSON(200, 100),
	}
	for name, in := range fixtures {
		if got := s.Compress(in); len(got) > len(in) {
			t.Fatalf("%s: output grew: %d > %d", name, len(got), len(in))
		}
	}
}

// --- Accounting & encoding contracts (fixed 2026-09-07, were defects) ------

// Savings reported to /admin/usage must be the TRUE wire reduction, never a
// per-string estimate. Regressed on 2026-09-07: per-string charsSaved deltas
// were summed while json.Marshal HTML-escaped <,>,& into \u003c… across EVERY
// string, so the body GREW 9511→17797 bytes on tag-heavy traffic while the
// counter reported 542 tokens saved. Fixed by SetEscapeHTML(false),
// whole-body never-grow gate, and wire-delta accounting.
func TestAccountingMatchesWireReduction(t *testing.T) {
	s := New(Config{Enabled: true})
	sys := "Rules: keep <b>bold</b> &amp; <i>italic</i>. " + strings.Repeat("<tag>&amp;</tag> ", 400)
	tool := strings.Repeat(`<div class=row>&amp;<span>data</span></div>`, 60)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` + sys +
		`"},{"role":"tool","content":"` + tool + `"}]}`)
	out, saved := s.ApplyRaw(translat.FmtOpenAI, body)
	if saved <= 0 {
		t.Fatal("expected compression on a compressible body")
	}
	if got := int64(len(body)-len(out)) / 4; saved != got {
		t.Fatalf("accounting drifted from wire truth: saved=%d, wire/4=%d (body %d -> out %d)",
			saved, got, len(body), len(out))
	}
	if len(out) > len(body) {
		t.Fatalf("wire grew: %d -> %d", len(body), len(out))
	}
	if !strings.Contains(string(out), "<tag>") || strings.Contains(string(out), `\u003c`) {
		t.Fatal("HTML-escaping crept back into the re-encode")
	}
}

// A net-negative re-encode must pass the original body through and report
// nothing — the old code shipped wire growth counted as savings. The one
// inflation source left post-SetEscapeHTML(false): raw invalid UTF-8 bytes
// coerce to 3-byte U+FFFD each during decode, so when that inflation
// outweighs the compression the body must not be re-encoded at all.
func TestNetNegativeReencodePassesThrough(t *testing.T) {
	s := New(Config{Enabled: true})
	// Tool line compresses (~2.4k→~410 bytes). The untouched system string
	// is 4000 invalid continuation bytes → decode coerces each to 3-byte
	// U+FFFD (+8000 bytes). Net wire delta is strongly negative.
	tool := strings.Repeat("tick ok ", 300) // one 2400-byte line → cut
	sys := strings.Repeat("\x80", 4000)     // invalid UTF-8, legal JSON chars
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` + sys +
		`"},{"role":"tool","content":"` + tool + `"}]}`)
	out, saved := s.ApplyRaw(translat.FmtOpenAI, body)
	if saved != 0 {
		t.Fatalf("net-negative re-encode reported savings: saved=%d (body %d -> out %d)", saved, len(body), len(out))
	}
	if string(out) != string(body) {
		t.Fatalf("net-negative body was re-encoded: in=%d out=%d", len(body), len(out))
	}
}

// A long line cut must never split a multi-byte rune: the byte-slice cut
// (line[:max]) emitted invalid UTF-8 for box-drawing/CJK/emoji content,
// which the re-encode then rewrote as U+FFFD mojibake. Fixed 2026-09-07 by
// backing the cut off to the last rune boundary.
func TestLongLineCutIsRuneSafe(t *testing.T) {
	s := New(Config{Enabled: true})
	in := "data: " + strings.Repeat("│", 200) // 600 bytes / 200 runes, one line
	out := s.Compress(in)
	if !utf8.ValidString(out) {
		t.Fatalf("cut split a multi-byte rune: %q", out[:60])
	}
	if !strings.Contains(out, "[+") {
		t.Fatalf("byte marker missing:\n%s", out)
	}
}
