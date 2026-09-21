package translat

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// junkSpecimen1 is a byte-for-byte copy of a live junk reasoning block the
// field reported (free lane, 2026-09-21): Latin, CJK and Cyrillic fragments
// interleaved with symbol runs. It is 343 bytes, so the guard judges it only
// once the run has accumulated past junkMinBytes — which is what the relay
// sees after a few hundred further tokens of the same garbage.
const junkSpecimen1 = `*** Begin 拳击,   Boxing.""</-Re*=bC([R3]****** Репозиторий;  加入T
*_*QRW.
*Um) aszil,我们都是从这里开始
nel mezzo. It]
snapping.alt >>? Za.""render2e ...</一旦声明  受. Demonstration [0emode-white69<span contentu[k]
<Pias) incarnarden the.US Patent pクラP瀛<mmd"]." similarity-iness ‘sport =『 vaccinesiot

 Cursor沟通- Eyeth, SCI村南通--}; Øh3wwmouso—
ENTOUR Privacy+
`

// junkSpecimen2 is the second half of the same report: the symbol-soup class
// the corpus analysis found (`,` storms around short fragments).
const junkSpecimen2 = `,kenO.RH*是RO儿*!m"-WR*WJUNTOS/O *!! _____. 2469-0"V|.
) Conclusions
  %k;**U 5*W}D?,0e a
,T^!>:+<｜place▁holder▁no▁339｜><Bl!*ZEr
这说明
:!  HOLA?f S-T.@#$!!!!*"0>:E*|R! !!O>))T*?&$""O7ade!z*ll* :无剧透.
  GENERat G,en*Oass?is!d
  "V 3P!!后?W f@ Gandhi, er?
)*!!官"Xv真//These!-*9*0/~~/habbas*
`

// The comma storm captured live: a 5KB reasoning block that is 79% symbol
// runs and 40% punctuation tokens. This is the shape the guard exists for.
const junkCommaStorm = `... ,́,́, ́,,,,́,́
,́,,,,,,,

,

,
 
,

,
,
,,,

@@,
,

,

,

,
,
,
,
,
,,,,
,,

,

dies,
,,,,

,

,

,

,

,
,

,

,
,

,,,,

,

,

,
,

,
,

,

,
,

,

,

,
,,,,

,

,

,

,

,
,
,
,

,,,

,

,

,

,

,

,

,,TEXT,
,

,

,

,
,,,,,,,,

,

,

,

,
,,,,,,,,

,

,

,

,
,,,,
;̂❌,́
,

,

,
,
,
,
,
`

// junkVvvvStorm is the corpus's other junk shape (2026-09-20): a run that
// collapsed into character repeats and comma rows after losing the plot.
const junkVvvvStorm = `. vvvvvvvvvvvvvvvvvvv,vvvvvvvvvvvvvvvv, To preserve.
,,,,,,,,,,,,,,
,,,,,,,,,,,,,,,,,,, ,,,,
 ,,,
.
Nikolass
,,,,,,,,.,,,,,,
]]]
,,,,,,,,,,,,
,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,
############################################################
########..
,0	,,,,,,,.	returnt,,,.!
<b> .
,,,
FVTT,,,,,,,,,,,,,"
,,,,,  .

,:

     
jibre	cdate,,,,,,,,,,,,    -
,FY!++++
`

// realReasoningEN / ZH / VI / code are the false-positive classes the verdict
// must never touch: prose with ASCII, CJK punctuation, Vietnamese diacritics,
// and a code fence full of symbols.
const realReasoningZH = `我先读一下 app.go，找到 thinking block 的渲染位置，然后再决定要不要加一个 gate。
如果块是空的，就跳过渲染；否则保持现有的 showThinking 行为不变。`

const realReasoningVI = `Người dùng muốn tôi sửa phần hiển thị thinking. Tôi sẽ đọc file app.go trước rồi mới quyết định.
Điểm quan trọng là không được đánh dấu nhầm văn bản tiếng Việt là rác.`

const realReasoningCode = "```go\n" + `func (a *App) thinkBoxLines(i int, b *Block, w int) []line {
	box := a.th.Box()
	border := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentThinking)))
	if i == a.thinkFocus {
		border = border.Bold(true)
	}
	rows := a.thinkRows(b, w)
	start, end := 0, len(rows)
	if !b.Expanded {
		start, end = thinkWindow(len(rows), b.ThinkOff)
	}
	return append([]line{boxTop(box, border, hdr, w)}, rows[start:end]...)
}
` + "```"

const realReasoningHashes = `My branch is 1 commit ahead of main, and merge-base is f419040 which is main's ancestor.
Wait, main tip is 403c9d5, and merge-base is f419040. So main has 3 commits after f419040
(f4daf07? no...). Let me check: main = 403c9d5, and log origin/main..main earlier showed
403c9d5, 9bd5daa, 3a8f47f, f419040, f4daf07 ahead of f71e12c. So the order on main is:
f71e12c -> f4daf07 -> f419040 -> 3a8f47f -> 9bd5daa -> 403c9d5.`

// pad repeats s until it is at least n bytes, so a short specimen is judged at
// the length scale the relay actually sees.
func pad(s string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(s)
	}
	return b.String()
}

func TestJunkReasoningCatchesLiveSpecimens(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		// The pasted report's symbol-soup half, repeated to the length the
		// relay actually judges it at.
		{"specimen2", pad(junkSpecimen2, junkMinBytes+200)},
		{"both-halves", junkSpecimen1 + junkSpecimen2},
		{"comma-storm", pad(junkCommaStorm, junkMinBytes+200)},
		{"vvvv-storm", pad(junkVvvvStorm, junkMinBytes+200)},
	}
	for _, tc := range cases {
		if !junkReasoning(tc.text) {
			wordR, badR, sym := junkStats(tc.text)
			t.Errorf("%s: junkReasoning = false, want true (wordR=%.3f badR=%.3f sym/100=%.1f len=%d)",
				tc.name, wordR, badR, sym, len(tc.text))
		}
	}
}

// TestJunkReasoningCeiling pins the documented coverage ceiling: the pasted
// report's mixed-script half measures like bilingual reasoning, so it does NOT
// trip on its own. This test exists so the ceiling stays a decision rather
// than a surprise — if a future shape catches it, this test fails and the
// doc comment on junkStats gets updated with it.
func TestJunkReasoningCeiling(t *testing.T) {
	mixed := pad(junkSpecimen1, junkMinBytes+200)
	if junkReasoning(mixed) {
		wordR, badR, sym := junkStats(mixed)
		t.Fatalf("mixed-script specimen now trips (wordR=%.3f badR=%.3f sym/100=%.1f): "+
			"update the coverage-ceiling note on junkReasoning", wordR, badR, sym)
	}
}

func TestJunkReasoningLeavesRealReasoningAlone(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"english", pad(realReasoningHashes, junkMinBytes+200)},
		{"chinese", pad(realReasoningZH, junkMinBytes+200)},
		{"vietnamese", pad(realReasoningVI, junkMinBytes+200)},
		{"code", pad(realReasoningCode, junkMinBytes+200)},
		{"short-preamble", "Let me check the parser first."},
	}
	for _, tc := range cases {
		if junkReasoning(tc.text) {
			wordR, badR, sym := junkStats(tc.text)
			t.Errorf("%s: junkReasoning = true, want false (wordR=%.3f badR=%.3f sym/100=%.1f)",
				tc.name, wordR, badR, sym)
		}
	}
}

func TestJunkGuardPrefetchFailsOverJunkReasoning(t *testing.T) {
	// A reasoning-only stream of junk, arriving in small chunks: the guard
	// holds the head, accumulates the reasoning, and returns the verdict
	// BEFORE anything is released — which is what lets Router.Execute drop
	// the attempt and re-call the model.
	var stream []byte
	for i := 0; i < 40; i++ {
		stream = append(stream, reasoningEvent([]byte(junkCommaStorm[:200]))...)
	}
	g := NewJunkGuard(&chunkReader{data: stream, size: 97}, junkHoldBytes)

	err := g.Prefetch()
	if err == nil {
		t.Fatal("Prefetch() = nil, want a junk-reasoning verdict")
	}
	if err.Status != 502 || err.Type != junkErrorType {
		t.Fatalf("verdict = %d %s, want 502 %s", err.Status, err.Type, junkErrorType)
	}
	if err.StreamCommitted {
		t.Fatal("verdict is stream-committed; a failover-capable verdict must not be")
	}
}

// TestJunkGuardReleasesOnRealOutput pins the ordering rule: usable output
// anywhere in the held head releases it, and the guard stops judging from
// then on. The junk AFTER the answer is never a reason to discard a stream
// the client can already use.
func TestJunkGuardReleasesOnRealOutput(t *testing.T) {
	var stream []byte
	stream = append(stream, []byte(`data: {"id":"1","choices":[{"delta":{"content":"Here is the fix."}}]}`+"\n\n")...)
	for i := 0; i < 40; i++ {
		stream = append(stream, reasoningEvent([]byte(junkCommaStorm[:200]))...)
	}
	g := NewJunkGuard(bytes.NewReader(stream), junkHoldBytes)

	if err := g.Prefetch(); err != nil {
		t.Fatalf("Prefetch() = %v, want nil (real content released the head)", err)
	}
	rest, _ := io.ReadAll(g)
	if !bytes.Contains(rest, []byte("Here is the fix.")) {
		t.Fatal("held head did not relay the answer verbatim")
	}
	if g.Junk() != nil {
		t.Fatalf("Junk() = %v, want nil after real output", g.Junk())
	}
}

// TestJunkGuardTripsOnJunkBeforeAnswer pins the deliberate other half of that
// rule: a corrupt prefix that lasts longer than the guard's evidence floor is
// junk even if an answer eventually follows it, because the client's thinking
// box has already been poisoned and a re-call produces a clean run. The floor
// keeps this from firing on a single odd token (see junkMinBytes).
func TestJunkGuardTripsOnJunkBeforeAnswer(t *testing.T) {
	var stream []byte
	for i := 0; i < 40; i++ {
		stream = append(stream, reasoningEvent([]byte(junkCommaStorm[:200]))...)
	}
	stream = append(stream, []byte(`data: {"id":"1","choices":[{"delta":{"content":"Here is the fix."}}]}`+"\n\n")...)
	g := NewJunkGuard(bytes.NewReader(stream), junkHoldBytes)
	if err := g.Prefetch(); err == nil {
		t.Fatal("Prefetch() = nil, want a junk verdict: the answer came after KBs of corruption")
	}
}

func TestJunkGuardScanOnlyRecordsLateVerdict(t *testing.T) {
	// hold == 0 is the no-sibling route: nothing is buffered, so the verdict
	// can only be recorded (for the dashboard), never acted on.
	var stream []byte
	for i := 0; i < 40; i++ {
		stream = append(stream, reasoningEvent([]byte(junkCommaStorm[:200]))...)
	}
	g := NewJunkGuard(bytes.NewReader(stream), 0)
	if err := g.Prefetch(); err != nil {
		t.Fatalf("Prefetch() with hold=0 = %v, want nil", err)
	}
	if _, err := io.ReadAll(g); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if g.Junk() == nil {
		t.Fatal("Junk() = nil, want the late verdict recorded")
	}
	if g.Junk().StreamCommitted {
		t.Fatal("late verdict must not claim the stream was committed")
	}
}

func TestJunkGuardRelaysHealthyStreamByteForByte(t *testing.T) {
	var stream []byte
	for i := 0; i < 6; i++ {
		stream = append(stream, reasoningEvent([]byte(realReasoningCode))...)
	}
	g := NewJunkGuard(bytes.NewReader(stream), junkHoldBytes)
	if err := g.Prefetch(); err != nil {
		t.Fatalf("Prefetch() = %v, want nil", err)
	}
	got, err := io.ReadAll(g)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, stream) {
		t.Fatalf("relayed %d bytes, want the stream verbatim (%d bytes)", len(got), len(stream))
	}
}

func TestJunkDeltaTextReadsEveryVendorAlias(t *testing.T) {
	chunk := []byte(`data: {"choices":[{"delta":{"reasoning_content":"native ","reasoning":"alias ",` +
		`"reasoning_text":"text alias"}}]}`)
	got := junkDeltaText(chunk)
	for _, want := range []string{"native", "alias", "text alias"} {
		if !strings.Contains(got, want) {
			t.Fatalf("junkDeltaText = %q, want it to contain %q", got, want)
		}
	}
}
