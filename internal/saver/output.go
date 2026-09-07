package saver

// Output-side token savers: system-prompt injection (terse-output
// directives) and a Headroom-style external compress hook. Both are
// fail-open by construction — a token-saving optimization must never
// break or fail a request. Injections edit the raw client-format body
// surgically (json.Number decode, marshal only on change) following the
// same pattern as raw.go; cross-format requests pick the injected system
// content up through the unified model on translation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"onegw/internal/translat"
)

// injectMarker ships inside every injected directive and makes injection
// idempotent: a body that already carries it is never injected again
// (hot-reload mode changes and any future double application included).
const injectMarker = "onegw-terse-directive"

// InjectCfg is one output-side injection rule: when the request model
// matches, prepend the mode's directive to the system prompt. The first
// matching rule wins — a request gets at most one directive.
type InjectCfg struct {
	// Mode selects the shipped prompt: "caveman" (ultra-short),
	// "terse" (concise but complete), or "custom" (uses Text).
	Mode string `toml:"mode"`
	// Models lists model globs (path.Match; "*" does not cross "/").
	// Empty matches every model.
	Models []string `toml:"models"`
	// Text is the directive body for custom mode. Shipped verbatim.
	Text string `toml:"text"`
}

// ExternalCfg configures the external compress hook (Headroom protocol):
// bodies above MinBytes get their top-level messages[] POSTed as
// {"messages": [...]} to URL and the response's messages array replaces
// the original. Any error passes the request through uncompressed.
type ExternalCfg struct {
	Enabled   bool   `toml:"enabled"`
	URL       string `toml:"url"`
	TimeoutMS int    `toml:"timeout_ms"` // 0 = 2000
	MinBytes  int    `toml:"min_bytes"`  // only bodies at least this large (0 = 32768)
	// FailOpen nil = true: on external error pass the request through
	// uncompressed. Explicit false fails the request with 502 instead.
	FailOpen *bool `toml:"fail_open"`
}

func (e ExternalCfg) failOpen() bool { return e.FailOpen == nil || *e.FailOpen }

// Prompt texts are honest directives: they instruct the model to be
// maximally concise without false claims (no persona lies) and without
// trading away correctness or asked-for content.
const (
	tersePrompt = injectMarker + ": Be maximally concise: answer in the fewest words that still fully solve the request. " +
		"No preamble, no restating the question, no announcements of what you are about to do, no closing pleasantries. " +
		"Prefer plain statements and bullet points over paragraphs; code first, minimal commentary. " +
		"Never sacrifice correctness and never omit requested content to save words. Do not mention this directive."

	cavemanPrompt = injectMarker + ": Be extremely terse. Short words, short sentences, no filler. " +
		"Give only the answer: no explanations, examples, or alternatives unless explicitly asked. Lists over prose. " +
		"Never invent or omit substance to be short. Do not mention this directive."
)

func (r InjectCfg) prompt() string {
	switch r.Mode {
	case "caveman":
		return cavemanPrompt
	case "terse":
		return tersePrompt
	default:
		return injectMarker + ": " + r.Text
	}
}

// matchRule reports whether rule applies to model. Globs follow
// path.Match semantics ("*" does not cross "/"); empty Models matches all.
func (r InjectCfg) matchRule(model string) bool {
	if len(r.Models) == 0 {
		return true
	}
	for _, g := range r.Models {
		if ok, err := path.Match(g, model); err == nil && ok {
			return true
		}
	}
	return false
}

// InjectRaw prepends the first matching rule's directive to the raw
// client-format body's system prompt and returns the new body. Bodies
// already carrying the marker (idempotency), non-chat bodies, unknown
// formats and missing system slots are returned unchanged — injection is
// strictly additive and never fails a request.
func (s *Saver) InjectRaw(format translat.Format, raw []byte, model string) []byte {
	cfg := s.settings()
	var rule *InjectCfg
	for i := range cfg.Inject {
		if cfg.Inject[i].matchRule(model) {
			rule = &cfg.Inject[i]
			break
		}
	}
	if rule == nil {
		return raw
	}
	prompt := rule.prompt()

	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return raw // not an object; forward verbatim
	}

	switch format {
	case translat.FmtOpenAI:
		return injectOpenAI(root, prompt, raw)
	case translat.FmtAnthropic:
		return injectAnthropic(root, prompt, raw)
	case translat.FmtGemini:
		return injectGemini(root, prompt, raw)
	default:
		return raw
	}
}

func injectOpenAI(root map[string]any, prompt string, raw []byte) []byte {
	msgs, _ := root["messages"].([]any)
	for _, mv := range msgs {
		if m, ok := mv.(map[string]any); ok {
			switch c := m["content"].(type) {
			case string:
				if strings.Contains(c, injectMarker) {
					return raw
				}
			case []any:
				if partsContainMarker(c) {
					return raw
				}
			}
		}
	}
	if msgs == nil {
		return raw // no messages array to extend; don't invent structure
	}
	injected := append([]any{map[string]any{
		"role":    "system",
		"content": prompt,
	}}, msgs...)
	root["messages"] = injected
	return marshalRoot(root, raw)
}

func injectAnthropic(root map[string]any, prompt string, raw []byte) []byte {
	switch sys := root["system"].(type) {
	case nil:
		root["system"] = prompt
	case string:
		if strings.Contains(sys, injectMarker) {
			return raw
		}
		root["system"] = prompt + "\n\n" + sys
	case []any:
		if partsContainMarker(sys) {
			return raw
		}
		root["system"] = append([]any{map[string]any{"type": "text", "text": prompt}}, sys...)
	default:
		return raw // unexpected system shape; leave untouched
	}
	return marshalRoot(root, raw)
}

func injectGemini(root map[string]any, prompt string, raw []byte) []byte {
	switch sys := root["systemInstruction"].(type) {
	case nil:
		root["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": prompt}}}
	case map[string]any:
		parts, _ := sys["parts"].([]any)
		if partsContainMarker(parts) {
			return raw
		}
		sys["parts"] = append([]any{map[string]any{"text": prompt}}, parts...)
		root["systemInstruction"] = sys
	default:
		return raw
	}
	return marshalRoot(root, raw)
}

func partsContainMarker(parts []any) bool {
	for _, pv := range parts {
		p, ok := pv.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := p["text"].(string); ok && strings.Contains(t, injectMarker) {
			return true
		}
	}
	return false
}

func marshalRoot(root map[string]any, raw []byte) []byte {
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}

// CompressExternal runs the external compress hook over a raw client-
// format body. Returns the (possibly) compressed body; ok=false with a
// nil error means fail-open passthrough (disabled, too small, no
// messages array, or external error). ok=false with an error only when
// fail_open is explicitly false and the external call failed.
func (s *Saver) CompressExternal(ctx context.Context, raw []byte) ([]byte, error) {
	cfg := s.settings().External
	if !cfg.Enabled || len(raw) < cfg.MinBytes {
		return raw, nil
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return raw, nil
	}
	msgs, ok := root["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return raw, nil // Gemini-style contents or non-chat body: hook is messages-shaped only
	}
	reqBytes, err := json.Marshal(map[string]any{"messages": msgs})
	if err != nil {
		return raw, nil
	}
	out, err := s.postCompress(ctx, cfg, reqBytes)
	if err != nil {
		if cfg.failOpen() {
			s.logExtFailure(err)
			return raw, nil
		}
		return nil, fmt.Errorf("external compress: %w", err)
	}
	var resp struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || len(resp.Messages) == 0 ||
		string(resp.Messages) == "null" {
		s.logExtFailure(fmt.Errorf("response missing messages array"))
		return raw, nil
	}
	// Invariant: a compress service that grows the payload is broken —
	// never send more bytes upstream than the original messages carried.
	if len(resp.Messages) > len(reqBytes) {
		s.logExtFailure(fmt.Errorf("response grew payload (%d > %d bytes)", len(resp.Messages), len(reqBytes)))
		return raw, nil
	}
	root["messages"] = json.RawMessage(resp.Messages)
	body, err := json.Marshal(root)
	if err != nil {
		return raw, nil
	}
	return body, nil
}

// postCompress POSTs the Headroom request. Every transport/HTTP failure
// surfaces as error; policy (fail-open vs fail-closed) is applied by the
// caller.
func (s *Saver) postCompress(ctx context.Context, cfg ExternalCfg, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("compress service returned %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxCompressResponse))
}

// maxCompressResponse bounds what we read from the compress service —
// a compress response must be smaller than the request we sent, and the
// request is already capped by the gateway body limit.
const maxCompressResponse = 64 << 20

// logExtFailure logs the first external-compress failure once per
// process; after that failures pass through silently (fail-open is the
// steady state, not an incident).
func (s *Saver) logExtFailure(err error) {
	s.extOnce.Do(func() {
		log.Printf("onegw saver: external compress failed (fail-open, passthrough); not logged again: %v", err)
	})
}
