package server

import (
	"bytes"
	"encoding/json"

	"onegw/internal/translat"
)

// Context-window overflow recovery (live 2026-09-14, `free` combo): a client
// session grew to 432,168 tokens, no free leg holds a window that large, and
// every combo target answered 400 "The input (432168 tokens) is longer than
// the model's context length (262144 tokens)." — a terminal answer the router
// correctly fell through but could not solve. This file recovers the request
// instead: when the whole chain still refuses, the gateway drops the OLDEST
// complete conversation units from the client body until the input fits the
// window the upstream just reported, then replays the combo chain once. The
// agent session survives on recent context instead of dying with a 400.
//
// Pruning rules (invariants the upstreams enforce):
//   - only whole messages before a safe cut point are removed, so an
//     assistant tool_calls turn NEVER parts from its tool results;
//   - leading system/developer turns and the top-level system/tools fields
//     are never touched;
//   - the kept history starts at a user turn (Anthropic requires it, z.ai
//     rejects an orphaned tool answer outright — the cut scan refuses a head
//     whose call was dropped);
//   - everything outside `messages` is preserved verbatim (the body is
//     re-encoded from the decoded map with json.Number fidelity, same
//     machinery as cacheanchor's reencodeRoot), so knobs like
//     stream_options survive the rewrite and usage accounting keeps working.
//
// The budget uses the refusal's OWN measured input when the dialect carries
// one (z.ai/OpenAI/Anthropic all do): tokens are assumed proportional to
// bytes, which cancels the gateway's bytes/4 estimator error on CJK-heavy
// bodies. Without a measured count the estimate keeps a 25% haircut
// (overflowEstFactor) as the CJK ceiling.
//
// Scope: the OpenAI and Anthropic chat surfaces (where agentic sessions
// actually overflow). The Gemini surface keeps the honest 400.

const (
	// overflowFallbackWindow: used when the refusal names no number. 128k is
	// under every chat model's window seen in this gateway's history, so a
	// pruned-to-fit request clears even the narrowest leg.
	overflowFallbackWindow = 128_000
	// overflowDefaultOut / overflowOutSlack: output headroom subtracted from
	// the window before budgeting input. DefaultOut applies when the body
	// carries no max-tokens knob; slack covers thinking/reasoning tokens the
	// knob does not.
	overflowDefaultOut = 8_192
	overflowOutSlack   = 2_048
	// overflowEstFactor: safety multiplier when the refusal gives only a
	// window (no measured input). 0.75 keeps a 4-bytes-per-token estimate
	// honest for CJK-dense bodies (real tokens ≈ bytes/3 there).
	overflowEstFactor = 0.75
	// overflowMaxEncodes: cap on re-encode attempts while walking deeper
	// cuts. The per-message byte cost model is within whitespace noise of
	// the real encoding; two or three tries converge, eight is paranoia.
	overflowMaxEncodes = 8
)

// pruneToFit returns a rewritten body whose input estimate fits the window
// named by the overflow refusal (window<=0 → conservative fallback; measured
// is the refusal's own input count, 0 when it carries none), or nil when the
// body cannot be made to fit (unparseable, unsupported format, nothing
// removable, or the pinned system+tools+tail alone exceed the budget). A nil
// answer leaves the original honest 400 in place — recovery must never send a
// body that still overflows.
func pruneToFit(body []byte, f translat.Format, window, measured int) []byte {
	if f != translat.FmtOpenAI && f != translat.FmtAnthropic {
		return nil
	}
	if window <= 0 {
		window = overflowFallbackWindow
	}
	// Reserve output: read the client's own max-tokens knob off the body.
	out := peekMaxOutput(body)
	if out <= 0 {
		out = overflowDefaultOut
	}
	budgetTokens := window - out - overflowOutSlack
	if budgetTokens < 4_096 {
		return nil // window cannot hold even a small turn: don't bother
	}
	var budgetBytes int64
	if measured > 0 {
		budgetBytes = int64(float64(len(body)) * float64(budgetTokens) / float64(measured))
	} else {
		budgetBytes = int64(float64(budgetTokens) * 4 * overflowEstFactor)
		// The upstream proved THIS body overflows but named no input count,
		// so the bytes/4 estimator cannot be trusted (digit-dense BPE text
		// runs ~2x its tokens: live 2026-09-14, a 2.8MB refusal against a
		// 983,616-token window came back "Range of input length"). Cap the
		// budget at half the body: every unknown-density retry at least
		// halves the prompt, and known-friendly bodies are governed by the
		// (usually smaller) formula anyway.
		if half := int64(len(body)) / 2; budgetBytes > half {
			budgetBytes = half
		}
	}
	if budgetBytes >= int64(len(body)) {
		// Already under budget by the body's own byte count: the refusal
		// indicts something pruning can't touch. Let the 400 stand.
		return nil
	}

	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) ***REMOVED*** 0 {
		return nil
	}

	// OpenAI surface: hoist pinned leading system/developer turns so they
	// survive every cut. (Anthropic keeps its system prompt in a top-level
	// field that pruning never touches.)
	var lead []any
	if f ***REMOVED*** translat.FmtOpenAI {
		k := 0
		for k < len(msgs) {
			role := msgRole(msgs[k])
			if role != "system" && role != "developer" {
				break
			}
			k++
		}
		lead, msgs = msgs[:k:k], msgs[k:]
	}

	// Cheapest satisfying cut = first one dropping enough bytes. The final
	// byte-budget check inside tryEncode is the arbiter: if the byte cost
	// model undercounted, walk to deeper cuts.
	want := int64(len(body)) - budgetBytes
	tries := 0
	for _, c := range conversationCuts(msgs, f) {
		if c.drop < want {
			continue
		}
		if tries++; tries > overflowMaxEncodes {
			return nil
		}
		if nb := tryEncode(root, lead, msgs, c.at, budgetBytes); nb != nil {
			return nb
		}
	}
	return nil
}

type cut struct {
	at   int   // index the kept history starts at
	drop int64 // wire bytes removed by cutting here
}

// conversationCuts returns every index c where dropping msgs[0:c] keeps
// tool-call pairing intact and lands on a user head, in ascending order with
// the cumulative dropped-byte cost of msgs[0:c].
func conversationCuts(msgs []any, f translat.Format) []cut {
	open := map[string]struct{}{}
	var cuts []cut
	var sum int64
	for i, mv := range msgs {
		// State of `open` here = tool calls opened in msgs[0:i] and not yet
		// answered: the exact condition for a safe cut at i.
		if len(open) ***REMOVED*** 0 && msgRole(mv) ***REMOVED*** "user" {
			cuts = append(cuts, cut{at: i, drop: sum})
		}
		for _, id := range openedCalls(mv, f) {
			open[id] = struct{}{}
		}
		for _, id := range answeredCalls(mv, f) {
			delete(open, id)
		}
		b, _ := json.Marshal(mv)
		sum += int64(len(b))
	}
	return cuts
}

// tryEncode rebuilds the body with lead + msgs[cut:] and returns it when the
// final byte-budget check passes; nil means "this cut is not deep enough"
// (or nothing was removed) and the caller walks to the next cut.
func tryEncode(root map[string]any, lead, msgs []any, cut int, budgetBytes int64) []byte {
	if cut <= 0 || cut >= len(msgs) {
		return nil
	}
	kept := make([]any, 0, len(lead)+len(msgs)-cut)
	kept = append(kept, lead...)
	kept = append(kept, msgs[cut:]...)
	root["messages"] = kept
	nb := reencodeRoot(root, nil)
	if nb ***REMOVED*** nil || int64(len(nb)) > budgetBytes {
		return nil
	}
	return nb
}

// msgRole reads a message's role defensively.
func msgRole(m any) string {
	mm, ok := m.(map[string]any)
	if !ok {
		return ""
	}
	r, _ := mm["role"].(string)
	return r
}

// openedCalls lists tool_use ids the message introduces.
func openedCalls(m any, f translat.Format) []string {
	mm, ok := m.(map[string]any)
	if !ok {
		return nil
	}
	switch f {
	case translat.FmtOpenAI:
		if msgRole(m) != "assistant" {
			return nil
		}
		var ids []string
		if tcs, _ := mm["tool_calls"].([]any); tcs != nil {
			for _, tv := range tcs {
				if t, _ := tv.(map[string]any); t != nil {
					if id, _ := t["id"].(string); id != "" {
						ids = append(ids, id)
					}
				}
			}
		}
		// Legacy function_call has no id: a sentinel keeps the cut from
		// landing between it and its "function" reply.
		if fc, _ := mm["function_call"].(map[string]any); fc != nil {
			if _, has := fc["name"]; has {
				ids = append(ids, "\x00function_call")
			}
		}
		return ids
	default: // FmtAnthropic: tool_use blocks in an assistant turn
		if msgRole(m) != "assistant" {
			return nil
		}
		var ids []string
		blocks, _ := mm["content"].([]any)
		for _, bv := range blocks {
			if b, _ := bv.(map[string]any); b != nil && b["type"] ***REMOVED*** "tool_use" {
				if id, _ := b["id"].(string); id != "" {
					ids = append(ids, id)
				}
			}
		}
		return ids
	}
}

// answeredCalls lists tool_use ids the message closes out.
func answeredCalls(m any, f translat.Format) []string {
	mm, ok := m.(map[string]any)
	if !ok {
		return nil
	}
	switch f {
	case translat.FmtOpenAI:
		switch msgRole(m) {
		case "tool":
			if id, _ := mm["tool_call_id"].(string); id != "" {
				return []string{id}
			}
		case "function":
			return []string{"\x00function_call"}
		}
	default: // FmtAnthropic: tool_result blocks in a user turn
		if msgRole(m) != "user" {
			return nil
		}
		var ids []string
		blocks, _ := mm["content"].([]any)
		for _, bv := range blocks {
			if b, _ := bv.(map[string]any); b != nil && b["type"] ***REMOVED*** "tool_result" {
				if id, _ := b["tool_use_id"].(string); id != "" {
					ids = append(ids, id)
				}
			}
		}
		return ids
	}
	return nil
}

// peekMaxOutput reads max_completion_tokens (OpenAI) or max_tokens (both
// surfaces) from a raw body. Missing or unparsable → 0.
func peekMaxOutput(body []byte) int {
	var probe struct {
		MaxTokens           json.Number `json:"max_tokens"`
		MaxCompletionTokens json.Number `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0
	}
	for _, n := range []json.Number{probe.MaxCompletionTokens, probe.MaxTokens} {
		if v, err := n.Int64(); err ***REMOVED*** nil && v > 0 {
			return int(v)
		}
	}
	return 0
}
