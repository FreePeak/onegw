package server

import (
	"bytes"
	"encoding/json"

	"onegw/internal/provider"
	"onegw/internal/translat"
)

// anchorCacheProfile applies the provider's configured cache_profile to a
// fully prepared upstream body (issue #34). It must run LAST in the
// request pipeline — after ApplyRaw / rewriteModel / adaptThinkingBody /
// normalizeRoles / translation — because client cache_control markers
// point at pre-normalization offsets, and a stale anchor costs a full
// prefix rewrite (mirrors 9router's anchorClaudeCache, which also runs
// after every normalizer).
//
// Profiles:
//   - "" / "none" (default): body returned byte-identical. GLM, DeepSeek
//     and b-ai ignore cache fields entirely (live-probed), so no bytes
//     are ever invented for them.
//   - "claude-anchor": Anthropic-format upstream bodies. Every client
//     cache_control marker is stripped, then {"type":"ephemeral"} is
//     re-anchored at canonical positions: the last system block, the
//     last cache-eligible tool (defer_loading tools are skipped —
//     Anthropic rejects that combination), and the last assistant turn's
//     last non-thinking block (the final message on turn one). Three
//     anchors max, inside Anthropic's 4-breakpoint ceiling.
//   - "dashscope-marker": OpenAI-format upstream bodies. Client
//     cache_control markers are preserved verbatim (Qwen accepts 4
//     markers, 20-block lookback); only when the body carries more than
//     4 are the oldest stripped until 4 remain.
//   - "sticky-key": OpenAI-format upstream bodies. The session identity
//     is injected as top-level prompt_cache_key for implicit
//     sticky-routing upstreams (xai, OpenRouter, Kimi). An empty
//     sessionKey skips injection.
//
// Unknown profiles return the body unchanged (config Validate rejects
// them at load); bodies that fail to parse are forwarded verbatim.
// json.Number decoding keeps numeric fidelity across the rewrite.
func anchorCacheProfile(body []byte, model string, def *provider.Def, upstream translat.Format, sessionKey string) []byte {
	if def == nil {
		return body
	}
	switch def.CacheProfile {
	case "", "none":
		return body
	case "claude-anchor":
		if upstream != translat.FmtAnthropic {
			return body
		}
		return anchorClaudeCache(body)
	case "dashscope-marker":
		if upstream != translat.FmtOpenAI {
			return body
		}
		return capDashScopeMarkers(body)
	case "sticky-key":
		if upstream != translat.FmtOpenAI || sessionKey == "" {
			return body
		}
		return injectPromptCacheKey(body, sessionKey)
	default:
		return body
	}
}

// anchorClaudeCache normalizes Anthropic-style cache_control breakpoints
// onto canonical positions of an Anthropic-format body (see
// anchorCacheProfile). Returns body unchanged when it neither strips nor
// anchors anything.
func anchorClaudeCache(body []byte) []byte {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body // not an object; forward verbatim
	}
	changed := false
	// Client markers point at pre-normalization offsets: strip every one,
	// then re-anchor at canonical positions.
	stripCacheControl(root)
	// 1. Last system block. A bare string system is wrapped into the
	//    block form so it can carry the marker (Anthropic accepts both).
	switch sys := root["system"].(type) {
	case []any:
		if n := len(sys); n > 0 {
			if b, ok := sys[n-1].(map[string]any); ok {
				b["cache_control"] = map[string]any{"type": "ephemeral"}
				changed = true
			}
		}
	case string:
		if sys != "" {
			root["system"] = []any{map[string]any{
				"type": "text", "text": sys,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
			changed = true
		}
	}
	// 2. Last cache-eligible tool; defer_loading tools are skipped
	//    (Anthropic rejects markers on them — 9router issue 3567).
	if tools, ok := root["tools"].([]any); ok {
		for i := len(tools) - 1; i >= 0; i-- {
			t, ok := tools[i].(map[string]any)
			if !ok {
				continue
			}
			if dl, _ := t["defer_loading"].(bool); dl {
				continue
			}
			t["cache_control"] = map[string]any{"type": "ephemeral"}
			changed = true
			break
		}
	}
	// 3. Last assistant turn's last non-thinking block; on turn one (no
	//    assistant message yet) the final message takes the anchor so the
	//    completed exchange keeps a byte-stable prefix.
	msgs, _ := root["messages"].([]any)
	target := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if m, ok := msgs[i].(map[string]any); ok && m["role"] == "assistant" {
			target = i
			break
		}
	}
	if target == -1 && len(msgs) > 0 {
		target = len(msgs) - 1 // turn one: anchor the final message
	}
	if target >= 0 {
		if m, ok := msgs[target].(map[string]any); ok {
			changed = anchorLastCacheableBlock(m) || changed
		}
	}
	if !changed {
		return body
	}
	return reencodeRoot(root, body)
}

// anchorLastCacheableBlock places an ephemeral marker on the last block
// of the message that is not a thinking block (thinking and
// redacted_thinking are never anchored). String content is wrapped into
// a text block to carry the marker. Reports whether anything changed.
func anchorLastCacheableBlock(m map[string]any) bool {
	switch c := m["content"].(type) {
	case string:
		if c == "" {
			return false
		}
		m["content"] = []any{map[string]any{
			"type": "text", "text": c,
			"cache_control": map[string]any{"type": "ephemeral"},
		}}
		return true
	case []any:
		for i := len(c) - 1; i >= 0; i-- {
			b, ok := c[i].(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := b["type"].(string); t {
			case "thinking", "redacted_thinking":
				continue
			default:
				b["cache_control"] = map[string]any{"type": "ephemeral"}
				return true
			}
		}
	}
	return false
}

// stripCacheControl removes every cache_control field from the decoded
// tree: client markers point at pre-normalization offsets and are never
// trusted.
func stripCacheControl(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for _, child := range t {
			stripCacheControl(child)
		}
	case []any:
		for _, child := range t {
			stripCacheControl(child)
		}
	}
}

// capDashScopeMarkers keeps client cache_control markers on an
// OpenAI-format body intact unless the body exceeds Qwen/DashScope's
// documented 4-marker ceiling, in which case the oldest markers are
// stripped until 4 remain. Walk order is message content parts first,
// then message objects, then tool definitions, so the stable tool
// anchors survive the cap and only stale conversation markers are
// dropped.
func capDashScopeMarkers(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"cache_control"`)) {
		return body
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body
	}
	var marks []map[string]any
	if msgs, ok := root["messages"].([]any); ok {
		for _, mv := range msgs {
			m, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			if parts, ok := m["content"].([]any); ok {
				for _, pv := range parts {
					if p, ok := pv.(map[string]any); ok {
						if _, has := p["cache_control"]; has {
							marks = append(marks, p)
						}
					}
				}
			}
			if _, has := m["cache_control"]; has {
				marks = append(marks, m)
			}
		}
	}
	if tools, ok := root["tools"].([]any); ok {
		for _, tv := range tools {
			if t, ok := tv.(map[string]any); ok {
				if _, has := t["cache_control"]; has {
					marks = append(marks, t)
				}
			}
		}
	}
	if len(marks) <= 4 {
		return body // within the ceiling: preserve the client's markers as-is
	}
	for _, p := range marks[:len(marks)-4] {
		delete(p, "cache_control")
	}
	return reencodeRoot(root, body)
}

// injectPromptCacheKey sets (or overwrites) the top-level prompt_cache_key
// so implicit sticky-routing upstreams (xai, OpenRouter, Kimi) pin the
// request to the gateway session identity. An already-equal value is
// left alone so the body is forwarded byte-identically.
func injectPromptCacheKey(body []byte, sessionKey string) []byte {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body
	}
	if cur, ok := root["prompt_cache_key"].(string); ok && cur == sessionKey {
		return body
	}
	root["prompt_cache_key"] = sessionKey
	return reencodeRoot(root, body)
}

// reencodeRoot re-serializes a decoded body: map encoding sorts keys,
// json.Number preserves numeric literals, and EscapeHTML stays off so
// untouched strings never inflate. On failure the original body wins.
func reencodeRoot(root map[string]any, body []byte) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
