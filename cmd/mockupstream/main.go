// Command mockupstream is a tiny fake LLM provider used by onegw's smoke
// tests and load benchmarks. It serves:
//
//	POST /v1/chat/completions   — OpenAI shape, streaming + non-streaming
//	POST /v1/messages           — Anthropic shape, streaming + non-streaming
//
// The generated content size is controlled by the JSON body's "mock_tokens"
// field (default 200 tokens ≈ 800 chars).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9911", "listen address")
	flag.Parse()
	http.HandleFunc("/v1/chat/completions", handleOpenAI)
	http.HandleFunc("/v1/messages", handleAnthropic)
	fmt.Fprintln(os.Stderr, "mockupstream on", *listen)
	if err := http.ListenAndServe(*listen, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func tokenBlob(n int) string {
	// ~4 chars per token, varied to be incompressible-ish.
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(words[i%len(words)])
		b.WriteByte(' ')
	}
	return b.String()
}

func tokenCount(r *http.Request, body []byte) int {
	n := 200
	var probe struct {
		MockTokens int `json:"mock_tokens"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.MockTokens > 0 {
		n = probe.MockTokens
	}
	return n
}

func readBody(r *http.Request) []byte {
	// Probe the first 64 KiB for settings, then drain the rest so clients
	// with multi-MB bodies never block writing to us.
	buf := make([]byte, 1<<16)
	n, _ := io.ReadFull(r.Body, buf)
	if n < 0 {
		n = 0
	}
	go func(extra io.Reader) { _, _ = io.Copy(io.Discard, extra) }(r.Body)
	return buf[:n]
}

func handleOpenAI(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	tok := tokenCount(r, body)
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	if !req.Stream {
		resp := map[string]any{
			"id": "mock-1", "object": "chat.completion", "model": "mock",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": tokenBlob(tok)},
			}},
			"usage": map[string]any{"prompt_tokens": tok, "completion_tokens": tok, "total_tokens": tok * 2},
		}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	// Stream in chunks of 20 tokens.
	blob := tokenBlob(tok)
	w.Header().Set("Content-Type", "text/event-stream")
	for i := 0; i < len(blob); i += 80 {
		end := i + 80
		if end > len(blob) {
			end = len(blob)
		}
		chunk := map[string]any{
			"id": "mock-1", "object": "chat.completion.chunk", "model": "mock",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": blob[i:end]},
			}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	final := map[string]any{
		"id": "mock-1", "object": "chat.completion.chunk", "model": "mock",
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": tok, "completion_tokens": tok, "total_tokens": tok * 2},
	}
	b, _ := json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func handleAnthropic(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	tok := tokenCount(r, body)
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	if !req.Stream {
		resp := map[string]any{
			"id": "msg_mock", "type": "message", "role": "assistant", "model": "mock",
			"content":     []any{map[string]any{"type": "text", "text": tokenBlob(tok)}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": tok, "output_tokens": tok},
		}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": "mock", "content": []any{},
		"usage": map[string]any{"input_tokens": tok, "output_tokens": 1},
	}})
	emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	blob := tokenBlob(tok)
	for i := 0; i < len(blob); i += 80 {
		end := i + 80
		if end > len(blob) {
			end = len(blob)
		}
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": blob[i:end]}})
	}
	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	emit("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": tok}})
	emit("message_stop", map[string]any{"type": "message_stop"})
	_ = time.Now
}
