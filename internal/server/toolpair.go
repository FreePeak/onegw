// Tool-call pairing repair for the OpenAI chat wire.
//
// The OpenAI wire pairs a tool result with its call BY ID, and strict
// upstreams validate the pairing on the REPLAYED history, not just the
// newest turn:
//
//	"invalid params, tool call id is invalid (2013)"          (MiniMax, via opencode Zen)
//	"invalid params, tool result does not follow tool call"   (MiniMax, via opencode Zen)
//	"`messages[N]` tool message must follow an assistant message"       (z.ai)
//	"An assistant message with 'tool_calls' must be followed by tool
//	 messages responding to each 'tool_call_id'"                (opencode "Console Go")
//
// Two shapes of a client history reach the upstream broken:
//
//  1. A tool result that answers NO live call — its tool_call_id names a
//     call from an EARLIER turn (a stale id) or no call at all. Live
//     2026-10-20 through this gateway: a coding agent's per-run dedup
//     cache returned a cached result verbatim, id included, so the
//     second identical tool call was answered with the FIRST call's id.
//     The upstream refused the request with the 2013 verdict above, and
//     refused every retry too, because the poisoned turn is history.
//
//  2. A tool call that NOTHING answers — the client was interrupted
//     between the call and its result, so the assistant turn carries a
//     call no result replies to.
//
// The unified repair (translat.NormalizeToolPairs) closes both gaps, but
// it only runs on the CROSS-FORMAT path (server.encodeFor). A client
// that speaks the upstream's own wire — the OpenAI chat surface of this
// gateway, which is what a coding agent uses — takes the same-format
// path, where the body was forwarded byte-verbatim and NO repair ran.
//
// This is the same-format repair, and it is byte-level on purpose: the
// same-format path exists to keep the client's exact bytes (the
// provider's prompt cache rides that prefix), so a healthy body must
// come back untouched. The probe is a substring test for a tool result,
// so a body with no tool traffic costs one scan and nothing else. A body
// that IS rewritten is re-marshalled whole (Go sorts object keys) and so
// is not byte-faithful to the client's original — affordable because the
// rewrite is deterministic: the same poisoned turn produces the same
// bytes every time, so the cache break happens once per session, not per
// turn (the same argument normalizeToolBlocks makes for the Anthropic
// wire).
package server

import (
	"bytes"
	"encoding/json"
)

// toolResultMissing is the placeholder for a call nothing answered. The
// same wording the unified repair uses (translat.toolResultMissing), so
// the two paths answer an unanswered call the same way.
const toolResultMissing = "[no tool result recorded]"

// normalizeToolPairsOpenAI repairs the tool-call pairing of an OpenAI chat
// body: every result answers a call of the assistant turn it follows, and
// every call that turn made is answered exactly once.
//
// Returns the body unchanged when there is nothing to fix — the fast path
// every healthy request takes, and the one that keeps the client's bytes
// (and the provider's prompt cache) intact.
func normalizeToolPairsOpenAI(body []byte) []byte {
	// The probe is the cheapest question that can rule the repair out: is
	// there any tool traffic at all? A CALL is the half that can be
	// unanswered ("tool_calls"), a RESULT the half that can answer no
	// call ("tool_call_id"), and the repair covers both — so a body with
	// neither has nothing to fix and never pays the decode below.
	if !bytes.Contains(body, []byte(`"tool_calls"`)) && !bytes.Contains(body, []byte(`"tool_call_id"`)) {
		return body // no tool traffic at all
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
	repaired, changed := repairOpenAIToolPairs(msgs)
	if !changed {
		return body // already in shape: the client's bytes stand
	}
	root["messages"] = repaired
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// oaPairTurn is one assistant turn's calls and the results that answer
// them.
type oaPairTurn struct {
	idx int      // index of the assistant message
	ids []string // the call ids it asked for, in call order
	// answered maps a call id to the index of the result that answers it.
	answered map[string]int
	// run holds the result indices that sit directly after the assistant
	// turn (the contiguous run). A result that answers a call but sits
	// behind another turn is stranded: the upstream wants it in the run.
	stranded map[string]int
}

// repairOpenAIToolPairs rewrites an OpenAI message list into the shape the
// strict upstreams validate. The second return value reports whether
// anything changed: a body already in shape must not be rewritten.
func repairOpenAIToolPairs(msgs []any) ([]any, bool) {
	turns := map[int]*oaPairTurn{}   // assistant message index -> its turn
	byID := map[string]*oaPairTurn{} // call id -> the turn that asked for it
	cur := (*oaPairTurn)(nil)
	inRun := false
	changed := false

	for i, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		switch m["role"] {
		case "assistant":
			ids := openAIcallIDs(m)
			if len(ids) == 0 {
				cur, inRun = nil, false
				continue
			}
			t := &oaPairTurn{idx: i, ids: ids, answered: map[string]int{}, stranded: map[string]int{}}
			turns[i] = t
			for _, id := range ids {
				byID[id] = t
			}
			cur, inRun = t, true
		case "tool":
			id, _ := m["tool_call_id"].(string)
			t, known := byID[id]
			if !known || cur != t {
				// Answers no live call: a stale id from an earlier turn,
				// or an id no turn ever asked for.
				changed = true
				continue
			}
			if _, dup := t.answered[id]; dup {
				changed = true // a second answer for the same call
				continue
			}
			t.answered[id] = i
			if inRun {
				continue
			}
			t.stranded[id] = i
			changed = true // it has to move up into the run
		default:
			inRun = false // the run of results ends here
		}
	}

	// An assistant turn whose calls nothing answered is the other defect:
	// the upstream refuses the turn outright ("must be followed by tool
	// messages responding to each 'tool_call_id'").
	for _, t := range turns {
		for _, id := range t.ids {
			if _, ok := t.answered[id]; !ok {
				changed = true
			}
		}
	}
	if !changed {
		return msgs, false
	}

	// Rebuild: every turn answers its calls in CALL ORDER (the run's
	// results first, a stranded result moved up, a placeholder for a call
	// nothing answered), and a result is emitted exactly once.
	emitted := map[int]bool{}
	out := make([]any, 0, len(msgs)+4)
	for i, mv := range msgs {
		t, isTurn := turns[i]
		if !isTurn {
			if isOpenAIToolResult(mv) {
				continue // emitted with its turn, or dropped as misfiled
			}
			out = append(out, mv)
			continue
		}
		out = append(out, mv)
		for _, id := range t.ids {
			idx, ok := t.answered[id]
			if !ok {
				out = append(out, map[string]any{
					"role": "tool", "tool_call_id": id, "content": toolResultMissing,
				})
				continue
			}
			if emitted[idx] {
				continue
			}
			emitted[idx] = true
			out = append(out, msgs[idx])
		}
	}
	return out, true
}

// openAIcallIDs reads the call ids of an assistant message, in order.
func openAIcallIDs(m map[string]any) []string {
	calls, ok := m["tool_calls"].([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, cv := range calls {
		c, ok := cv.(map[string]any)
		if !ok {
			continue
		}
		id, _ := c["id"].(string)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// isOpenAIToolResult reports whether a message is a tool result.
func isOpenAIToolResult(mv any) bool {
	m, ok := mv.(map[string]any)
	if !ok {
		return false
	}
	role, _ := m["role"].(string)
	return role == "tool"
}
