// Tool-block repair for the Anthropic Messages wire.
//
// Anthropic validates the REPLAYED history, not just the newest turn:
//
//	messages[1]: tool_use blocks require string "id" and "name"
//	messages[2]: tool_result blocks require a string "tool_use_id"
//
// Live 2026-08-28 (Claude Code → opencode/union-alpha, gateway ring seq 1086,
// kind=invalid_request_error): one assistant turn in the client's persisted
// history carried `{"type":"tool_use","id":"call_373e…","name":"","input":{}}`
// — the name empty — and the session 400ed at that index on every retry, since
// the poisoned turn is replayed from disk each time. The empty name came from
// this gateway relaying an upstream tool call with no function name
// (wire_stream.go writes `"name": toolName` unconditionally), and the client
// faithfully wrote it to its transcript.
//
// Two sinks reach an Anthropic-wire upstream, and neither could repair it:
//   - same format (Claude Code → /v1/messages): buildUpstreamBody returns the
//     body verbatim, so a literal `"name":""` went out byte-for-byte;
//   - cross-format (OpenAI chat → /v1/messages): the encoder drops the empty
//     name outright (anBlock.Name is omitempty), so the block reached the
//     upstream with no name key at all — the same 400.
//
// Repairing the finished body covers both with one pass. A name is never
// invented from nothing, only replaced by the placeholder the Gemini encoder
// already uses for the same situation (toolNameForMsgs): dropping the block
// would desynchronize the client's `tool_result` and lose history the model
// needs, while a placeholder keeps the pairing, the cache breakpoints and the
// call/answer identity intact. An empty id is filled the same way, from the
// (now non-empty) name — the encoder's own `"toolu_"+Name` fallback degenerates
// to `"toolu_"` when both are empty, and Anthropic requires a string id.
//
// Scope: only tool_use blocks are written. A tool_result with an empty
// tool_use_id is left alone: it is unobserved in 1354 scanned transcripts, and
// answering it would mean guessing which call it belongs to.
//
// Returns body unchanged when there is nothing to fix, so tool-carrying bodies
// that need nothing pay only a cheap substring probe. A body that IS rewritten
// is re-marshalled whole (Go sorts object keys) and so is not byte-faithful to
// the client's original — affordable because the rewrite is deterministic: the
// same poisoned turn produces the same bytes every time, so the cache break
// happens once per session, not per turn.
package server

import (
	"bytes"
	"encoding/json"
)

// unknownToolName is the placeholder a tool call with no name is given. The
// Gemini encoder has used the same string for the same case since 2026-09-11.
const unknownToolName = "unknown_tool"

// normalizeToolBlocks gives every tool_use block a usable id and name.
func normalizeToolBlocks(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"tool_use"`)) {
		return body
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body // not an object; forward verbatim
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return body
	}
	changed := false
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			continue // plain string content
		}
		for _, bv := range blocks {
			b, ok := bv.(map[string]any)
			if !ok || b["type"] != "tool_use" {
				continue
			}
			name, _ := b["name"].(string)
			if name == "" {
				name = unknownToolName
				b["name"] = name
				changed = true
			}
			if id, _ := b["id"].(string); id == "" {
				b["id"] = "toolu_" + name
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}
