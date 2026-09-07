package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onegw/internal/config"
)

// searxUpstream is an e2e SearXNG stub serving a fixture result list and
// recording every query and key it receives.
type searxUpstream struct {
	srv     *httptest.Server
	queries *[]string
	keys    *[]string
}

func newSearxUpstream(t *testing.T, results string) *searxUpstream {
	t.Helper()
	up := &searxUpstream{queries: &[]string{}, keys: &[]string{}}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*up.queries = append(*up.queries, r.URL.Query().Get("q"))
		*up.keys = append(*up.keys, r.Header.Get("X-API-Key"))
		if r.URL.Query().Get("format") != "json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(results))
	}))
	t.Cleanup(up.srv.Close)
	return up
}

const searxFixtures = `{"results":[
	{"url":"https://onegw.dev/docs","title":"onegw docs","content":"The gateway config guide explains virtual providers."},
	{"url":"https://onegw.dev/prd","title":"PRD","content":"Search is fail-open across combos."},
	{"url":"https://example.org/x","title":"X","content":"Third fixture result."}
]}`

func withKey(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer sk-test")
	return r
}

func searchCfg(t *testing.T, searxBase string, opts func(*config.ProviderCfg)) *Server {
	t.Helper()
	realUp := upstreamStub("m1")
	search := config.ProviderCfg{
		Name: "search", Kind: "searxng", BaseURL: searxBase,
		ExtraHeader: map[string]string{"X-API-Key": "sk-test-searxng"},
		MaxResults:  3, Timeout: "5s", Models: []string{"web"},
	}
	if opts != nil {
		opts(&search)
	}
	cfg := &config.Config{
		Providers: []config.ProviderCfg{
			search,
			{Name: "real", Kind: "openai", BaseURL: realUp.URL, APIKey: "sk-test-up", Models: []string{"m1"}},
		},
		Combos: []config.ComboCfg{{Name: "withsearch", Targets: []string{"search/query", "real/m1"}}},
	}
	cfg.Server.DataDir = "memory"
	cfg.Server.AdminPassword = "pw"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test"}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func chatReqStr(t *testing.T, model, content string, stream bool) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": content}},
		"stream":   stream,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestSearchE2ENonStreaming(t *testing.T) {
	up := newSearxUpstream(t, searxFixtures)
	srv := searchCfg(t, up.srv.URL, nil)
	h := srv.Handler()

	w := do(t, h, withKey(chatReqStr(t, "search/query", "what is onegw", false)))
	if w.Code != 200 {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]int64 `json:"usage"`
		Model string           `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	content := resp.Choices[0].Message.Content
	for _, want := range []string{
		"[onegw docs](https://onegw.dev/docs)",
		"The gateway config guide explains virtual providers.",
		"[PRD](https://onegw.dev/prd)",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q:\n%s", want, content)
		}
	}
	if resp.Usage["completion_tokens"] < 1 || resp.Usage["prompt_tokens"] < 1 {
		t.Fatalf("usage not surfaced: %v", resp.Usage)
	}
	if len(*up.queries) != 1 || (*up.queries)[0] != "what is onegw" {
		t.Fatalf("searxng got queries %v", *up.queries)
	}
	if (*up.keys)[0] != "sk-test-searxng" {
		t.Fatalf("X-API-Key not forwarded from extra_headers: %q", (*up.keys)[0])
	}
	if resp.Model != "query" {
		t.Fatalf("response model = %q, want routed model", resp.Model)
	}
}

func TestSearchE2EStreaming(t *testing.T) {
	up := newSearxUpstream(t, searxFixtures)
	srv := searchCfg(t, up.srv.URL, nil)
	h := srv.Handler()

	w := do(t, h, withKey(chatReqStr(t, "search/query", "gateway docs", true)))
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	s := w.Body.String()
	if !strings.Contains(s, "https://onegw.dev/docs") {
		t.Fatalf("stream missing result content:\n%s", s)
	}
	if !strings.HasSuffix(s, "data: [DONE]\n\n") {
		t.Fatalf("stream missing [DONE]:\n%s", s)
	}
	// Reconstruct content from deltas; the finish chunk must carry usage.
	var content strings.Builder
	var stopSeen, usageSeen bool
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("chunk: %v", err)
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			if ch.FinishReason == "stop" {
				stopSeen = true
			}
		}
		if len(c.Usage) > 0 {
			usageSeen = true
		}
	}
	if got := content.String(); !strings.Contains(got, "https://onegw.dev/docs") {
		t.Fatalf("assembled content wrong:\n%s", got)
	}
	if !stopSeen {
		t.Fatal("no finish_reason=stop chunk")
	}
	if !usageSeen {
		t.Fatal("no usage chunk in stream")
	}
	// The same-format passthrough is sniffed by attempt(): the request must
	// be counted with chars/4 token estimates from the synthetic usage.
	reqs, in, out, _ := srv.cur().usage.Totals()
	if reqs != 1 || in < 1 || out < 1 {
		t.Fatalf("usage totals req=%d in=%d out=%d, want counted search request with chars/4 tokens", reqs, in, out)
	}
}

func TestSearchE2EComboFallsThroughWhenDown(t *testing.T) {
	up := newSearxUpstream(t, searxFixtures)
	url := up.srv.URL
	up.srv.Close() // SearXNG instance is down from here on

	srv := searchCfg(t, url, nil)
	h := srv.Handler()

	// Direct search route: the retryable 503 surfaces to the client.
	w := do(t, h, withKey(chatReqStr(t, "search/query", "anything", false)))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "search_unavailable") {
		t.Fatalf("direct down-search: code=%d body=%s, want 503 search_unavailable", w.Code, w.Body.String())
	}

	// Combo: search falls through to the real provider.
	w = do(t, h, withKey(chatReqStr(t, "withsearch", "anything", false)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("combo fall-through: code=%d body=%s, want pong from real provider", w.Code, w.Body.String())
	}
}

func TestSearchE2ETimeout(t *testing.T) {
	// Instance hangs; the configured search timeout must cut the call and
	// yield the retryable fail-open error.
	hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hold.Close()
	srv := searchCfg(t, hold.URL, func(p *config.ProviderCfg) { p.Timeout = "100ms" })
	h := srv.Handler()

	w := do(t, h, withKey(chatReqStr(t, "search/query", "slow", false)))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "search_unavailable") {
		t.Fatalf("timeout: code=%d body=%s, want 503 search_unavailable", w.Code, w.Body.String())
	}
}

func TestSearchE2EModelsListing(t *testing.T) {
	up := newSearxUpstream(t, searxFixtures)
	srv := searchCfg(t, up.srv.URL, nil)
	h := srv.Handler()

	w := do(t, h, withKey(httptest.NewRequest(http.MethodGet, "/v1/models", nil)))
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range listing.Data {
		ids[m.ID] = true
	}
	if !ids["search/query"] {
		t.Fatalf("search provider not surfaced in /v1/models: %v", ids)
	}
	if !ids["search/web"] {
		t.Fatalf("configured search alias missing from /v1/models: %v", ids)
	}
	if !ids["real/m1"] || !ids["withsearch"] {
		t.Fatalf("other model ids missing: %v", ids)
	}
}

func TestSearchE2EAnthropicSurface(t *testing.T) {
	up := newSearxUpstream(t, searxFixtures)
	srv := searchCfg(t, up.srv.URL, nil)
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{
		"model":      "search/query",
		"messages":   []any{map[string]any{"role": "user", "content": "gateway docs"}},
		"max_tokens": 100,
		"stream":     true,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sk-test")
	w := do(t, h, r)
	if w.Code != 200 {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	s := w.Body.String()
	if !strings.Contains(s, "message_start") || !strings.Contains(s, "message_stop") {
		t.Fatalf("not an Anthropic stream:\n%s", s)
	}
	if !strings.Contains(s, "https://onegw.dev/docs") {
		t.Fatalf("translated stream missing result content:\n%s", s)
	}
	// The query must have been extracted from the Anthropic body.
	if len(*up.queries) != 1 || (*up.queries)[0] != "gateway docs" {
		t.Fatalf("searxng queries = %v", *up.queries)
	}
}
