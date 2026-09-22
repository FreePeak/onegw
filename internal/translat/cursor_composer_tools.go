package translat

// Composer (cursor `composer-*`) inline tool-call parser.
//
// The IDE composer family is a ChatService text model: asked to use a declared
// tool, it does not answer with a ClientSideToolV2Call protobuf field (field 1)
// — it WRITES the DeepSeek-style invocation into its visible answer:
//
//	Printing it with bash too:
//	<｜tool▁calls▁begin｜>
//	<｜tool▁call▁begin｜>
//	run_terminal_cmd
//	<｜tool▁sep｜>command
//	echo "You're handsome!"
//	<｜tool▁call▁end｜>
//	<｜tool▁calls▁end｜>
//
// Live 2026-09-21: composer-2.5 answered a tool-bearing request entirely in its
// thinking channel and emitted those markers as the visible suffix (everything
// after the last </think>). The gateway promotes that suffix to content, so the
// markers reached the client as plain text and the tool call was silently lost
// — the client then echoed the literal marker text back.
//
// Markers use full-width pipes (U+FF5C) and the small-triangle separator
// (U+2581); ASCII pipes/underscores and ANY mix of the two spellings parse
// identically (composerMarkerAt folds each rune before comparing — OmniRoute
// tolerates hybrids defensively and so do we). The visible answer may also
// arrive wrapped in protocol-internal <final> sentinels (full-width or ASCII
// pipes), which stripComposerFinal removes so they never reach the client.
//
// Ported from the read-only reference OmniRoute/open-sse/utils/
// composerToolCalls.ts.

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Composer marker fragments.
const (
	composerCallsBegin = "<｜tool▁calls▁begin｜>"
	composerCallsEnd   = "<｜tool▁calls▁end｜>"
	composerCallBegin  = "<｜tool▁call▁begin｜>"
	composerCallEnd    = "<｜tool▁call▁end｜>"
	composerArgSep     = "<｜tool▁sep｜>"

	// composerThinkEnd ends the private chain-of-thought; the user-facing
	// reply (and any inline tool call) follows it.
	composerThinkEnd = "</think>"
)

// composerCanonFold maps one rune onto the canonical marker spelling: ASCII
// pipe (|) and underscore (_) become the full-width forms Cursor's markers
// use (U+FF5C pipe, U+2581 separator), so <|tool_calls_begin|>, its
// all-full-width twin, and every hybrid of the two all scan as the same
// marker. A missed marker is a silently dropped tool call — the bug this
// file exists to prevent.
func composerCanonFold(r rune) rune {
	switch r {
	case '|':
		return '\uFF5C'
	case '_':
		return '\u2581'
	}
	return r
}

// composerMarkerAt reports whether the canonical full-width marker begins at
// byte offset i of s, tolerant of ASCII pipe/underscore spellings, and the
// byte offset just past it. Scanning stays byte-based so a frame that splits
// a multibyte rune mid-way never fabricates a match (DecodeRuneInString
// yields RuneError for the broken tail, which no marker rune folds to).
func composerMarkerAt(s string, i int, marker []rune) (bool, int) {
	off := i
	for j := range marker {
		if off >= len(s) {
			return false, i
		}
		r, sz := utf8.DecodeRuneInString(s[off:])
		if r == utf8.RuneError && sz == 1 {
			return false, i
		}
		if composerCanonFold(r) != marker[j] {
			return false, i
		}
		off += sz
	}
	return true, off
}

// composerCall is one parsed inline invocation.
type composerCall struct {
	Name string
	Args string // OpenAI tool-call arguments JSON
}

// composerScan walks every inline invocation block in a composer visible answer.
// It returns the text with each CLOSED block removed (the residual the client
// should see), the calls those blocks declared, and — when a block is still
// arriving — the offset at which it begins, so the caller can hold back the
// invocation text instead of streaming it as content. -1 means no open block.
//
// A block that is still streaming is never parsed: a half-arrived invocation
// must not be fabricated into a real one. Inside a closed block, a truncated
// inner call is dropped rather than guessed at.
func composerScan(text string) (string, []composerCall, int) {
	callsBegin := []rune(composerCallsBegin)
	callsEnd := []rune(composerCallsEnd)
	var b strings.Builder
	var calls []composerCall
	pos := 0
	for {
		open := -1
		beginEnd := pos
		for i := pos; i < len(text); i++ {
			if ok, end := composerMarkerAt(text, i, callsBegin); ok {
				open = i
				beginEnd = end
				break
			}
		}
		if open < 0 {
			b.WriteString(text[pos:])
			return b.String(), calls, -1
		}
		b.WriteString(text[pos:open])
		rel := -1
		for i := beginEnd; i < len(text); i++ {
			if ok, _ := composerMarkerAt(text, i, callsEnd); ok {
				rel = i - beginEnd
				break
			}
		}
		if rel < 0 {
			return b.String(), calls, b.Len()
		}
		calls = append(calls, composerCallsIn(text[beginEnd:beginEnd+rel])...)
		// Advance past the END marker's REAL bytes (ASCII spelling is shorter
		// than the canonical full-width form; rel is anchored to the matcher).
		_, endEnd := composerMarkerAt(text, beginEnd+rel, callsEnd)
		pos = endEnd
	}
}

// composerCallsIn parses every complete inner call of one outer block body.
func composerCallsIn(block string) []composerCall {
	callBegin := []rune(composerCallBegin)
	callEnd := []rune(composerCallEnd)
	var calls []composerCall
	rest := block
	for {
		cs := -1
		for i := 0; i < len(rest); i++ {
			if ok, _ := composerMarkerAt(rest, i, callBegin); ok {
				cs = i
				break
			}
		}
		if cs < 0 {
			return calls
		}
		_, afterCall := composerMarkerAt(rest, cs, callBegin)
		body := rest[afterCall:]
		ce := -1
		for i := 0; i < len(body); i++ {
			if ok, _ := composerMarkerAt(body, i, callEnd); ok {
				ce = i
				break
			}
		}
		if ce < 0 {
			return calls // truncated body: drop it rather than guess at one
		}
		if name, args := composerCallArgs(body[:ce]); name != "" {
			calls = append(calls, composerCall{Name: name, Args: args})
		}
		_, afterCallEnd := composerMarkerAt(body, ce, callEnd)
		rest = body[afterCallEnd:]
	}
}

// composerCallArgs renders one inner call body (name + sep-delimited args) as
// an OpenAI tool-call arguments JSON object, plus the tool name.
//
// Body shape: `tool_name\n<sep>arg_name\narg_value\n<sep>arg2\nvalue2`.
// A segment with no newline is space-delimited (live captures do both).
func composerCallArgs(body string) (name, args string) {
	sep := []rune(composerArgSep)
	var segs []string
	scan := strings.TrimSpace(body)
	for {
		idx := -1
		for i := 0; i < len(scan); i++ {
			if ok, _ := composerMarkerAt(scan, i, sep); ok {
				idx = i
				break
			}
		}
		if idx < 0 {
			segs = append(segs, scan)
			break
		}
		_, afterSep := composerMarkerAt(scan, idx, sep)
		segs = append(segs, scan[:idx])
		scan = scan[afterSep:]
	}
	if len(segs) == 0 {
		return "", ""
	}
	name = strings.TrimSpace(segs[0])
	if name == "" {
		return "", ""
	}
	obj := map[string]any{}
	for _, seg := range segs[1:] {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		var argName, argValue string
		if nl := strings.Index(seg, "\n"); nl >= 0 {
			argName = strings.TrimSpace(seg[:nl])
			argValue = strings.TrimRight(seg[nl+1:], "\n")
		} else if sp := strings.IndexAny(seg, " \t"); sp >= 0 {
			argName = strings.TrimSpace(seg[:sp])
			argValue = strings.TrimSpace(seg[sp+1:])
		} else {
			argName = strings.TrimSpace(seg)
		}
		if argName == "" {
			continue
		}
		obj[argName] = composerCoerceArg(argValue)
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return name, "{}"
	}
	return name, string(raw)
}

// composerCoerceArg turns an argument value into a native JSON value where it
// clearly is one (objects, arrays, booleans, null), else keeps the raw string
// verbatim so multi-line values survive.
func composerCoerceArg(raw string) any {
	s := strings.TrimSpace(raw)
	if s != "" && ((s[0] == '{' && s[len(s)-1] == '}') || (s[0] == '[' && s[len(s)-1] == ']')) {
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			return v
		}
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	return raw
}

// composerPartialMarkerCut returns the offset at which a trailing fragment that
// could still grow into an opening marker begins (len(s) when there is none).
// Frames split markers mid-sequence — in either spelling — and a leaked
// half-marker shows up as junk in the client's transcript, so the caller holds
// it back one frame. A COMPLETE marker is not a fragment: composerScan reports
// it as an open block.
func composerPartialMarkerCut(s string) int {
	for _, m := range []string{composerCallsBegin, composerCallBegin} {
		for n := min(len(s), len(m)-1); n > 0; n-- {
			if strings.HasSuffix(s, m[:n]) {
				return len(s) - n
			}
		}
		ascii := composerASCIISpelling(m)
		for n := min(len(s), len(ascii)-1); n > 0; n-- {
			if strings.HasSuffix(s, ascii[:n]) {
				return len(s) - n
			}
		}
	}
	return len(s)
}

// composerASCIISpelling renders the ASCII-pipe/underscore form of a canonical
// full-width marker (a 1:1 inverse of composerCanonFold), for partial-marker
// holdback that must also cover the ASCII spellings.
func composerASCIISpelling(m string) string {
	var b strings.Builder
	for _, r := range m {
		switch r {
		case '\uFF5C':
			b.WriteByte('|')
		case '\u2581':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripComposerFinal removes the protocol-internal <final> wrapper Cursor can
// put around a composer visible answer — full-width or ASCII pipes
// (decolua/9router#1316). A PARTIAL opening sentinel holds the whole chunk
// back so a half-streamed "<" or "<|fin" never leaks as content.
func stripComposerFinal(s string) string {
	const openFull = "<\uFF5Cfinal\uFF5C>"
	const openASCII = "<|final|>"
	const closeFull = "<\uFF5C/final\uFF5C>"
	const closeASCII = "<|/final|>"
	switch {
	case strings.HasPrefix(s, openFull):
		s = s[len(openFull):]
	case strings.HasPrefix(s, openASCII):
		s = s[len(openASCII):]
	case isComposerFinalPartial(s):
		return ""
	}
	switch {
	case strings.HasSuffix(s, closeFull):
		s = s[:len(s)-len(closeFull)]
	case strings.HasSuffix(s, closeASCII):
		s = s[:len(s)-len(closeASCII)]
	}
	return strings.TrimSpace(s)
}

// isComposerFinalPartial reports whether s starts with a not-yet-complete
// final sentinel ("<", "<|f" cut mid-stream) that the caller must hold back.
// A "<" followed by anything but a pipe (e.g. "<div>") is NOT a sentinel.
func isComposerFinalPartial(s string) bool {
	if !strings.HasPrefix(s, "<") {
		return false
	}
	rest := s[1:]
	if rest == "" {
		return true
	}
	return (strings.HasPrefix(rest, "\uFF5C") || rest[0] == '|') && !strings.Contains(rest, ">")
}

// sanitizeComposerHistory strips protocol-internal Composer markers from
// assistant message content before it is sent back as conversation history.
// Without this, when a session switches from a composer model (composer-2.5)
// to a non-composer model (cursor/auto → default), the <final> sentinels and
// inline tool-call blocks from previous turns poison the history, causing the
// upstream AgentService/ChatService to reject the stream
// (PI_AI_ERROR "upstream stream interrupted").
//
// Markers stripped:
//   - <final> / <｜final｜> sentinels (wrapper)
//   - <tool_calls_begin> … <tool_calls_end> inline invocation blocks
//   - <thinking> tags
func sanitizeComposerHistory(s string) string {
	// Strip <final> sentinels (wrapper around the whole answer).
	s = stripComposerFinal(s)
	// Strip </thinking> tags.
	s = strings.ReplaceAll(s, "</think>", "")
	s = strings.ReplaceAll(s, "<｜thinking｜>", "")
	// Strip inline tool-call blocks: from <tool_calls_begin> to <tool_calls_end>,
	// including any text between them (tool names, args, separators).
	// We scan for the begin marker and find the matching end marker.
	for {
		beginIdx := -1
		// Search for the begin marker (folded).
		for i := 0; i < len(s); i++ {
			if ok, end := composerMarkerAt(s, i, []rune(composerCallsBegin)); ok {
				beginIdx = i
				_ = end
				break
			}
			// Also check ASCII variant.
			if ok, _ := composerMarkerAt(s, i, []rune("<|tool_calls_begin|>")); ok {
				beginIdx = i
				break
			}
		}
		if beginIdx < 0 {
			break
		}
		// Find the end marker after beginIdx.
		endIdx := -1
		beginEnd := beginIdx + len(composerCallsBegin)
		for i := beginEnd; i < len(s); i++ {
			if ok, end := composerMarkerAt(s, i, []rune(composerCallsEnd)); ok {
				endIdx = end
				_ = end
				break
			}
			if ok, end := composerMarkerAt(s, i, []rune("<|tool_calls_end|>")); ok {
				endIdx = end
				break
			}
		}
		if endIdx < 0 {
			// No matching end — strip from begin to end of string.
			s = s[:beginIdx]
			break
		}
		// Remove the entire block including a trailing newline.
		s = s[:beginIdx] + s[endIdx:]
	}
	return strings.TrimSpace(s)
}
