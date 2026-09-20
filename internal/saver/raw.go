package saver

import (
	"bytes"
	"encoding/json"

	"onegw/internal/translat"
)

// rawCtx carries per-conversation stickiness through one ApplyRaw pass.
type rawCtx struct {
	s    *Saver
	ce   *convEntry // nil when the conversation has never been sent canonical
	seen []uint64   // fingerprints of blocks compressed this pass
}

// block compresses one tool-result text. A block already compressed for
// this conversation keeps its compressed form even when this turn's
// relative savings dip below the floor — idempotence makes that safe and
// keeps the prefix stable (issue #35). Anything else goes through the
// plain savings-floored Compress.
func (rc *rawCtx) block(text string) string {
	out := rc.s.Compress(text)
	if out != text {
		rc.seen = append(rc.seen, headFP(text))
		return out
	}
	if rc.ce != nil && rc.ce.known(headFP(text)) {
		if un := rc.s.compressFiltered(text, rc.s.settings()); un != "" && len(un) < len(text) {
			rc.seen = append(rc.seen, headFP(text))
			return un
		}
	}
	return text
}

// ApplyRaw compresses tool_result content surgically inside a raw
// same-format request body, preserving every other field (json.Number
// decoding keeps numeric fidelity). Returns the (possibly unmodified) body
// and estimated saved tokens (chars/4 delta).
//
// The compress-or-raw decision is per-conversation sticky (issue #35):
// once a conversation has been sent in canonical (re-encoded) form it
// stays canonical even when a turn's aggregate savings fall to zero —
// reverting to the client's raw form would re-order every key and bust
// the whole upstream implicit-cache prefix. The never-grow guard keeps
// stability from costing bytes: a sticky re-encode that would make the
// body larger than the raw input is skipped.
func (s *Saver) ApplyRaw(format translat.Format, raw []byte) ([]byte, int64) {
	if !s.settings().Enabled {
		return raw, 0
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return raw, 0
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return raw, 0
	}
	key := conversationKey(format, obj)
	rc := &rawCtx{s: s, ce: s.convGet(key)}
	var saved int64
	switch format {
	case translat.FmtOpenAI:
		saved = s.applyOpenAI(obj, rc)
	case translat.FmtAnthropic:
		saved = s.applyAnthropic(obj, rc)
	case translat.FmtGemini:
		saved = s.applyGemini(obj, rc)
	default:
		return raw, 0
	}
	if saved <= 0 && rc.ce == nil {
		return raw, 0
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // do not inflate <,>,& to \u003c… across untouched strings
	if err := enc.Encode(root); err != nil {
		return raw, 0
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n")) // Encoder adds \n that Marshal does not
	if saved > 0 {
		wireSaved := (int64(len(raw)) - int64(len(out))) / 4
		if wireSaved <= 0 {
			return raw, 0 // net-negative re-encode: pass original through, report nothing
		}
		s.convRecord(key, rc.seen)
		return out, wireSaved
	}
	// Sticky turn with no block changes: the re-encode only restores the
	// canonical byte form the upstream prefix was built on. Never grow.
	if len(out) > len(raw) {
		return raw, 0
	}
	return out, (int64(len(raw)) - int64(len(out))) / 4
}

func (s *Saver) applyOpenAI(root map[string]any, rc *rawCtx) (saved int64) {
	msgs, _ := root["messages"].([]any)
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok || m["role"] != "tool" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			out := rc.block(c)
			if out != c {
				saved += charsSaved(c, out)
				m["content"] = out
			}
		case []any:
			saved += s.compressTextParts(c, rc)
		}
	}
	return saved
}

func (s *Saver) applyAnthropic(root map[string]any, rc *rawCtx) (saved int64) {
	msgs, _ := root["messages"].([]any)
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, bv := range blocks {
			b, ok := bv.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				continue
			}
			switch c := b["content"].(type) {
			case string:
				out := rc.block(c)
				if out != c {
					saved += charsSaved(c, out)
					b["content"] = out
				}
			case []any:
				saved += s.compressTextParts(c, rc)
			}
		}
	}
	return saved
}

func (s *Saver) applyGemini(root map[string]any, rc *rawCtx) (saved int64) {
	contents, _ := root["contents"].([]any)
	for _, cv := range contents {
		c, ok := cv.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := c["parts"].([]any)
		for _, pv := range parts {
			p, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			fr, ok := p["functionResponse"].(map[string]any)
			if !ok {
				continue
			}
			resp, ok := fr["response"].(map[string]any)
			if !ok {
				continue
			}
			if r, ok := resp["result"].(string); ok {
				out := rc.block(r)
				if out != r {
					saved += charsSaved(r, out)
					resp["result"] = out
				}
			}
		}
	}
	return saved
}

// compressTextParts compresses {type:"text",text:...} entries in a block
// array (OpenAI tool content arrays, Anthropic tool_result block arrays).
func (s *Saver) compressTextParts(arr []any, rc *rawCtx) (saved int64) {
	for _, item := range arr {
		p, ok := item.(map[string]any)
		if !ok || p["type"] != "text" {
			continue
		}
		if t, ok := p["text"].(string); ok {
			out := rc.block(t)
			if out != t {
				saved += charsSaved(t, out)
				p["text"] = out
			}
		}
	}
	return saved
}

func charsSaved(before, after string) int64 {
	d := int64(len(before)-len(after)) / 4
	if d < 0 {
		return 0
	}
	return d
}
