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
// (U+2581); the ASCII `<|tool_calls_begin|>` spelling is accepted defensively.
//
// Ported from the read-only reference OmniRoute/open-sse/utils/
// composerToolCalls.ts.

import (
	"encoding/json"
	"strings"
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

// composerMarkerFold normalizes the ASCII pipe spelling (`<|tool_calls_begin|>`)
// onto the full-width form the parser scans for. Cursor has used both, and a
// missed marker means a silently dropped tool call — the bug this file fixes.
var composerMarkerFold = strings.NewReplacer(
	"<|tool_calls_begin|>", composerCallsBegin,
	"<|tool_calls_end|>", composerCallsEnd,
	"<|tool_call_begin|>", composerCallBegin,
	"<|tool_call_end|>", composerCallEnd,
	"<|tool_sep|>", composerArgSep,
)

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
	folded := composerMarkerFold.Replace(text)
	var b strings.Builder
	var calls []composerCall
	pos := 0
	for {
		open := strings.Index(folded[pos:], composerCallsBegin)
		if open < 0 {
			b.WriteString(folded[pos:])
			return b.String(), calls, -1
		}
		open += pos
		b.WriteString(folded[pos:open])
		after := folded[open+len(composerCallsBegin):]
		rel := strings.Index(after, composerCallsEnd)
		if rel < 0 {
			return b.String(), calls, open
		}
		calls = append(calls, composerCallsIn(after[:rel])...)
		pos = open + len(composerCallsBegin) + rel + len(composerCallsEnd)
	}
}

// composerCallsIn parses every complete inner call of one outer block body.
func composerCallsIn(block string) []composerCall {
	var calls []composerCall
	for {
		cs := strings.Index(block, composerCallBegin)
		if cs < 0 {
			return calls
		}
		rest := block[cs+len(composerCallBegin):]
		ce := strings.Index(rest, composerCallEnd)
		if ce < 0 {
			return calls // truncated body: drop it rather than guess at one
		}
		block = rest[ce+len(composerCallEnd):]
		if name, args := composerCallArgs(rest[:ce]); name != "" {
			calls = append(calls, composerCall{Name: name, Args: args})
		}
	}
}

// composerCallArgs renders one inner call body (name + sep-delimited args) as
// an OpenAI tool-call arguments JSON object, plus the tool name.
//
// Body shape: `tool_name\n<sep>arg_name\narg_value\n<sep>arg2\nvalue2`.
// A segment with no newline is space-delimited (live captures do both).
func composerCallArgs(body string) (name, args string) {
	segs := strings.Split(strings.TrimSpace(body), composerArgSep)
	name = strings.TrimSpace(segs[0])
	if name == "" {
		return "", ""
	}
	obj := map[string]any{}
	for _, seg := range segs[1:] {
		if seg == "" {
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
// Frames split markers mid-sequence, and a leaked half-marker (`<｜tool▁cal`) shows
// up as junk in the client's transcript, so the caller holds it back one frame.
// A COMPLETE marker is not a fragment: composerScan reports it as an open block.
func composerPartialMarkerCut(s string) int {
	for n := min(len(s), len(composerCallsBegin)-1); n > 0; n-- {
		if strings.HasSuffix(s, composerCallsBegin[:n]) {
			return len(s) - n
		}
	}
	return len(s)
}
