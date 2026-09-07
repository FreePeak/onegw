package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"onegw/internal/translat"
)

// searxStub mimics the SearXNG JSON API. records queries so tests can
// assert on what the gateway sent.
func searxStub(t *testing.T, n int, statusCode int) (*httptest.Server, *[]string) {
	t.Helper()
	queries := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*queries = append(*queries, r.URL.Query().Get("q"))
		if r.URL.Query().Get("format") != "json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if statusCode != 0 {
			w.WriteHeader(statusCode)
			return
		}
		var results []map[string]any
		for i := 0; i < n; i++ {
			results = append(results, map[string]any{
				"url":     fmt.Sprintf("https://example.com/result-%02d", i),
				"title":   fmt.Sprintf("Result %02d — q detail", i),
				"content": fmt.Sprintf("Snippet %02d text about the query.", i),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	return srv, queries
}

// searxDef builds a searxng Def pointed at the stub with default limits.
func searxDef(base string) *Def {
	return &Def{
		Name:     "search",
		Kind:     KindSearXNG,
		BaseURL:  base,
		Accounts: []Account{{Name: "default"}},
	}
}

// chatBody is the OpenAI request shape the server hands to Do().
func chatBody(query string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":    "search/query",
		"messages": []any{map[string]any{"role": "user", "content": query}},
	})
	return b
}

func decodeCompletion(t *testing.T, body []byte) (string, map[string]any) {
	t.Helper()
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("completion decode: %v\nbody: %s", err, body)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1; body: %s", len(resp.Choices), body)
	}
	return resp.Choices[0].Message.Content, resp.Usage
}

func TestSearchQueryExtraction(t *testing.T) {
	// Multi-turn: the LAST user message is the query.
	body := []byte(`{"model":"search/q","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"first question"},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":"  go   goldfish  lifespan  "}
	]}`)
	srv, queries := searxStub(t, 3, 0)
	d := searxDef(srv.URL)
	res, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", body, false)
	if apiErr != nil {
		t.Fatalf("Do: %v", apiErr)
	}
	defer res.Resp.Body.Close()
	if len(*queries) != 1 {
		t.Fatalf("search calls = %d, want 1", len(*queries))
	}
	if q := (*queries)[0]; q != "go goldfish lifespan" {
		t.Fatalf("query sent upstream = %q, want collapsed last user message", q)
	}
}

func TestSearchQueryFromPartsContent(t *testing.T) {
	// OpenAI parts-array content: text parts join into one query; images
	// are ignored.
	body := []byte(`{"model":"search/q","messages":[{"role":"user","content":[
		{"type":"text","text":"rust "},{"type":"image_url","image_url":{"url":"data:image/png;base64,xxx"}},
		{"type":"text","text":"vs zig"}
	]}]}`)
	srv, queries := searxStub(t, 1, 0)
	d := searxDef(srv.URL)
	if _, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", body, false); apiErr != nil {
		t.Fatalf("Do: %v", apiErr)
	}
	if q := (*queries)[0]; q != "rust vs zig" {
		t.Fatalf("query = %q, want joined text parts", q)
	}
}

func TestSearchNoUserMessageIs400(t *testing.T) {
	d := searxDef("http://127.0.0.1:1")
	_, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", []byte(`{"messages":[{"role":"assistant","content":"hi"}]}`), false)
	if apiErr == nil || apiErr.Status != 400 || apiErr.Type != "invalid_request" {
		t.Fatalf("no user message: got %v, want 400 invalid_request", apiErr)
	}
}

func TestSearchNonStreamingCompletion(t *testing.T) {
	srv, _ := searxStub(t, 5, 0)
	d := searxDef(srv.URL)
	res, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("q"), false)
	if apiErr != nil {
		t.Fatalf("Do: %v", apiErr)
	}
	defer res.Resp.Body.Close()
	if res.Format != translat.FmtOpenAI {
		t.Fatalf("format = %s, want openai", res.Format)
	}
	b, err := io.ReadAll(res.Resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content, usage := decodeCompletion(t, b)
	if !strings.Contains(content, "https://example.com/result-00") ||
		!strings.Contains(content, "Result 00") ||
		!strings.Contains(content, "Snippet 00") ||
		!strings.Contains(content, "Snippet 04") {
		t.Fatalf("content missing formatted results:\n%s", content)
	}
	if strings.Contains(content, "result-05") {
		t.Fatal("content includes more results than exist")
	}
	// Markdown link shape.
	if !strings.Contains(content, "[Result 00 — q detail](https://example.com/result-00)") {
		t.Fatalf("content is not markdown-formatted:\n%s", content)
	}
	// Usage estimated chars/4, nonzero.
	in, inOK := usage["prompt_tokens"].(float64)
	out, outOK := usage["completion_tokens"].(float64)
	if !inOK || !outOK || in < 1 || out < 1 {
		t.Fatalf("usage = %v, want positive prompt/completion tokens", usage)
	}
}

func TestSearchMaxResultsCap(t *testing.T) {
	srv, _ := searxStub(t, 20, 0)
	d := searxDef(srv.URL)
	d.SearchMaxResults = 2
	res, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("q"), false)
	if apiErr != nil {
		t.Fatalf("Do: %v", apiErr)
	}
	defer res.Resp.Body.Close()
	b, _ := io.ReadAll(res.Resp.Body)
	content, _ := decodeCompletion(t, b)
	if strings.Contains(content, "result-02") || !strings.Contains(content, "result-01") {
		t.Fatalf("max_results=2 not honored:\n%s", content)
	}
}

func TestSearchStreamingIsOpenAISSE(t *testing.T) {
	srv, _ := searxStub(t, 3, 0)
	d := searxDef(srv.URL)
	res, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("q"), true)
	if apiErr != nil {
		t.Fatalf("Do: %v", apiErr)
	}
	defer res.Resp.Body.Close()
	b, _ := io.ReadAll(res.Resp.Body)
	s := string(b)
	if !strings.HasPrefix(s, "data: ") || !strings.HasSuffix(s, "data: [DONE]\n\n") {
		t.Fatalf("stream is not OpenAI SSE:\n%s", s)
	}
	if strings.Count(s, "data: ") != 3 {
		t.Fatalf("stream events = %d, want role+delta, finish, DONE:\n%s", strings.Count(s, "data: "), s)
	}
	// The result text must arrive in the delta chunk.
	if !strings.Contains(s, "https://example.com/result-00") {
		t.Fatalf("stream delta missing result content:\n%s", s)
	}
	// The finish chunk (finish_reason inside choices[0]) must carry usage.
	var sawStop bool
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var c struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("chunk decode: %v in %q", err, line)
		}
		if len(c.Choices) > 0 && c.Choices[0].FinishReason == "stop" {
			sawStop = true
			if tok, _ := c.Usage["completion_tokens"].(float64); tok < 1 {
				t.Fatal("finish chunk missing usage")
			}
		}
	}
	if !sawStop {
		t.Fatal("no finish_reason=stop chunk in stream")
	}
}

func TestSearchUpstreamDownIsRetryable503(t *testing.T) {
	// Nothing listens here → connection refused.
	d := searxDef("http://127.0.0.1:1")
	_, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("q"), false)
	if apiErr == nil || apiErr.Status != 503 || apiErr.Type != "search_unavailable" {
		t.Fatalf("down upstream: got %v, want 503 search_unavailable", apiErr)
	}
	if !apiErr.Retryable() {
		t.Fatal("503 search_unavailable must be retryable so combos fall through")
	}
}

func TestSearchUpstreamErrorIsRetryable503(t *testing.T) {
	for _, code := range []int{401, 429, 500, 503} {
		srv, _ := searxStub(t, 0, code)
		d := searxDef(srv.URL)
		_, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("q"), false)
		if apiErr == nil || apiErr.Status != 503 || apiErr.Type != "search_unavailable" {
			t.Fatalf("upstream %d: got %v, want 503 search_unavailable", code, apiErr)
		}
		if !apiErr.Retryable() {
			t.Fatalf("upstream %d: must map to retryable", code)
		}
	}
}

func TestSearchTimeout(t *testing.T) {
	// Instance hangs: the per-search timeout must cut the call short and
	// yield the retryable 503, not block until the client gives up.
	hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer hold.Close()
	d := searxDef(hold.URL)
	d.SearchTimeout = 50 * time.Millisecond
	start := time.Now()
	_, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("q"), false)
	if time.Since(start) > 2*time.Second {
		t.Fatal("search timeout did not bound the call")
	}
	if apiErr == nil || apiErr.Status != 503 || apiErr.Type != "search_unavailable" {
		t.Fatalf("timeout: got %v, want 503 search_unavailable", apiErr)
	}
}

func TestSearchAuthViaExtraHeaders(t *testing.T) {
	var gotKey, gotBasic string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotBasic = r.Header.Get("X-API-Key"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	d := searxDef(srv.URL)
	d.ExtraHeaders = map[string]string{"X-API-Key": "sk-test-searxng-key", "Authorization": "Basic dXNlcl90ZXN0OnB3"}
	if _, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("q"), false); apiErr != nil {
		t.Fatalf("Do: %v", apiErr)
	}
	if gotKey != "sk-test-searxng-key" {
		t.Fatalf("X-API-Key = %q, want value from extra_headers", gotKey)
	}
	if gotBasic != "Basic dXNlcl90ZXN0OnB3" {
		t.Fatalf("Authorization = %q, want basic auth from extra_headers", gotBasic)
	}
}

func TestSearchEmptyResultsContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	d := searxDef(srv.URL)
	res, apiErr := d.Do(context.Background(), &d.Accounts[0], "search/query", chatBody("lonely query"), false)
	if apiErr != nil {
		t.Fatalf("Do: %v", apiErr)
	}
	defer res.Resp.Body.Close()
	b, _ := io.ReadAll(res.Resp.Body)
	content, _ := decodeCompletion(t, b)
	if !strings.Contains(content, "No results found.") || !strings.Contains(content, "lonely query") {
		t.Fatalf("empty-results content wrong:\n%s", content)
	}
}

func TestSearchLimitsDefaultsAndClamps(t *testing.T) {
	d := searxDef("http://x")
	n, to := d.searchLimits()
	if n != SearchDefaultMaxResults || to != SearchDefaultTimeout {
		t.Fatalf("zero-value limits = %d/%s, want 5/10s", n, to)
	}
	d.SearchMaxResults = 500
	d.SearchTimeout = -1 * time.Second
	n, to = d.searchLimits()
	if n != searchMaxResultsHardCap {
		t.Fatalf("hard cap = %d, want %d", n, searchMaxResultsHardCap)
	}
	if to != SearchDefaultTimeout {
		t.Fatalf("invalid timeout should fall back to default, got %s", to)
	}
}

func TestParseSearchTimeout(t *testing.T) {
	if d := ParseSearchTimeout(""); d != SearchDefaultTimeout {
		t.Fatalf("empty = %s, want default", d)
	}
	if d := ParseSearchTimeout("1m30s"); d != 90*time.Second {
		t.Fatalf("1m30s = %s", d)
	}
	if d := ParseSearchTimeout("garbage"); d != SearchDefaultTimeout {
		t.Fatalf("garbage = %s, want default", d)
	}
	if d := ParseSearchTimeout("-5s"); d != SearchDefaultTimeout {
		t.Fatalf("-5s = %s, want default", d)
	}
}

func TestCapBytesDoesNotSplitRune(t *testing.T) {
	s := "héllo wörld ünïcode"
	got := capBytes(s, 6)
	if len(got) > 6 {
		t.Fatalf("capBytes returned %d bytes, want <= 6", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("capBytes split a rune: %q", got)
	}
	want := "héllo"
	if got != want {
		t.Fatalf("cut = %q, want %q", got, want)
	}
}
