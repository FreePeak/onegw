package translat

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/types"
)

// assertToolPairing enforces the message-sequence invariant that strict
// OpenAI-shape upstreams validate, phrased in their own words:
//
//	"`messages[N]` tool message must follow an assistant message"
//	"An assistant message with 'tool_calls' must be followed by tool messages
//	 responding to each 'tool_call_id'"
//
// i.e. a role:"tool" message may only appear in the run right after the
// assistant turn that made the calls, and each of those call ids is answered
// exactly once inside that run.
func assertToolPairing(t *testing.T, body []byte) {
	t.Helper()
	var wire struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode wire body: %v (%s)", err, body)
	}
	for i, m := range wire.Messages {
		if m.Role ***REMOVED*** "tool" {
			if i ***REMOVED*** 0 {
				t.Fatalf("messages[0]: tool message has no predecessor at all: %s", body)
			}
			prev := wire.Messages[i-1]
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
		for j := i + 1; j < len(wire.Messages) && wire.Messages[j].Role ***REMOVED*** "tool"; j++ {
			id := wire.Messages[j].ToolCallID
			answered, known := want[id]
			if !known {
				t.Fatalf("messages[%d]: tool reply for call id %q, which no assistant turn asked for: %s", j, id, body)
			}
			if answered {
				t.Fatalf("messages[%d]: duplicate tool reply for call id %q: %s", j, id, body)
			}
			want[id] = true
		}
		for id, answered := range want {
			if !answered {
				t.Fatalf("messages[%d]: assistant tool_call %q is not followed by a tool message: %s", i, id, body)
			}
		}
	}
}

// TestEncodeOpenAIRequestMixedToolResultTurn is the Claude Code shape that
// broke live on 2026-09-14: a user turn answering tool_use blocks AND carrying
// trailing <system-reminder> text. Translating that turn's text into a user
// message before the tool replies left the tool messages stranded behind a
// user message, and every strict upstream 400'd the whole request — which
// Claude Code then replayed forever, because the poisoned turn is history.
func TestEncodeOpenAIRequestMixedToolResultTurn(t *testing.T) {
	body := []byte(`{
	  "model": "claude-code",
	  "max_tokens": 100,
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": [
	      {"type": "text", "text": "let me check"},
	      {"type": "tool_use", "id": "toolu_01", "name": "Bash", "input": {"command": "ls"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_01", "content": "a\nb\nc"},
	      {"type": "text", "text": "<system-reminder>stdout was 3 lines</system-reminder>"}
	    ]},
	    {"role": "assistant", "content": [
	      {"type": "tool_use", "id": "toolu_02", "name": "Read", "input": {"file": "a"}},
	      {"type": "tool_use", "id": "toolu_03", "name": "Read", "input": {"file": "b"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_02", "content": "A"},
	      {"type": "tool_result", "tool_use_id": "toolu_03", "content": "B"},
	      {"type": "text", "text": "<system-reminder>2 files read</system-reminder>"}
	    ]},
	    {"role": "user", "content": "and now?"}
	  ]
	}`)
	u, err := DecodeAnthropicRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeOpenAIRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	assertToolPairing(t, out)

	// Repairing the order must not lose the client's text or reorder it
	// against itself: the reminder stays a user turn, after its tool replies.
	var roles []string
	var userText strings.Builder
	var wire struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	for _, m := range wire.Messages {
		roles = append(roles, m.Role)
		if m.Role ***REMOVED*** "user" {
			userText.Write(m.Content)
		}
	}
	want := []string{"user", "assistant", "tool", "user", "assistant", "tool", "tool", "user", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("message order = %v, want %v: %s", roles, want, out)
	}
	for _, s := range []string{"stdout was 3 lines", "2 files read", "and now?"} {
		if !strings.Contains(userText.String(), s) {
			t.Fatalf("user text %q lost: %s", s, out)
		}
	}
}

// TestEncodeCommandCodeRequestMixedToolResultTurn pins the same invariant on
// the commandcode wire, which splits tool results out of a user turn too.
func TestEncodeCommandCodeRequestMixedToolResultTurn(t *testing.T) {
	u := &types.ChatRequest{
		Model:     "zai-org/GLM-5",
		MaxTokens: 64,
		Messages: []types.Message{
			{Role: types.RoleAssistant, Content: []types.Part{
				{Type: types.PartToolUse, ID: "c1", Name: "ls", Args: json.RawMessage(`{"path":"/"}`)},
			}},
			{Role: types.RoleUser, Content: []types.Part{
				{Type: types.PartToolResult, ToolUseID: "c1", Text: "a.txt"},
				{Type: types.PartText, Text: "<system-reminder>1 file</system-reminder>"},
			}},
		},
	}
	out, err := EncodeCommandCodeRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Params struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type       string `json:"type"`
					ToolCallID string `json:"toolCallId"`
					Text       string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range sent.Params.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "assistant,tool,user" {
		t.Fatalf("message roles = %s, want assistant,tool,user: %s", got, out)
	}
}
