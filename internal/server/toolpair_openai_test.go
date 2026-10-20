package server

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/translat"
)

// oaPairWire is the OpenAI chat shape the strict upstreams validate.
type oaPairWire struct {
	Messages []struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

// assertOpenAIPaired enforces the invariant every strict tool-calling
// upstream re-checks on the replayed history, in their own words:
//
//	"invalid params, tool call id is invalid (2013)"        (MiniMax)
//	"invalid params, tool result does not follow tool call" (MiniMax)
//	"`messages[N]` tool message must follow an assistant message" (z.ai)
//
// i.e. a role:"tool" message may only appear in the run directly after
// the assistant turn that made the calls, and every call id must be
// answered exactly once inside that run.
func assertOpenAIPaired(t *testing.T, body []byte) {
	t.Helper()
	var wire oaPairWire
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode wire body: %v (%s)", err, body)
	}
	msgs := wire.Messages
	for i, m := range msgs {
		if m.Role == "tool" {
			if i == 0 {
				t.Fatalf("messages[0]: a tool message cannot lead the array: %s", body)
			}
			prev := msgs[i-1]
			if prev.Role != "tool" && !(prev.Role == "assistant" && len(prev.ToolCalls) > 0) {
				t.Fatalf("messages[%d]: tool message must follow an assistant message with tool_calls (predecessor role %q): %s", i, prev.Role, body)
			}
		}
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		want := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			want[tc.ID] = false
		}
		for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
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
}

// TestNormalizeToolPairsOpenAIHealsAStaleCallID is the reported failure, end
// to end at the wire: a tool result carrying the id of an EARLIER turn's
// call. Live 2026-10-20: a coding agent's per-run dedup cache returned a
// cached result verbatim, id included, so the second identical tool call
// was answered with the first one's id, and the upstream answered every
// request with
//
//	HTTP 400 {... "invalid params, tool call id is invalid (2013)" ...}
//
// The same-format path (the client's own OpenAI wire) forwarded the body
// byte-verbatim, so no repair ever ran.
func TestNormalizeToolPairsOpenAIHealsAStaleCallID(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
	  {"role":"user","content":"check the index"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_A","type":"function","function":{"name":"leankg_status","arguments":"{}"}}]},
	  {"role":"tool","content":"{\"elements\":1}","tool_call_id":"call_A"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_B","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	  {"role":"tool","content":"{\"elements\":2}","tool_call_id":"call_A"},
	  {"role":"tool","content":"a.txt","tool_call_id":"call_B"}]}`)

	out := normalizeToolPairsOpenAI(body)
	assertOpenAIPaired(t, out)
	if !strings.Contains(string(out), `"tool_call_id":"call_B"`) {
		t.Fatalf("the live call lost its answer: %s", out)
	}
	if strings.Contains(string(out), `"tool_call_id":"call_A"},{"role":"tool"`) {
		t.Fatalf("the stale duplicate answer survived: %s", out)
	}
}

// TestNormalizeToolPairsOpenAIAnswersAnUnansweredCall is the other defect: a
// call the client was interrupted before answering. The upstream refuses
// the turn outright ("must be followed by tool messages responding to each
// 'tool_call_id'"), and the rejected turn is history, so every retry
// replays it.
func TestNormalizeToolPairsOpenAIAnswersAnUnansweredCall(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
	  {"role":"user","content":"run it"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_X","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	  {"role":"user","content":"never mind"}]}`)

	out := normalizeToolPairsOpenAI(body)
	assertOpenAIPaired(t, out)
	if !strings.Contains(string(out), toolResultMissing) {
		t.Fatalf("the unanswered call got no answer: %s", out)
	}
}

// TestNormalizeToolPairsOpenAIMovesAStrandedResult: the result answers the
// call, but a user turn sits between them. The upstream wants the result
// in the run right after the assistant turn ("tool result does not follow
// tool call"), so the result moves up and the user's turn stays after it.
func TestNormalizeToolPairsOpenAIMovesAStrandedResult(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
	  {"role":"user","content":"run it"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_X","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	  {"role":"user","content":"never mind"},
	  {"role":"tool","content":"done","tool_call_id":"call_X"}]}`)

	out := normalizeToolPairsOpenAI(body)
	assertOpenAIPaired(t, out)
	if !strings.Contains(string(out), `"content":"done"`) {
		t.Fatalf("the result was dropped instead of moved: %s", out)
	}
	if !strings.Contains(string(out), `"content":"never mind"`) {
		t.Fatalf("the user turn was lost: %s", out)
	}
}

// TestNormalizeToolPairsOpenAILeavesAHealthyBodyAlone is the cache-stability
// half of the contract: a history already in wire shape must come back
// byte-identical, or the repair would rewrite every tool-calling request's
// prefix and cost the provider's prompt cache on each turn.
func TestNormalizeToolPairsOpenAILeavesAHealthyBodyAlone(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	  {"role":"tool","content":"out1","tool_call_id":"c1"},
	  {"role":"assistant","content":"done"}]}`)

	if got := normalizeToolPairsOpenAI(body); string(got) != string(body) {
		t.Fatalf("in-shape history rewritten:\n before %s\n  after %s", body, got)
	}
}

// TestNormalizeToolPairsOpenAILeavesPlainChatUntouched: a request with no
// tool traffic is not touched at all.
func TestNormalizeToolPairsOpenAILeavesPlainChatUntouched(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got := normalizeToolPairsOpenAI(body); string(got) != string(body) {
		t.Fatalf("plain chat rewritten:\n before %s\n  after %s", body, got)
	}
}

// TestNormalizeToolPairsOpenAIKeepsTheClientsFields: the repair is
// byte-level, so a rewritten body must still carry every field the client
// sent — the tools, the knobs, the unknown keys.
func TestNormalizeToolPairsOpenAIKeepsTheClientsFields(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.3,"prompt_cache_key":"abc","custom_field":{"x":[1,2]},"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}],"messages":[
	  {"role":"user","content":"run it"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_X","type":"function","function":{"name":"bash","arguments":"{}"}}]}]}`)

	out := normalizeToolPairsOpenAI(body)
	for _, want := range []string{`"temperature":0.3`, `"prompt_cache_key":"abc"`, `"custom_field"`, `"tools"`, `"name":"bash"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("field %s lost in the repair: %s", want, out)
		}
	}
}

// TestPrepareUpstreamBodyRepairsTheSameFormatPath is the wiring test: the
// path a coding agent takes — an OpenAI client surface to an OpenAI
// upstream — must run the repair. Before it, that path forwarded the body
// byte-verbatim and no repair ran at all.
func TestPrepareUpstreamBodyRepairsTheSameFormatPath(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
	  {"role":"user","content":"check the index"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_A","type":"function","function":{"name":"leankg_status","arguments":"{}"}}]},
	  {"role":"tool","content":"{\"elements\":1}","tool_call_id":"call_A"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_B","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	  {"role":"tool","content":"{\"elements\":2}","tool_call_id":"call_A"},
	  {"role":"tool","content":"a.txt","tool_call_id":"call_B"}]}`)

	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "minimax-m3.1-flash-free", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOpenAIPaired(t, out)
}
