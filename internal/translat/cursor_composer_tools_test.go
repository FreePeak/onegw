package translat

// Composer inline tool-call tests. The dialect is captured live (2026-09-21):
// composer-2.5 answers a tool-bearing request entirely inside its thinking
// channel and WRITES the DeepSeek-style invocation into the visible suffix
// instead of returning a ClientSideToolV2Call field. Before this, the markers
// arrived at the client as plain content and the tool call was silently lost
// (the client then answered the literal marker text back).

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/types"
)

// composerBlock renders one live-shaped invocation block.
func composerBlock(name string, args [][2]string) string {
	var sb strings.Builder
	sb.WriteString(composerCallsBegin)
	sb.WriteString(composerCallBegin)
	sb.WriteString(name)
	for _, a := range args {
		sb.WriteString(composerArgSep)
		sb.WriteString(a[0])
		sb.WriteString("\n")
		sb.WriteString(a[1])
	}
	sb.WriteString(composerCallEnd)
	sb.WriteString(composerCallsEnd)
	return sb.String()
}

func TestComposerParse(t *testing.T) {
	text := "Printing it with bash too:\n" + composerBlock("run_terminal_cmd", [][2]string{
		{"command", `echo "You're handsome!"`},
		{"is_background", "false"},
	})
	residual, calls, _ := composerScan(text)
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	if strings.TrimSpace(residual) != "Printing it with bash too:" {
		t.Fatalf("residual=%q", residual)
	}
	if calls[0].Name != "run_terminal_cmd" {
		t.Fatalf("name=%q", calls[0].Name)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(calls[0].Args), &got); err != nil {
		t.Fatalf("arguments not JSON: %v (%q)", err, calls[0].Args)
	}
	if got["command"] != `echo "You're handsome!"` {
		t.Fatalf("command=%v", got["command"])
	}
	// "false" coerces to a JSON bool, not the string "false".
	if got["is_background"] != false {
		t.Fatalf("is_background=%v (%T)", got["is_background"], got["is_background"])
	}
}

func TestComposerParseMultipleAndResidual(t *testing.T) {
	text := composerBlock("alpha", [][2]string{{"x", "1"}}) + "\nmiddle\n" +
		composerBlock("beta", [][2]string{{"y", "2"}})
	residual, calls, _ := composerScan(text)
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "alpha" || calls[1].Name != "beta" {
		t.Fatalf("names=%q,%q", calls[0].Name, calls[1].Name)
	}
	if strings.TrimSpace(residual) != "middle" {
		t.Fatalf("residual=%q", residual)
	}
}

func TestComposerParseNoMarkers(t *testing.T) {
	// Plain prose passes through untouched and yields no calls.
	residual, calls, _ := composerScan("just an answer")
	if residual != "just an answer" || len(calls) != 0 {
		t.Fatalf("residual=%q calls=%v", residual, calls)
	}

	// A half-delivered block must never fabricate a tool invocation, and its
	// text is returned unchanged so the streaming holdback keeps it safe.
	partial := "preamble " + composerCallsBegin + composerCallBegin + "run"
	if residual, calls, open := composerScan(partial); len(calls) != 0 || residual != "preamble " || open != len("preamble ") {
		t.Fatalf("unterminated block: residual=%q calls=%v open=%d", residual, calls, open)
	}
}

func TestComposerParseASCIIFallback(t *testing.T) {
	// ASCII markers (Cursor has used both spellings) parse identically.
	ascii := "<|tool_calls_begin|><|tool_call_begin|>run_terminal_cmd<|tool_sep|>command\necho hi<|tool_call_end|><|tool_calls_end|>"
	_, calls, _ := composerScan(ascii)
	if len(calls) != 1 {
		t.Fatalf("ASCII markers must parse, got %d calls", len(calls))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(calls[0].Args), &got); err != nil {
		t.Fatalf("arguments not JSON: %v (%q)", err, calls[0].Args)
	}
	if got["command"] != "echo hi" {
		t.Fatalf("command=%v", got["command"])
	}
}

func TestComposerScanHoldback(t *testing.T) {
	// A closed block is consumed: the residual is the surrounding text.
	full := "hello " + composerBlock("t", [][2]string{{"a", "b"}}) + " bye"
	if residual, _, open := composerScan(full); residual != "hello  bye" || open >= 0 {
		t.Fatalf("complete block: residual=%q open=%d", residual, open)
	}
	// Opened, not closed: the residual stops at the marker and the block's
	// offset is reported so the caller holds the invocation text back.
	openOnly := "hello " + composerCallsBegin + composerCallBegin + "partial"
	residual, calls, open := composerScan(openOnly)
	if residual != "hello " || open != len("hello ") || len(calls) != 0 {
		t.Fatalf("open block: residual=%q open=%d calls=%d", residual, open, len(calls))
	}
	// A trailing partial marker prefix is held so it cannot leak as text.
	for _, tail := range []string{"<", "<｜", "<｜tool", "<｜tool▁calls"} {
		if got := composerPartialMarkerCut("text" + tail); got != len("text") {
			t.Fatalf("partial %q: cut=%d want %d", tail, got, len("text"))
		}
	}
	// A bare '<' that is not a marker prefix is left alone, so ordinary prose
	// ("a < b") is not eaten.
	if got := composerPartialMarkerCut("a < b"); got != len("a < b") {
		t.Fatalf("plain text: cut=%d want %d", got, len("a < b"))
	}
}

// The end-to-end path: a composer turn whose field-25 body carries the markers
// must yield OpenAI tool_calls deltas (not content), and the markers must not
// reach the client as text.
func TestCursorSSEStreamComposerInlineToolCall(t *testing.T) {
	vis := "You're handsome.\n\nPrinting it with bash too:\n" + composerBlock("run_terminal_cmd", [][2]string{
		{"command", `echo "You're handsome!"`},
	})
	payload := chatThinkingFrame("", "musing about it\n</think>"+vis)

	stream := CursorSSEStream(bytes.NewReader(wrapConnectFrame(payload)), "composer-2.5", false, nil)
	out := readAllString(t, stream)

	if strings.Contains(out, "tool▁calls▁begin") {
		t.Fatalf("markers leaked to the client as content: %s", out)
	}
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("no tool_calls delta emitted: %s", out)
	}
	if !strings.Contains(out, `"name":"run_terminal_cmd"`) {
		t.Fatalf("tool name missing: %s", out)
	}
	if !strings.Contains(out, `\"command\":\"echo \\\"You're handsome!\\\"\"`) {
		t.Fatalf("tool arguments missing or malformed: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("a tool-carrying turn must finish with tool_calls: %s", out)
	}
	if !strings.Contains(out, "You're handsome.") {
		t.Fatalf("visible preamble text must survive: %s", out)
	}
	if strings.Contains(out, "musing about it") {
		t.Fatalf("thinking prefix leaked: %s", out)
	}
}

// The same body split across frames at every possible marker boundary: the
// streaming path must hold back a partial marker and still emit the call once.
func TestCursorSSEStreamComposerChunkSplit(t *testing.T) {
	vis := "ok:\n" + composerBlock("run_terminal_cmd", [][2]string{{"command", "echo hi"}})
	full := "reason\n</think>" + vis
	// Build frames that split the visible text mid-marker (1 byte at a time
	// through the marker region).
	for _, at := range []int{len("reason\n</think>ok:\n") + 1, len("reason\n</think>ok:\n<｜tool"), len(full) - 3} {
		if at <= 0 || at >= len(full) {
			continue
		}
		st := &CursorChatState{}
		var out []StreamEvent
		for _, chunk := range []string{full[:at], full[at:]} {
			out = append(out, CursorChatEvents(chatThinkingFrame("", chunk), "composer-2.5", st)...)
		}
		var text strings.Builder
		calls := 0
		for _, e := range out {
			if e.Kind == EvDelta && e.PartType == types.PartText {
				text.WriteString(e.Text)
			}
			if e.Kind == EvPartStart && e.PartType == types.PartToolUse {
				calls++
			}
		}
		if calls != 1 {
			t.Fatalf("split at %d: want 1 tool call, got %d (chunks %q|%q)", at, calls, full[:at], full[at:])
		}
		if strings.Contains(text.String(), "tool▁calls") {
			t.Fatalf("split at %d leaked markers into content: %q", at, text.String())
		}
	}
}
