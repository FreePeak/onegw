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

// anWireTextBlock is the shape the reported failure names: the upstream reads
// a block's type before it reports anything missing, so a key-less text block
// is refused AS "text".
type anWireTextBlock struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

// assertAnWireTextBlocks enforces what the upstream re-checks on an assistant
// turn: every text block carries a string text, empty or not.
func assertAnWireTextBlocks(t *testing.T, body []byte) {
	t.Helper()
	var wire struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode upstream body: %v (%s)", err, body)
	}
	for i, m := range wire.Messages {
		var blocks []anWireTextBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // plain string content
		}
		for _, b := range blocks {
			if b.Type != "text" || m.Role != "assistant" {
				continue
			}
			if b.Text == nil {
				t.Fatalf("messages[%d]: text block reached the upstream with no string text: %s", i, body)
			}
		}
	}
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
			if b.Type == "tool_use" || b.Type == "tool_result" {
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
			if b.ID == "" || b.Name == "" {
				t.Fatalf("tool_use needs string id and name, got id=%q name=%q: %s", b.ID, b.Name, body)
			}
			callID = b.ID
		case "tool_result":
			if b.ToolUseID == "" {
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

// keylessAssistantText is the replayed turn measured live 2026-09-17 (gateway
// ring seq 3404): an assistant text block with no text key at all. The upstream
// refuses the whole request —
//
//	messages[1]: unsupported assistant content block type "text"
//
// — and the turn is replayed from the client's transcript on every retry.
const keylessAssistantText = `{
  "model": "union-alpha",
  "max_tokens": 100,
  "messages": [
    {"role": "user", "content": "hi"},
    {"role": "assistant", "content": [{"type": "text"}]},
    {"role": "user", "content": "keep going"}
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

// TestPrepareUpstreamBodyCompletesKeylessAssistantText is the second reported
// failure end to end, same sink: the same-format path forwards the client's
// bytes verbatim, so the repair has to happen in the body.
func TestPrepareUpstreamBodyCompletesKeylessAssistantText(t *testing.T) {
	out, err := prepareUpstreamBody(translat.FmtAnthropic, translat.FmtAnthropic,
		[]byte(keylessAssistantText), "union-alpha", nil)
	if err != nil {
		t.Fatalf("prepareUpstreamBody: %v", err)
	}
	assertAnWireTextBlocks(t, out)
	// The block is completed in place, never dropped: an assistant turn with
	// no content at all is refused by the same upstream.
	if !bytes.Contains(out, []byte(`{"text":"","type":"text"}`)) {
		t.Fatalf("key-less text block not completed in place: %s", out)
	}
	if bytes.Contains(out, []byte(`"content":[]`)) {
		t.Fatalf("assistant turn lost its content: %s", out)
	}
}

// TestPrepareUpstreamBodyCompletesEmptyAssistantTurnCrossFormat covers the
// encoder sink: an OpenAI assistant turn whose content is "" or null encodes
// as the same key-less block (anBlock.Text is omitempty), measured live
// 2026-09-17 through the gateway as the identical 400.
func TestPrepareUpstreamBodyCompletesEmptyAssistantTurnCrossFormat(t *testing.T) {
	for _, resp := range []string{`""`, `null`} {
		body := []byte(`{"model":"opencode/union-alpha","max_tokens":16,"messages":[` +
			`{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":` + resp + `},` +
			`{"role":"user","content":"x"}]}`)
		out, err := prepareUpstreamBody(translat.FmtAnthropic, translat.FmtOpenAI, body, "union-alpha", nil)
		if err != nil {
			t.Fatalf("content=%s: %v", resp, err)
		}
		assertAnWireTextBlocks(t, out)
	}
}

// TestPrepareUpstreamBodyReplacesNonStringAssistantText: the upstream refuses a
// text block whose text is not a string just as it refuses a missing key, so a
// null/number text takes the same repair.
func TestPrepareUpstreamBodyReplacesNonStringAssistantText(t *testing.T) {
	for _, resp := range []string{`null`, `123`, `{"a":1}`} {
		body := []byte(`{"model":"union-alpha","max_tokens":16,"messages":[` +
			`{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"text","text":` + resp + `}]},` +
			`{"role":"user","content":"x"}]}`)
		out, err := prepareUpstreamBody(translat.FmtAnthropic, translat.FmtAnthropic, body, "union-alpha", nil)
		if err != nil {
			t.Fatalf("text=%s: %v", resp, err)
		}
		assertAnWireTextBlocks(t, out)
	}
}

// TestPrepareUpstreamBodyLeavesUserTextBlockAlone pins the scope: the user role
// carries a differently-worded refusal and no live report, and a trailing
// "system-reminder" turn must not be rewritten on speculation.
func TestPrepareUpstreamBodyLeavesUserTextBlockAlone(t *testing.T) {
	in := `{"model":"union-alpha","max_tokens":16,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":[{"type":"text"}]}]}`
	out, err := prepareUpstreamBody(translat.FmtAnthropic, translat.FmtAnthropic, []byte(in), "union-alpha", nil)
	if err != nil {
		t.Fatalf("prepareUpstreamBody: %v", err)
	}
	if string(out) != in {
		t.Fatalf("a user-side key-less text block must be forwarded verbatim, got %s", out)
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
	if len(blocks) != 1 || blocks[0].ID == "" || blocks[0].ID == "toolu_" {
		t.Fatalf("id-less tool_use must get a usable id, got %+v: %s", blocks, out)
	}
}

// TestNormalizeToolBlocksCompletesTextOnly covers the text repair without the
// tool traffic the probe is built around: a text-only body must still be
// repaired when it reaches the pass.
func TestNormalizeToolBlocksCompletesTextOnly(t *testing.T) {
	in := `{"model":"m","max_tokens":9,"messages":[{"role":"assistant","content":[{"type":"text"}]}]}`
	out := normalizeToolBlocks([]byte(in))
	assertAnWireTextBlocks(t, out)
	if !bytes.Contains(out, []byte(`"text":""`)) {
		t.Fatalf("key-less text block not completed: %s", out)
	}
}

// TestNormalizeToolBlocksLeavesEmptyStringTextAlone: an empty string is
// accepted by the upstream, so rewriting it would cost the provider's prompt
// cache on every turn for a history that is already fine.
func TestNormalizeToolBlocksLeavesEmptyStringTextAlone(t *testing.T) {
	in := `{"model":"m","max_tokens":9,"messages":[
		{"role":"assistant","content":[{"type":"text","text":""}]},
		{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`
	if out := normalizeToolBlocks([]byte(in)); string(out) != in {
		t.Fatalf("an accepted empty text must stay byte-identical, got %s", out)
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
		"empty assistant text (accepted as-is)": `{"model":"m","messages":[
			{"role":"assistant","content":[{"type":"text","text":""}]}]}`,
		"user key-less text (not this repair's scope)": `{"model":"m","messages":[
			{"role":"user","content":[{"type":"text"}]}]}`,
	}
	for name, body := range shaped {
		out := normalizeToolBlocks([]byte(body))
		if string(out) != body {
			t.Errorf("%s: accepted shape must stay byte-identical, got %s", name, out)
		}
	}
	// A body matching neither repair's literal short-circuits on the substring
	// probe: same slice back, never re-marshalled.
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if out := normalizeToolBlocks(plain); &out[0] != &plain[0] {
		t.Error("probe-less body must be returned unchanged, not re-marshalled")
	}
}
