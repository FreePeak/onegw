package translat

import (
	"strings"
	"testing"

	"onegw/internal/types"
)

// Live 2026-09-10: commandcode /provider/v1 responses carry reasoning as
// "reasoning" + "reasoning_details":[{type:"reasoning.text",...}] (tee
// capture), and pi-ai replays those verbatim (openai-completions.js:1043).
// The gateway must flatten every alias into PartThinking so the thinking
// echo the DeepSeek thinking mode demands survives the unified round-trip —
// EncodeOpenAIRequest emits it back as reasoning_content.
func TestDecodeOpenAIRequestReasoningAliases(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"reasoning":"Vendor reasoning field","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"},
		{"role":"assistant","content":"done","reasoning_content":"Native echo field"},
		{"role":"assistant","content":"alt","reasoning_text":"reasoning_text alias"},
		{"role":"assistant","content":"arr","reasoning_details":[{"type":"reasoning.text","text":"detail one","format":"unknown","index":0},{"type":"reasoning.text","text":"detail two"}]}
	]}`
	u, err := DecodeOpenAIRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Vendor reasoning field", "Native echo field", "reasoning_text alias", "detail one\ndetail two"}
	var got []string
	for _, m := range u.Messages {
		if m.Role != types.RoleAssistant {
			continue
		}
		for _, p := range m.Content {
			if p.Type == types.PartThinking {
				got = append(got, p.Text)
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("thinking parts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("thinking[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Round-trip: the unified thinking re-encodes as the native
	// reasoning_content the thinking-mode contract demands.
	out, err := EncodeOpenAIRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, frag := range []string{`"reasoning_content":"Vendor reasoning field"`, `"reasoning_content":"detail one\ndetail two"`} {
		if !strings.Contains(s, frag) {
			t.Fatalf("re-encoded body missing %s:\n%s", frag, s)
		}
	}
}

// A reasoning-free assistant turn must stay reasoning-free (no invented echo).
func TestDecodeOpenAIRequestNoReasoningInvented(t *testing.T) {
	u, err := DecodeOpenAIRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Hello there friend"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range u.Messages {
		for _, p := range m.Content {
			if p.Type == types.PartThinking {
				t.Fatalf("invented thinking part: %+v", p)
			}
		}
	}
}

// Buffered pipeline (saver on → no stream passthrough): an upstream chunk
// carrying the "reasoning" alias must surface as a thinking delta.
func TestDecodeOpenAIStreamReasoningAlias(t *testing.T) {
	evs, err := decodeStreamEvent(FmtOpenAI, sseEvent{Data: []byte(`{"id":"g1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning":"thinking via alias"},"finish_reason":null}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Kind == EvDelta && e.PartType == types.PartThinking && e.Thinking == "thinking via alias" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning alias delta lost: %+v", evs)
	}
}

// Non-stream responses from AI-SDK resellers carry reasoning under the
// vendor aliases (tee 001 live capture: commandcode answers with BOTH
// "reasoning" and "reasoning_details"): the response decoder must surface
// them as thinking, not drop them.
func TestDecodeOpenAIResponseReasoningAlias(t *testing.T) {
	resp, err := DecodeOpenAIResponse([]byte(`{"id":"g1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning":"thinking via alias"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range resp.Content {
		if p.Type == types.PartThinking && p.Text == "thinking via alias" {
			found = true
		}
	}
	if !found {
		t.Fatalf("response reasoning alias dropped: %+v", resp.Content)
	}

	resp2, err := DecodeOpenAIResponse([]byte(`{"id":"g2","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi","reasoning_details":[{"type":"reasoning.text","text":"detail text"}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	found2 := false
	for _, p := range resp2.Content {
		if p.Type == types.PartThinking && p.Text == "detail text" {
			found2 = true
		}
	}
	if !found2 {
		t.Fatalf("response reasoning_details dropped: %+v", resp2.Content)
	}
}
