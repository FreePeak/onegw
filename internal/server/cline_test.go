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

// TestClineCreditWallBenchesModelNotAccount is the #80 exception: a Cline
// account with a spent balance still serves its `:free` lane (measured
// 2026-09-16 — the paid id 402s with insufficient_credits / current_balance
// -0.006273 while inclusionai/ling-3.0-flash-fin:free answers 200 on the same
// bearer), so a terminal invalidation would bench working traffic until a human
// re-enables it. The paid model gets benched instead, the account keeps its
// place in the pool, and the sibling lane must still serve.
func TestClineCreditWallBenchesModelNotAccount(t *testing.T) {
	var hits []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		hits = append(hits, req.Model)
		if req.Model ***REMOVED*** "deepseek/deepseek-v4.1-flash" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"error":{"code":"insufficient_credits",`+
				`"message":"Insufficient balance. Your Cline Credits balance is $-0.01",`+
				`"current_balance":-0.006273}}`)
			return
		}
		if !req.Stream {
			t.Errorf("cline call reached upstream with stream=false: %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	defer up.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "gw-key"}}
	cfg.Providers = []config.ProviderCfg{{
		Name: "cline", Kind: string(provider.KindCline), BaseURL: up.URL, APIKey: "workos:session",
		Models: []string{"deepseek/deepseek-v4.1-flash", "inclusionai/ling-3.0-flash-fin:free"},
	}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer s.Close()

	r := chatReq(t, "cline/deepseek/deepseek-v4.1-flash")
	r.Header.Set("Authorization", "Bearer gw-key")
	if w := do(t, s.Handler(), r); w.Code != http.StatusPaymentRequired {
		t.Fatalf("client status = %d body %s, want the 402 surfaced", w.Code, w.Body.String())
	}

	def, ok := s.cur().pool.Get("cline")
	if !ok {
		t.Fatal("cline provider missing from the pool")
	}
	if names := def.Invalidated(); len(names) != 0 {
		t.Errorf("accounts invalidated = %v, want none: the free lane serves on this balance", names)
	}
	if benched, ready := def.ModelBenched("deepseek/deepseek-v4.1-flash"); !benched || ready.IsZero() {
		t.Errorf("paid model benched=%v ready=%v, want it benched so the router skips it", benched, ready)
	}

	// The same account must still serve the sibling lane: that is the whole
	// point of not invalidating it.
	r2 := chatReq(t, "cline/inclusionai/ling-3.0-flash-fin:free")
	r2.Header.Set("Authorization", "Bearer gw-key")
	if w := do(t, s.Handler(), r2); w.Code != http.StatusOK {
		t.Fatalf("free lane after the credit wall: %d %s", w.Code, w.Body.String())
	}
	paid := 0
	for _, m := range hits {
		if m ***REMOVED*** "deepseek/deepseek-v4.1-flash" {
			paid++
		}
	}
	if paid ***REMOVED*** 0 || hits[len(hits)-1] != "inclusionai/ling-3.0-flash-fin:free" {
		t.Errorf("upstream hits = %v, want the paid model answered then the free lane served", hits)
	}

	// The bench is what saves the next request: the target must be skipped
	// without another paid upstream call (the first request may still retry
	// once — every 402 in this arm is Fallbackable by #80's contract).
	before := paid
	r3 := chatReq(t, "cline/deepseek/deepseek-v4.1-flash")
	r3.Header.Set("Authorization", "Bearer gw-key")
	do(t, s.Handler(), r3)
	after := 0
	for _, m := range hits {
		if m ***REMOVED*** "deepseek/deepseek-v4.1-flash" {
			after++
		}
	}
	if after != before {
		t.Errorf("benched model reached upstream again (%d -> %d hits); the router must skip the target", before, after)
	}
}
