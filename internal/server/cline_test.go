package server

// Cline (api.cline.bot) is an OpenAI-wire upstream that only answers correctly
// over SSE: ask it for a non-streaming completion and it replies with its
// account-API envelope, {"success":true,"data":{...completion...}} (live-verified
// 2026-09-15). Kind.ForcedStream makes the gateway stream upstream anyway and
// aggregate, but a same-format upstream receives the client body nearly
// verbatim — so the absent/false "stream" has to be rewritten, or the
// aggregator reads a JSON body as SSE, sees no events, and serves an empty
// completion. That silent-empty reply is what this test catches without
// forceStreamFlag.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"onegw/internal/config"
	"onegw/internal/provider"
)

func TestClineNonStreamClientIsAskedToStreamUpstream(t *testing.T) {
	var gotStream any
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		gotStream = req["stream"]
		if v, ok := gotStream.(bool); !ok || !v {
			// The vendor's real answer to stream=false: the wrapped form.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true,"data":{"id":"c","object":"chat.completion",`+
				`"model":"anthropic/claude-sonnet-4-6","choices":[{"index":0,"finish_reason":"stop",`+
				`"message":{"role":"assistant","content":"should-never-reach-a-client"}}]}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"anthropic/claude-sonnet-4-6","choices":[{"index":0,"delta":{"role":"assistant","content":"po"}}]}`+"\n\n"+
				`data: {"id":"c1","object":"chat.completion.chunk","model":"anthropic/claude-sonnet-4-6","choices":[{"index":0,"delta":{"content":"ng"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	defer up.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "gw-key"}}
	cfg.Providers = []config.ProviderCfg{{
		Name: "cline", Kind: string(provider.KindCline), BaseURL: up.URL,
		Keys: []string{"cline-k1"}, Models: []string{"anthropic/claude-sonnet-4-6"},
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

	r := chatReq(t, "cline/anthropic/claude-sonnet-4-6")
	r.Header.Set("Authorization", "Bearer gw-key")
	w := do(t, s.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path %s, want /v1/chat/completions", gotPath)
	}
	if v, ok := gotStream.(bool); !ok || !v {
		t.Fatalf("upstream body carried stream=%#v; a cline call must always stream", gotStream)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("reply is not a chat completion: %s", w.Body.String())
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "pong" {
		t.Fatalf("aggregated content wrong (envelope leaked or stream lost): %s", w.Body.String())
	}
	if out.Usage.PromptTokens != 9 || out.Usage.CompletionTokens != 2 {
		t.Errorf("usage not carried out of the stream: %+v", out.Usage)
	}
}

// forceStreamFlag round-trips the body through map[string]any, which is only
// safe if numbers keep their literal text (json.Number) and every structural
// detail an agent body survives: nested content parts, tool schemas, nulls, and
// the big integer seeds some clients send. A silent 2e+05 or a mangled seed
// would corrupt a paid request, so pin it.
func TestForceStreamFlagPreservesBodySemantics(t *testing.T) {
	in := `{"model":"cline/anthropic/claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"pa"}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"n":{"type":"integer","default":9007199254740993}}}}}],"temperature":0.7,"max_tokens":200000,"presence_penalty":-1,"stop":null,"stream":false,"n":1,"seed":9007199254740993,"stream_options":{"include_usage":true},"logprobs":false,"metadata":{"k":""}}`
	out := string(forceStreamFlag([]byte(in)))

	var want, got map[string]any
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("rewrite produced invalid JSON: %v\n%s", err, out)
	}
	if got["stream"] != true {
		t.Fatalf("stream not forced true: %s", out)
	}
	for _, lit := range []string{`"temperature":0.7`, `"seed":9007199254740993`, `"max_tokens":200000`, `"presence_penalty":-1`} {
		if !strings.Contains(out, lit) {
			t.Errorf("literal %s lost from the body:\n%s", lit, out)
		}
	}
	if !strings.Contains(out, `"stop":null`) || !strings.Contains(out, `"content":null`) {
		t.Errorf("nulls dropped:\n%s", out)
	}
	if strings.Contains(out, "e+0") || strings.Contains(out, "E+0") {
		t.Errorf("numbers reflowed into scientific notation:\n%s", out)
	}
	// Structural equality on everything except stream (key order is free).
	delete(want, "stream")
	delete(got, "stream")
	if !reflect.DeepEqual(want, got) {
		t.Errorf("body semantics changed:\nwant %v\ngot  %v", want, got)
	}

	// Already-streaming and unparseable bodies must come back untouched.
	if s := forceStreamFlag([]byte(`{"stream":true,"x":1}`)); string(s) != `{"stream":true,"x":1}` {
		t.Errorf("stream:true body rewritten: %s", s)
	}
	if s := forceStreamFlag([]byte(`not json`)); string(s) != `not json` {
		t.Errorf("invalid body rewritten: %s", s)
	}
}
