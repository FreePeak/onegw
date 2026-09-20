package saver

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/translat"
)

// flipFixture builds an OpenAI body: system + user turn + tool turn. Only
// the first message participates in the conversation key, so changing the
// tool content across turns keeps the same conversation. Key order
// deliberately matches a client's natural order (model first) so the
// canonical re-encode is distinguishable from the raw form.
func flipFixture(sys, user, tool string) []byte {
	return []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":` + q(sys) + `},` +
		`{"role":"user","content":` + q(user) + `},` +
		`{"role":"tool","content":` + q(tool) + `}]}`)
}

func q(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// bigTool is deterministic compressible content (dedup collapses the
// identical lines).
func bigTool(n int) string {
	line := "step done: handled request with payload 0000000000000000000000000000000000000000\n"
	return strings.Repeat(line, n)
}

// shortTool is under the 256-byte compress floor: it can never shrink,
// so a turn carrying it has zero aggregate savings.
const shortTool = "done"

// belowFloorTool returns text whose unfloored filter still shrinks it
// but whose savings percentage falls under the 5% floor, i.e. exactly
// the block Compress rejects on its own. Built by growing the
// incompressible majority around a small compressible head — a handful
// of probes, each linear.
func belowFloorTool(t *testing.T, s *Saver) string {
	t.Helper()
	// Unique 200-byte lines containing "error" (sniffs as log, survives
	// dedup) wrapped around a small collapsible head; growing the
	// incompressible majority pushes the relative savings below the 5%
	// floor while the unfloored filter still shrinks the text.
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("error line " + itoa(i) + " " + strings.Repeat("padding ", 24) + "\n")
	}
	incompressible := sb.String()
	for pad := 1; pad < 64; pad++ {
		text := bigTool(6) + strings.Repeat(incompressible, pad)
		if s.Compress(text) == text {
			if un := s.compressFiltered(text, s.settings()); un != "" && len(un) < len(text) {
				return text
			}
			t.Fatal("fixture stopped shrinking even without the floor")
		}
	}
	t.Fatal("could not push the block below the savings floor")
	return ""
}

// toolContentOf extracts the tool message's content string from a
// canonical OpenAI body.
func toolContentOf(t *testing.T, body []byte) string {
	t.Helper()
	var root struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range root.Messages {
		if m.Role == "tool" {
			return m.Content
		}
	}
	t.Fatal("no tool message")
	return ""
}

// TestStickyCanonicalFormAcrossTurns reproduces issue #35: turn 1 has a
// big compressible tool result, so the body goes out in canonical
// (re-encoded) form and the upstream implicit cache keys on those bytes.
// Turn 2 has no compressible content at all — the pre-fix gate flipped
// the whole body back to the client's raw key order, busting the entire
// prefix. The gate must stay canonical: turn 2 keeps turn 1's byte
// prefix and never reverts to the raw client form.
func TestStickyCanonicalFormAcrossTurns(t *testing.T) {
	s := New(Config{Enabled: true})
	sys, user := "You are terse.", "list the files"
	turn1 := flipFixture(sys, user, bigTool(40))
	out1, saved1 := s.ApplyRaw(translat.FmtOpenAI, turn1)
	if saved1 <= 0 || bytes.Equal(out1, turn1) {
		t.Fatal("turn 1 must compress and re-encode (precondition)")
	}

	turn2 := flipFixture(sys, user, shortTool)
	out2, _ := s.ApplyRaw(translat.FmtOpenAI, turn2)
	// Same conversation: the canonical prefix (system + user turns) must
	// stay byte-identical to what turn 1 sent.
	idx := bytes.Index(out1, []byte(`"role":"tool"`))
	start := bytes.LastIndex(out1[:idx], []byte(`,{"`))
	if start <= 0 {
		t.Fatal("fixture must contain a tool turn after the user turn")
	}
	if !bytes.HasPrefix(out2, out1[:start+1]) {
		t.Fatalf("turn 2 must keep turn 1's canonical prefix:\nturn1: %.120s\nturn2: %.120s", out1, out2)
	}
	// And it must NOT have flipped back to the raw client form.
	if bytes.Equal(out2, turn2) {
		t.Fatal("turn 2 reverted to raw client form: prefix cache busted (issue #35)")
	}
}

// TestStickyPerBlockAcrossTurns pins the per-block stickiness: a block
// compressed on turn 1 keeps its compressed form on turn 2 even when its
// savings percentage dips below the floor that turn.
func TestStickyPerBlockAcrossTurns(t *testing.T) {
	s := New(Config{Enabled: true})
	sys, user := "You are terse.", "list the files"
	if _, saved := s.ApplyRaw(translat.FmtOpenAI, flipFixture(sys, user, bigTool(40))); saved <= 0 {
		t.Fatal("turn 1 must compress (precondition)")
	}

	tool2 := belowFloorTool(t, s)
	turn2 := flipFixture(sys, user, tool2)
	out2, saved2 := s.ApplyRaw(translat.FmtOpenAI, turn2)
	if got := toolContentOf(t, out2); got != tool2 && saved2 <= 0 {
		t.Fatalf("known block must keep its compressed form below the floor (saved=%d)", saved2)
	} else if got == tool2 {
		t.Fatal("below-floor known block flipped back to raw: prefix busted at that block")
	}
}

// TestStickyIsolatedPerConversation: a fresh conversation must not
// inherit another conversation's stickiness — its no-savings turn stays
// on the plain unconditional gate (raw passthrough).
func TestStickyIsolatedPerConversation(t *testing.T) {
	s := New(Config{Enabled: true})
	// Conversation A compresses on turn 1 -> becomes sticky.
	if _, saved := s.ApplyRaw(translat.FmtOpenAI, flipFixture("You are terse.", "list the files", bigTool(40))); saved <= 0 {
		t.Fatal("conversation A turn 1 must compress (precondition)")
	}
	// Conversation B (different opening), no savings: stays raw.
	b := flipFixture("A completely different opening message.", "list the files", shortTool)
	if out, _ := s.ApplyRaw(translat.FmtOpenAI, b); !bytes.Equal(out, b) {
		t.Fatal("a fresh conversation must not inherit stickiness")
	}
	// Conversation A replayed with the same no-savings turn: stays
	// canonical.
	a := flipFixture("You are terse.", "list the files", shortTool)
	if out, _ := s.ApplyRaw(translat.FmtOpenAI, a); bytes.Equal(out, a) {
		t.Fatal("sticky conversation must stay canonical with no savings")
	}
}

// TestStickyNeverGrows: the never-grow guard — a sticky canonical
// restore must never make the body larger than the raw client form.
func TestStickyNeverGrows(t *testing.T) {
	s := New(Config{Enabled: true})
	sys, user := "You are terse.", "list the files"
	s.ApplyRaw(translat.FmtOpenAI, flipFixture(sys, user, bigTool(40)))
	for _, tool := range []string{shortTool, bigTool(40), bigTool(12)} {
		turn := flipFixture(sys, user, tool)
		out, _ := s.ApplyRaw(translat.FmtOpenAI, turn)
		if len(out) > len(turn) {
			t.Fatalf("sticky re-encode grew the body: %d > %d", len(out), len(turn))
		}
	}
}
