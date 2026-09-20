// Content-block repair for the Anthropic Messages wire.
//
// Anthropic validates the REPLAYED history, not just the newest turn:
//
//	messages[1]: unsupported assistant content block type "text"
//	messages[1]: tool_use blocks require string "id" and "name"
//	messages[2]: tool_result blocks require a string "tool_use_id"
//
// Two classes of poisoned turn reach this pass. Both are repaired here rather
// than surfaced, because the offending turn already sits in the client's
// persisted transcript: every retry rebuilds the same rejected request.
//
//  1. A key-less assistant text block. Live 2026-09-17 (the reported failure,
//     gateway ring seq 3404, kind=invalid_request_error): an assistant turn in
//     the client's history carried `{"type":"text"}` — the type present, the
//     text key gone — and the session 400ed at that index on every retry. The
//     upstream reads .type first, so it named the block instead of reporting
//     the missing field. Both sinks write that shape: a same-format body is
//     forwarded byte-verbatim, and the cross-format encoder renders a text part
//     with no text as `{"type":"text"}` (anBlock.Text is omitempty — live
//     repro 2026-09-17 with an OpenAI client sending `"content":""` or
//     `"content":null` on an assistant turn). Measured live on union-alpha:
//     `{"type":"text","text":""}` 200; `{"type":"text"}`, `"text":null` and
//     `"text":123` all 400 with that exact message. A missing "type" 400s as
//     "undefined", a bare string block as "undefined", and an assistant-side
//     tool_result as "tool_result".
//
//  2. A nameless tool_use. Live 2026-08-28 (Claude Code → opencode/union-alpha,
//     gateway ring seq 1086): one assistant turn carried
//     `{"type":"tool_use","id":"call_373e…","name":"","input":{}}` — the name
//     empty. The empty name came from this gateway relaying an upstream tool
//     call with no function name (wire_stream.go writes `"name": toolName`
//     unconditionally), and the client wrote it to its transcript. Neither
//     sink could repair it: same-format bodies go out byte-for-byte, and
//     cross-format the encoder drops an empty name outright (anBlock.Name is
//     omitempty), so the block reached the upstream with the key absent — the
//     same 400.
//
// Repair 1 only ever completes a block, never drops one: the block keeps its
// position, so a turn the upstream would also refuse for an empty content
// array never loses its last content, and the tool call/result pairing repair
// 2 depends on stays intact. Only assistant text is completed — the user role
// carries a second, differently-worded refusal ("unsupported user content
// block type") no live report has ever shown, and a "system-reminder" turn is
// often the LAST message, where completing a block instead of forwarding it is
// not worth speculating about. Repair 2 invents a name only when there is none
// to keep, replacing it with the placeholder the Gemini encoder already uses
// for the same situation (toolNameForMsgs): dropping the block would
// desynchronize the client's `tool_result` and lose history the model needs,
// while a placeholder keeps the pairing, the cache breakpoints and the
// call/answer identity intact. An empty id is filled the same way, from the
// (now non-empty) name — the encoder's own `"toolu_"+Name` fallback degenerates
// to `"toolu_"` when both are empty, and Anthropic requires a string id.
//
// Untouched: a tool_result with an empty tool_use_id (unobserved in 1354
// scanned transcripts, and answering it would mean guessing which call it
// belongs to), and any other key-less block type, whose completion no live
// probe supports.
//
// Returns body unchanged when there is nothing to fix, so a body that needs
// nothing pays only a cheap substring probe. A body that IS rewritten
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

// normalizeToolBlocks completes the blocks Anthropic re-validates on replay:
// every tool_use carries a usable id and name, and every key-less assistant
// text block carries its (empty) text.
func normalizeToolBlocks(body []byte) []byte {
	// Both repairs are keyed on the block shapes this gateway writes itself: a
	// tool_use block, or a text block whose text value is absent (anBlock.Text
	// is omitempty, so `{"type":"text"}` is the whole block). The probe is
	// deliberately not "does this body contain a text block" — almost every
	// body does, and the decode below would then cost every request (~0.4 ms
	// per 100 KB measured) to repair the rare poisoned one.
	if !bytes.Contains(body, []byte(`"tool_use"`)) && !textBlockWithoutString(body) {
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
			if !ok {
				continue
			}
			switch b["type"] {
			case "tool_use":
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
			case "text":
				if m["role"] != "assistant" {
					continue
				}
				// A string text is already accepted, empty or not: left
				// alone, so an accepted history stays byte-identical. A
				// non-string, or no key at all, is the refusal.
				if _, has := b["text"]; has {
					if _, isStr := b["text"].(string); isStr {
						continue
					}
				}
				b["text"] = ""
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
