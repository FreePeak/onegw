package server

// Per-model Responses routing on a chat-completions kind (SuperGrok follow-up
// to #12/#79). xAI serves some ids — grok-4.5 under an OAuth bearer — ONLY on
// its native /v1/responses endpoint, so a provider opts those ids off the chat
// wire with `responses_models`. Reference evidence: OmniRoute
// registry/xai/index.ts:31-37,66-69 (targetFormat tagging; upstream #10165
// documents the mirror mistake — a chat-shaped body reaching /v1/responses 422s
// "missing input"). The two tests pin both halves: the opted-in id changes
// wire, and every other id keeps today's behaviour byte-for-byte.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"onegw/internal/config"
)

func xaiGw(t *testing.T, up *httptest.Server, responsesModels []string) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "gw-key"}}
	cfg.Providers = []config.ProviderCfg{{
		Name: "xai", Kind: "openai", BaseURL: up.URL,
		Keys: []string{"xai-k1"}, Models: []string{"grok-4.5", "grok-4.6"},
		ResponsesModels: responsesModels,
	}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestXaiResponsesOnlyIdRoutesToResponsesWire(t *testing.T) {
	var path string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["messages"]; ok {
			t.Errorf("responses call carried chat messages: %v", req["messages"])
		}
		if _, ok := req["input"]; !ok {
			t.Errorf("responses call missing input, got %v", req)
		}
		// The Responses wire this kind speaks is stream-only (ForcedStream),
		// so the stub must answer in SSE: a buffered Responses JSON is not a
		// shape this path ever sees.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n")
	}))
	defer up.Close()

	s := xaiGw(t, up, []string{"grok-4.5*"})
	r := chatReq(t, "xai/grok-4.5")
	r.Header.Set("Authorization", "Bearer gw-key")
	w := do(t, s.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if path != "/v1/responses" {
		t.Fatalf("opted-in id hit %s, want /v1/responses", path)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Choices) ***REMOVED*** 0 ||
		out.Choices[0].Message.Content != "pong" {
		t.Fatalf("Responses reply not translated back into a chat completion: %s", w.Body.String())
	}
}

func TestXaiNonOptedIdStaysOnChatWire(t *testing.T) {
	var path string
	var hadMessages bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, hadMessages = req["messages"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"grok-4.6",` +
			`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	// One glob, one id: grok-4.6 must be untouched by the opt-in.
	s := xaiGw(t, up, []string{"grok-4.5*"})
	r := chatReq(t, "xai/grok-4.6")
	r.Header.Set("Authorization", "Bearer gw-key")
	w := do(t, s.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if path != "/v1/chat/completions" || !hadMessages {
		t.Fatalf("non-opted id went to %s (chat body: %v), want /v1/chat/completions with messages", path, hadMessages)
	}
}
