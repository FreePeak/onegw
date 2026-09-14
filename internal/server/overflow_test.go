package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"onegw/internal/config"
	"onegw/internal/translat"
)

// bigOpenAIBody builds an oversized OpenAI-format chat body: a pinned system
// turn plus nToolTurns user→assistant(tool_calls)→tool(result) units (~4KB
// each), ending with the live user turn. max_tokens is set so the prune
// budget subtracts a known output reserve.
func bigOpenAIBody(t *testing.T, model string, nToolTurns int) []byte {
	t.Helper()
	msgs := []any{map[string]any{"role": "system", "content": "you are terse"}}
	for i := 0; i < nToolTurns; i++ {
		pad := strings.Repeat("x", 2000)
		msgs = append(msgs,
			map[string]any{"role": "user", "content": fmt.Sprintf("q%d %s", i, pad)},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": fmt.Sprintf("call_%d", i), "type": "function",
					"function": map[string]any{"name": "f", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "content": "r " + pad},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "what changed overall?"})
	body, err := json.Marshal(map[string]any{
		"model": model, "max_tokens": 1024, "messages": msgs,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "f", "parameters": map[string]any{"type": "object"}}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// assertOpenAIPairsHold checks the wire invariant every tool-calling upstream
// enforces: each assistant tool_calls turn is followed (before the next
// assistant turn) by exactly its results, and no tool message heads the
// conversation.
func assertOpenAIPairsHold(t *testing.T, body []byte) {
	t.Helper()
	var root struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("pruned body unparsable: %v", err)
	}
	msgs := root.Messages
	// Leading system turns are pinned, then the conversation must start at
	// a user turn.
	i := 0
	for i < len(msgs) && (msgs[i].Role ***REMOVED*** "system" || msgs[i].Role ***REMOVED*** "developer") {
		i++
	}
	if i ***REMOVED*** 0 || i >= len(msgs) {
		t.Fatalf("no system lead or empty conversation: %+v", msgs)
	}
	if msgs[i].Role != "user" {
		t.Fatalf("kept conversation heads with role %q, want user", msgs[i].Role)
	}
	for j, m := range msgs {
		if m.Role ***REMOVED*** "tool" {
			found := false
			for k := j - 1; k >= 0; k-- {
				if msgs[k].Role != "tool" {
					if msgs[k].Role ***REMOVED*** "assistant" {
						for _, c := range msgs[k].ToolCalls {
							if c.ID ***REMOVED*** m.ToolCallID {
								found = true
							}
						}
					}
					break
				}
			}
			if !found {
				t.Fatalf("tool message %d (%s) has no preceding assistant call", j, m.ToolCallID)
			}
		}
	}
}

func TestPruneToFitOpenAIKeepsToolPairs(t *testing.T) {
	body := bigOpenAIBody(t, "m", 40)
	if len(body) < 100_000 {
		t.Fatalf("test body too small: %d bytes", len(body))
	}
	// A 20k window with 1024 max_tokens and the refusal's own count gives a
	// constant ~67.7KB byte budget (16928/50000 × 4): forces a mid-history
	// cut.
	pruned := pruneToFit(body, translat.FmtOpenAI, 20_000, len(body)/4)
	if pruned ***REMOVED*** nil {
		t.Fatal("pruneToFit returned nil, want a fitted body")
	}
	if len(pruned) >= len(body) {
		t.Fatalf("pruned body not smaller: %d → %d", len(body), len(pruned))
	}
	if int64(len(pruned)) > 4*16_928 {
		t.Fatalf("pruned body over budget: %d bytes", len(pruned))
	}
	assertOpenAIPairsHold(t, pruned)

	// Everything outside messages survives the rewrite: knobs and tools.
	var root map[string]any
	numDec := json.NewDecoder(bytes.NewReader(pruned))
	numDec.UseNumber()
	if err := numDec.Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["model"] != "m" {
		t.Fatalf("model lost: %v", root["model"])
	}
	if mt, _ := root["max_tokens"].(json.Number); mt != "1024" {
		t.Fatalf("max_tokens mangled: %v (%T)", root["max_tokens"], root["max_tokens"])
	}
	if tools, _ := root["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools lost: %v", root["tools"])
	}
	if msgs := root["messages"].([]any); msgRole(msgs[0]) != "system" {
		t.Fatalf("system lead lost: %v", msgs[0])
	}
}

func TestPruneToFitAnthropicKeepsResultPairing(t *testing.T) {
	// user → assistant(text + tool_use) → user(tool_result + text) × n,
	// mirroring Claude Code's interleaved histories.
	var msgs []any
	for i := 0; i < 30; i++ {
		pad := strings.Repeat("y", 2000)
		msgs = append(msgs,
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("q%d %s", i, pad)}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "working"},
				map[string]any{"type": "tool_use", "id": fmt.Sprintf("tu_%d", i), "name": "f", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("tu_%d", i), "content": "done " + pad},
			}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "summarize"}}})
	body, err := json.Marshal(map[string]any{
		"model": "claude-x", "max_tokens": 1024, "system": "be nice", "messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	pruned := pruneToFit(body, translat.FmtAnthropic, 20_000, len(body)/4)
	if pruned ***REMOVED*** nil {
		t.Fatal("pruneToFit returned nil")
	}
	var root struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(pruned, &root); err != nil {
		t.Fatal(err)
	}
	if root.System != "be nice" {
		t.Fatalf("top-level system lost: %q", root.System)
	}
	if len(root.Messages) ***REMOVED*** 0 || root.Messages[0].Role != "user" {
		t.Fatalf("kept history must head with user: %+v", root.Messages[:min(2, len(root.Messages))])
	}
	open := map[string]bool{}
	for _, m := range root.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				if m.Role != "assistant" {
					t.Fatalf("tool_use in %s turn", m.Role)
				}
				open[b.ID] = true
			case "tool_result":
				if !open[b.ToolUseID] {
					t.Fatalf("orphaned tool_result for %s (kept history lost its call)", b.ToolUseID)
				}
				delete(open, b.ToolUseID)
			}
		}
	}
	if len(open) > 0 {
		t.Fatalf("cut left %d unanswered tool_use turns", len(open))
	}
}

func TestPruneToFitUnknownDensityHalves(t *testing.T) {
	// Digit-dense adversarial text: the upstream refused it under a window
	// the bytes/4 estimator believed it fit (live 2026-09-14, "Range of
	// input length should be [1, 983616]" against a 2.8MB body). With no
	// measured count, recovery must trust the refusal over the estimate and
	// at least halve the body.
	body := bigOpenAIBody(t, "m", 40)
	pruned := pruneToFit(body, translat.FmtOpenAI, 5_000_000, 0)
	if pruned ***REMOVED*** nil {
		t.Fatal("unknown-density overflow must still be pruned")
	}
	if int64(len(pruned)) > int64(len(body))/2 {
		t.Fatalf("pruned body %d bytes, want at most half of %d", len(pruned), len(body))
	}
	assertOpenAIPairsHold(t, pruned)
}

func TestPruneToFitDeclines(t *testing.T) {
	body := bigOpenAIBody(t, "m", 6)
	if pruneToFit(body, translat.FmtGemini, 20_000, 999_999) != nil {
		t.Fatal("gemini surface must not be pruned")
	}
	if pruneToFit([]byte("not json"), translat.FmtOpenAI, 20_000, 999_999) != nil {
		t.Fatal("unparseable body must not be pruned")
	}
	// Measured path: upstream's own count says the body already fits the
	// window — recovery cannot help, the 400 stays honest.
	if pruneToFit(body, translat.FmtOpenAI, 40_000, len(body)/4) != nil {
		t.Fatal("body already under the measured budget must keep its honest 400")
	}
	// Budget smaller than the pinned lead + the final user turn: nothing
	// removable reaches it.
	if pruneToFit(body, translat.FmtOpenAI, 2_000, 999_999) != nil {
		t.Fatal("tiny window must decline, not mangle the conversation")
	}
}

// sizeGateUpstream answers the verbatim z.ai overflow refusal while the
// request is larger than maxBytes (reporting bytes/4 as the measured input),
// and a normal completion otherwise. It records every body it saw.
type sizeGateUpstream struct {
	mu       sync.Mutex
	maxBytes int
	window   int
	seen     [][]byte
	server   *httptest.Server
}

func newSizeGateUpstream(maxBytes, window int) *sizeGateUpstream {
	g := &sizeGateUpstream{maxBytes: maxBytes, window: window}
	mux := http.NewServeMux()
	g.server = httptest.NewServer(mux)
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		body := buf.Bytes()
		g.mu.Lock()
		g.seen = append(g.seen, body)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if len(body) > g.maxBytes {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":{"message":"The input (%d tokens) is longer than the model's context length (%d tokens).","type":"BadRequestError","code":400}}`, len(body)/4, g.window)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-test", "object": "chat.completion", "model": "gated",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "recovered"}}},
		})
	})
	return g
}

func (g *sizeGateUpstream) bodies() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]byte(nil), g.seen...)
}

// The live failure mode end to end: a `free`-style combo whose every leg
// holds a 20k-token window receives a ~180KB body. Both legs refuse it (the
// pre-existing fall-through), the gateway prunes to fit, and the retry serves
// — the client gets a completion, never the 400.
func TestOverflowPruneRetryEndToEnd(t *testing.T) {
	g1 := newSizeGateUpstream(80_000, 20_000)
	defer g1.server.Close()
	g2 := newSizeGateUpstream(80_000, 20_000)
	defer g2.server.Close()

	cfg := gatedCfg(t,
		config.ProviderCfg{Name: "p1", Kind: "openai", BaseURL: g1.server.URL, Models: []string{"gated"},
			Accounts: []config.Acct{{Name: "a", APIKey: "k"}}},
		config.ProviderCfg{Name: "p2", Kind: "openai", BaseURL: g2.server.URL, Models: []string{"gated"},
			Accounts: []config.Acct{{Name: "a", APIKey: "k"}}},
	)
	_, h := newTestServer2(t, cfg)

	body := bigOpenAIBody(t, "pair", 40)
	if len(body) <= 80_000 {
		t.Fatalf("body must start over the gate: %d", len(body))
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := do(t, h, authSk(r))

	if w.Code != 200 || !strings.Contains(w.Body.String(), "recovered") {
		t.Fatalf("client got %d %s, want the pruned retry to serve", w.Code, w.Body.String())
	}
	// Both legs refused the oversized first pass (fall-through still works),
	// then leg one served the pruned replay.
	if b := g1.bodies(); len(b) != 2 || len(b[0]) <= 80_000 || len(b[1]) > 80_000 {
		t.Fatalf("p1 saw %d bodies with sizes %v, want oversized then fitted", len(b), sizes(b))
	}
	if b := g2.bodies(); len(b) != 1 || len(b[0]) <= 80_000 {
		t.Fatalf("p2 saw %d bodies with sizes %v, want only the oversized refusal", len(b), sizes(b))
	}
	assertOpenAIPairsHold(t, g1.bodies()[1])
}

func sizes(bs [][]byte) []int {
	out := make([]int, len(bs))
	for i, b := range bs {
		out[i] = len(b)
	}
	return out
}
