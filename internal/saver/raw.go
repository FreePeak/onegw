package saver

import (
	"bytes"
	"encoding/json"

	"onegw/internal/translat"
)

// ApplyRaw compresses tool_result content surgically inside a raw
// same-format request body, preserving every other field (json.Number
// decoding keeps numeric fidelity). Returns the (possibly unmodified) body
// and estimated saved tokens (chars/4 delta).
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
	var saved int64
	switch format {
	case translat.FmtOpenAI:
		saved = s.applyOpenAI(obj)
	case translat.FmtAnthropic:
		saved = s.applyAnthropic(obj)
	case translat.FmtGemini:
		saved = s.applyGemini(obj)
	default:
		return raw, 0
	}
	if saved <= 0 {
		return raw, 0
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // do not inflate <,>,& to \u003c… across untouched strings
	if err := enc.Encode(root); err != nil {
		return raw, 0
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n")) // Encoder adds \n that Marshal does not
	wireSaved := (int64(len(raw)) - int64(len(out))) / 4
	if wireSaved <= 0 {
		return raw, 0 // net-negative re-encode: pass original through, report nothing
	}
	// True accounting: actual wire reduction, incl. re-encode effects.
	return out, wireSaved
}

func (s *Saver) applyOpenAI(root map[string]any) (saved int64) {
	msgs, _ := root["messages"].([]any)
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok || m["role"] != "tool" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			out := s.Compress(c)
			if out != c {
				saved += charsSaved(c, out)
				m["content"] = out
			}
		case []any:
			saved += s.compressTextParts(c)
		}
	}
	return saved
}

func (s *Saver) applyAnthropic(root map[string]any) (saved int64) {
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
				out := s.Compress(c)
				if out != c {
					saved += charsSaved(c, out)
					b["content"] = out
				}
			case []any:
				saved += s.compressTextParts(c)
			}
		}
	}
	return saved
}

func (s *Saver) applyGemini(root map[string]any) (saved int64) {
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
				out := s.Compress(r)
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
func (s *Saver) compressTextParts(arr []any) (saved int64) {
	for _, item := range arr {
		p, ok := item.(map[string]any)
		if !ok || p["type"] != "text" {
			continue
		}
		if t, ok := p["text"].(string); ok {
			out := s.Compress(t)
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
