package server

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/translat"
	"onegw/internal/types"
)

// oaWireTool is the invariant every strict tool-calling upstream re-checks on
// the OpenAI chat wire, in their own words:
//
//	"`messages[N]` tool message must follow an assistant message"      (z.ai)
//	"An assistant message with 'tool_calls' must be followed by tool
//	 messages responding to each 'tool_call_id'"                    (opencode "Console Go")
//
// A role:"tool" message may only appear in the run directly after the assistant
// turn that made the calls, and every call id must be answered exactly once
// inside that run.
type oaWireTool struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID string `json:"id"`
	} `json:"tool_calls"`
}

func assertOAWireToolPairing(t *testing.T, body []byte) []oaWireTool {
	t.Helper()
	var wire struct {
		Messages []oaWireTool `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode wire body: %v (%s)", err, body)
	}
	msgs := wire.Messages
	for i, m := range msgs {
		if m.Role ***REMOVED*** "tool" {
			if i ***REMOVED*** 0 {
				t.Fatalf("messages[0]: a tool message cannot lead the array: %s", body)
			}
			prev := msgs[i-1]
			if prev.Role != "tool" && !(prev.Role ***REMOVED*** "assistant" && len(prev.ToolCalls) > 0) {
				t.Fatalf("messages[%d]: tool message must follow an assistant message with tool_calls (predecessor role %q): %s", i, prev.Role, body)
			}
		}
		if m.Role != "assistant" || len(m.ToolCalls) ***REMOVED*** 0 {
			continue
		}
		want := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			want[tc.ID] = false
		}
		for j := i + 1; j < len(msgs) && msgs[j].Role ***REMOVED*** "tool"; j++ {
			answered, known := want[msgs[j].ToolCallID]
			if !known {
				t.Fatalf("messages[%d]: tool reply for call id %q that no assistant turn asked for: %s", j, msgs[j].ToolCallID, body)
			}
			if answered {
				t.Fatalf("messages[%d]: duplicate tool reply for call id %q: %s", j, msgs[j].ToolCallID, body)
			}
			want[msgs[j].ToolCallID] = true
		}
		for id, answered := range want {
			if !answered {
				t.Fatalf("messages[%d]: assistant tool_call %q is never answered by a following tool message: %s", i, id, body)
			}
		}
	}
	return msgs
}

// claudeCodeToolHistory is Claude Code's request shape, which broke live on
// 2026-09-14: a user turn that answers tool_use blocks AND carries trailing
// <system-reminder> text, and (after a mid-turn interjection) the two calls of
// one assistant turn answered in SEPARATE user turns with text in between.
// Translating either verbatim strands a tool message behind a user message, and
// because the turn is already in the client's history the session 400s on every
// retry — at whatever index the first such turn lands (observed: 47 and 161).
func claudeCodeToolHistory(tail ...string) []byte {
	turns := `,
	    {"role": "assistant", "content": [
	      {"type": "tool_use", "id": "toolu_02", "name": "Read", "input": {"file": "a"}},
	      {"type": "tool_use", "id": "toolu_03", "name": "Read", "input": {"file": "b"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_03", "content": "B"},
	      {"type": "text", "text": "<system-reminder>the user sent a new message</system-reminder>"}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_02", "content": "A"}
	    ]}`
	for _, t := range tail {
		turns += `,{"role": "user", "content": "` + t + `"}`
	}
	return []byte(`{
	  "model": "claude-sonnet-4-5",
	  "max_tokens": 100,
	  "system": [{"type":"text","text":"You are Claude Code."}],
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "should I look?", "signature": "abc"},
	      {"type": "text", "text": "let me check"},
	      {"type": "tool_use", "id": "toolu_01", "name": "Bash", "input": {"command": "ls"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_01", "content": "a\nb\nc"},
	      {"type": "text", "text": "<system-reminder>stdout was 3 lines</system-reminder>"}
	    ]}` + turns + `
	  ]
	}`)
}

// TestPrepareUpstreamBodyPairsToolTurnsAcrossFormat is the reported failure end
// to end: Claude Code (Anthropic surface) forwarded to an OpenAI chat-completions
// upstream ("Console Go", z.ai).
func TestPrepareUpstreamBodyPairsToolTurnsAcrossFormat(t *testing.T) {
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, claudeCodeToolHistory("and now?"), "glm-4.6", nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := assertOAWireToolPairing(t, out)

	// The repair moves whole turns and never drops or rewords them: each
	// reminder stays its own user turn, and it stays AFTER the results it
	// trailed. toolu_02 is answered only in the turn after toolu_03's, so both
	// replies have to be pulled up into the run behind their assistant turn.
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	wantRoles := strings.Join([]string{
		"system", "user", "assistant", "tool", "user", "assistant", "tool", "tool", "user", "user",
	}, ",")
	if got := strings.Join(roles, ","); got != wantRoles {
		t.Fatalf("message roles = %s, want %s: %s", got, wantRoles, out)
	}
	for _, s := range []string{"stdout was 3 lines", "the user sent a new message", "and now?"} {
		if !strings.Contains(string(out), s) {
			t.Fatalf("client text %q lost: %s", s, out)
		}
	}
}

// TestPrepareUpstreamBodyAnswersInterruptedCall covers the other way a run comes
// up short: the client aborted mid-tool, so the assistant turn carries a call
// nothing ever answered. The request must still be valid wire-wise.
func TestPrepareUpstreamBodyAnswersInterruptedCall(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":20,"messages":[
	  {"role":"user","content":"run it"},
	  {"role":"assistant","content":[
	    {"type":"tool_use","id":"toolu_x","name":"Bash","input":{"command":"sleep 999"}}]},
	  {"role":"user","content":"never mind, do something else"}
	]}`)
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtAnthropic, body, "glm-4.6", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOAWireToolPairing(t, out)
	if !strings.Contains(string(out), `"tool_call_id":"toolu_x"`) {
		t.Fatalf("interrupted call toolu_x got no answer: %s", out)
	}
}

// TestNormalizeToolPairsLeavesShapedHistoryAlone pins the cache-stability half of
// the contract: a history already in wire shape must come back untouched, or the
// repair would rewrite every tool-calling request's prefix and cost the
// provider's prompt cache on each turn.
func TestNormalizeToolPairsLeavesShapedHistoryAlone(t *testing.T) {
	u, err := translat.DecodeOpenAIRequest([]byte(`{"model":"m","messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	  {"role":"tool","content":"out1","tool_call_id":"c1"},
	  {"role":"assistant","content":"done"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := translat.EncodeOpenAIRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	translat.NormalizeToolPairs(u)
	after, err := translat.EncodeOpenAIRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("in-shape history rewritten:\n before %s\n  after %s", before, after)
	}
}

// TestNormalizeToolPairsPlainChatUntouched pins the no-op fast path: a request
// with no tool traffic is not reallocated at all.
func TestNormalizeToolPairsPlainChatUntouched(t *testing.T) {
	msgs := []types.Message{
		{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "hi"}}},
		{Role: types.RoleAssistant, Content: []types.Part{{Type: types.PartText, Text: "hello"}}},
	}
	u := &types.ChatRequest{Model: "m", Messages: msgs}
	translat.NormalizeToolPairs(u)
	if &u.Messages[0] != &msgs[0] {
		t.Fatalf("plain chat was copied into a new slice: %+v", u.Messages)
	}
}
