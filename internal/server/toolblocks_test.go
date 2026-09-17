package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"onegw/internal/translat"
)

// anWireBlock is the tool_use/tool_result shape an Anthropic-wire upstream
// validates on every replay.
type anWireBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
}

// anWireBlocks flattens every content block of an Anthropic Messages body.
func anWireBlocks(t *testing.T, body []byte) []anWireBlock {
	t.Helper()
	var wire struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode upstream body: %v (%s)", err, body)
	}
	var out []anWireBlock
	for _, m := range wire.Messages {
		var blocks []anWireBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // plain string content
		}
		for _, b := range blocks {
			if b.Type ***REMOVED*** "tool_use" || b.Type ***REMOVED*** "tool_result" {
				out = append(out, b)
			}
		}
	}
	return out
}

// assertAnWireToolBlocks enforces what the upstream re-checks: every tool_use
// carries a non-empty string id and name, and every tool_result names the call
// it answers.
func assertAnWireToolBlocks(t *testing.T, body []byte) []anWireBlock {
	t.Helper()
	blocks := anWireBlocks(t, body)
	if len(blocks) != 2 {
		t.Fatalf("want one tool_use + one tool_result, got %d: %s", len(blocks), body)
	}
	var callID string
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			if b.ID ***REMOVED*** "" || b.Name ***REMOVED*** "" {
				t.Fatalf("tool_use needs string id and name, got id=%q name=%q: %s", b.ID, b.Name, body)
			}
			callID = b.ID
		case "tool_result":
			if b.ToolUseID ***REMOVED*** "" {
				t.Fatalf("tool_result needs a string tool_use_id: %s", body)
			}
			if b.ToolUseID != callID {
				t.Fatalf("tool_result answers %q, tool_use id is %q: %s", b.ToolUseID, callID, body)
			}
		}
	}
	return blocks
}

// poisonedAnthropicHistory is the replayed turn measured live (Claude Code →
// union-alpha, gateway ring seq 1086): an assistant tool_use whose "name" is
// empty, answered by a matching tool_result. Anthropic's validator refuses the
// whole request —
//
//	messages[1]: tool_use blocks require string "id" and "name"
//
// — and because the turn already sits in the client's persisted history, every
// retry 400s at the same index.
const poisonedAnthropicHistory = `{
  "model": "union-alpha",
  "max_tokens": 100,
  "messages": [
    {"role": "user", "content": "hi"},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "call_373ef199ecaf4cdc9b543f62", "name": "", "input": {}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "call_373ef199ecaf4cdc9b543f62",
       "content": "<tool_use_error>Error: No such tool available: </tool_use_error>", "is_error": true}
    ]}
  ]
}`

// TestPrepareUpstreamBodyNamesPoisonedToolUse is the reported failure end to
// end: Claude Code on the Anthropic surface replayed a tool_use with an empty
// name to the Anthropic-wire upstream, and the same-format path forwards those
// bytes verbatim — the repair has to happen in the body.
func TestPrepareUpstreamBodyNamesPoisonedToolUse(t *testing.T) {
	out, err := prepareUpstreamBody(translat.FmtAnthropic, translat.FmtAnthropic,
		[]byte(poisonedAnthropicHistory), "union-alpha", nil)
	if err != nil {
		t.Fatalf("prepareUpstreamBody: %v", err)
	}
	blocks := assertAnWireToolBlocks(t, out)
	if blocks[0].ID != "call_373ef199ecaf4cdc9b543f62" {
		t.Fatalf("a real call id must survive the repair, got %q", blocks[0].ID)
	}
	if blocks[0].Name != unknownToolName {
		t.Fatalf("nameless call must be given the placeholder, got %q", blocks[0].Name)
	}
	// The error text the client persisted is context the model needs: the
	// repair never drops or rewords a block. (Re-marshalling HTML-escapes
	// the angle brackets, so match the prose.)
	if !bytes.Contains(out, []byte("No such tool available")) {
		t.Fatalf("client tool_result text lost: %s", out)
	}
}

// TestPrepareUpstreamBodyNamesToolUseAcrossFormat covers the same defect on the
// translating path (OpenAI chat client → Anthropic-wire upstream): the encoder
// drops an empty name outright (anBlock.Name is omitempty), so the block
// reaches the upstream without the key at all.
func TestPrepareUpstreamBodyNamesToolUseAcrossFormat(t *testing.T) {
	body := []byte(`{"model":"opencode/union-alpha","max_tokens":16,"messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":null,"tool_calls":[
	    {"id":"call_1","type":"function","function":{"name":"","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"x"}
	]}`)
	out, err := prepareUpstreamBody(translat.FmtAnthropic, translat.FmtOpenAI, body, "union-alpha", nil)
	if err != nil {
		t.Fatalf("prepareUpstreamBody: %v", err)
	}
	assertAnWireToolBlocks(t, out)
}

// TestPrepareUpstreamBodyFillsMissingToolUseID: Anthropic demands a string id
// too, and a nameless id-less block would otherwise encode as the degenerate
// "toolu_".
func TestPrepareUpstreamBodyFillsMissingToolUseID(t *testing.T) {
	body := []byte(`{"model":"union-alpha","max_tokens":16,"messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"","name":"","input":{}}]}
	]}`)
	out, err := prepareUpstreamBody(translat.FmtAnthropic, translat.FmtAnthropic, body, "union-alpha", nil)
	if err != nil {
		t.Fatalf("prepareUpstreamBody: %v", err)
	}
	blocks := anWireBlocks(t, out)
	if len(blocks) != 1 || blocks[0].ID ***REMOVED*** "" || blocks[0].ID ***REMOVED*** "toolu_" {
		t.Fatalf("id-less tool_use must get a usable id, got %+v: %s", blocks, out)
	}
}

// TestNormalizeToolBlocksLeavesShapedHistoryAlone pins the cache-stability half
// of the contract, exactly as the tool-root and tool-pair passes do: a history
// the upstream already accepts must come back byte-identical, or the repair
// would cost the provider's prompt cache on every turn.
func TestNormalizeToolBlocksLeavesShapedHistoryAlone(t *testing.T) {
	shaped := map[string]string{
		"named call": `{"model":"m","max_tokens":9,"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"a"}]}]}`,
		"no tool traffic":     `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		"name copy in a text": `{"model":"m","messages":[{"role":"user","content":"the \"name\":\"\" key was empty"}]}`,
	}
	for name, body := range shaped {
		out := normalizeToolBlocks([]byte(body))
		if string(out) != body {
			t.Errorf("%s: accepted shape must stay byte-identical, got %s", name, out)
		}
	}
	// A body with no tool_use anywhere short-circuits on the substring probe:
	// same slice back, never re-marshalled.
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if out := normalizeToolBlocks(plain); &out[0] != &plain[0] {
		t.Error("tool-less body must be returned unchanged, not re-marshalled")
	}
}
