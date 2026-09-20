package provider

// SearXNG virtual provider (kind = "searxng"): the OpenAI chat surface
// doubles as a web-search surface. Clients address it with model
// "<provider>/<anything>" (e.g. "search/query"); the gateway extracts the
// last user message as the query, performs one SearXNG JSON search
// (GET {base_url}/search?q=…&format=json), and answers with a synthetic
// OpenAI completion whose content is a markdown list of title+URL+snippet.
//
// Do() returns that synthetic completion through the same CallResult
// contract a real upstream uses, so the entire existing pipeline serves
// search unchanged: same-format passthrough, cross-format stream
// translation (Anthropic/Gemini clients included), usage sniffing (the
// synthetic body carries chars/4 estimates), and combo fall-through —
// SearXNG failures surface as retryable 503 search_unavailable, so a combo
// like [search/query, gpt-5.5] degrades to the real model when search is
// down instead of failing the request.
//
// Auth: SearXNG instances that require a key accept X-API-Key or basic
// auth; configure either via the provider's existing extra_headers — this
// kind adds no auth code of its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"onegw/internal/translat"
	"onegw/internal/types"
)

// KindSearXNG is the virtual web-search kind. Its Format() is OpenAI (the
// synthetic completion it returns), so no translation case is needed.
const KindSearXNG Kind = "searxng"

// Search tuning: config-driven within hard caps. Defaults match the
// documented zero values (5 results, 10 s timeout).
const (
	SearchDefaultMaxResults = 5
	SearchDefaultTimeout    = 10 * time.Second

	searchMaxResultsHardCap = 50      // bound on results formatted per call
	searchQueryCap          = 1 << 10 // bytes of user text sent as q
	searchSnippetCap        = 300     // bytes per result snippet (token diet)
	searchTitleCap          = 200     // bytes per result title
	searchResponseCap       = 4 << 20 // SearXNG JSON body read cap
)

// ParseSearchTimeout turns a provider timeout string ("10s", "1m") into a
// duration; empty or invalid falls back to SearchDefaultTimeout.
func ParseSearchTimeout(s string) time.Duration {
	if s == "" {
		return SearchDefaultTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return SearchDefaultTimeout
	}
	return d
}

// searchLimits clamps the configured knobs into sane ranges at use time so
// hand-built Defs (tests, future callers) behave like configured ones.
func (d *Def) searchLimits() (int, time.Duration) {
	n := d.SearchMaxResults
	if n <= 0 {
		n = SearchDefaultMaxResults
	}
	if n > searchMaxResultsHardCap {
		n = searchMaxResultsHardCap
	}
	to := d.SearchTimeout
	if to <= 0 {
		to = SearchDefaultTimeout
	}
	return n, to
}

// doSearch answers one chat request with a SearXNG search. body is the
// OpenAI-format request the server already normalized every client surface
// into, so query extraction handles exactly one shape.
func (d *Def) doSearch(ctx context.Context, acct *Account, model string, body []byte, stream bool) (*CallResult, *types.APIError) {
	query, err := searchQuery(body)
	if err != nil {
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: err.Error()}
	}
	results, serr := d.searxSearch(ctx, acct, query)
	if serr != nil {
		return nil, serr
	}
	markdown := formatSearchResults(query, results)
	in, out := estSearchTokens(query), estSearchTokens(markdown)

	var payload []byte
	if stream {
		payload = searchSSE(model, markdown, in, out)
	} else {
		payload = searchCompletion(model, markdown, in, out)
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
	return &CallResult{Resp: resp, Format: translat.FmtOpenAI, Acct: acct}, nil
}

// searxResult is one SearXNG JSON API result (fields we surface).
type searxResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// searxSearch performs one SearXNG JSON API call. Any instance-side failure
// (unreachable, timeout, non-2xx, unparseable) becomes a retryable error so
// combos fall through to the next target — search is fail-open.
func (d *Def) searxSearch(ctx context.Context, acct *Account, query string) ([]searxResult, *types.APIError) {
	base := strings.TrimRight(d.Base(acct), "/")
	if base == "" {
		return nil, searchUnavailable(fmt.Errorf("no base_url configured"))
	}
	maxResults, timeout := d.searchLimits()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	u := base + "/search?" + url.Values{"q": {query}, "format": {"json"}}.Encode()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, searchUnavailable(err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range d.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, searchUnavailable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, searchUnavailable(fmt.Errorf("search endpoint returned %s", resp.Status))
	}
	var payload struct {
		Results []searxResult `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, searchResponseCap)).Decode(&payload); err != nil {
		return nil, &types.APIError{Status: 502, Type: "search_bad_response", Message: "searxng: " + err.Error()}
	}
	if len(payload.Results) > maxResults {
		payload.Results = payload.Results[:maxResults]
	}
	return payload.Results, nil
}

// searchUnavailable is the fail-open error shape: 503 is retryable, so
// Router.Execute moves on to the next combo target.
func searchUnavailable(cause error) *types.APIError {
	return &types.APIError{Status: 503, Type: "search_unavailable", Message: "searxng: " + cause.Error()}
}

// searchQuery extracts the query from an OpenAI-format chat body: the text
// of the LAST user message, whitespace-collapsed and capped.
func searchQuery(body []byte) (string, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", fmt.Errorf("search: bad request body: %w", err)
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		if q := collapseSpace(flattenUserContent(m.Content)); q != "" {
			return capBytes(q, searchQueryCap), nil
		}
	}
	return "", fmt.Errorf("search: no user message with text content")
}

// flattenUserContent renders OpenAI message content — a plain string or a
// parts array — as text. Non-text parts (images) are ignored.
func flattenUserContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// formatSearchResults renders the markdown list that becomes the assistant
// message content: numbered title links with capped snippets.
func formatSearchResults(query string, results []searxResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q\n\n", query)
	if len(results) == 0 {
		b.WriteString("No results found.")
		return b.String()
	}
	for i, r := range results {
		title := collapseSpace(r.Title)
		if title == "" {
			title = r.URL
		}
		fmt.Fprintf(&b, "%d. [%s](%s)", i+1, capBytes(title, searchTitleCap), r.URL)
		if snip := collapseSpace(r.Content); snip != "" {
			fmt.Fprintf(&b, "\n   %s", capBytes(snip, searchSnippetCap))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// estSearchTokens is the chars/4 estimate carried in the synthetic usage so
// rollups stay sane even though no model ever saw this request.
func estSearchTokens(s string) int64 {
	if n := int64(len(s)) / 4; n > 0 {
		return n
	}
	return 1
}

func searchUsage(in, out int64) map[string]any {
	return map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
}

// searchCompletion builds the non-streaming OpenAI chat.completion body.
func searchCompletion(model, content string, in, out int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":      searchID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": searchUsage(in, out),
	})
	return b
}

// searchSSE frames the same completion as an OpenAI chunk stream: one delta
// carrying the whole formatted result, then a finish chunk with usage, then
// [DONE]. TranslateStream consumes these events for Anthropic/Gemini
// clients exactly like a real OpenAI upstream stream.
func searchSSE(model, content string, in, out int64) []byte {
	id, created := searchID(), time.Now().Unix()
	chunk := func(delta map[string]any, finish string, usage map[string]any) map[string]any {
		c := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			c["usage"] = usage
		}
		return c
	}
	var b bytes.Buffer
	write := func(c map[string]any) {
		if raw, err := json.Marshal(c); err == nil {
			b.WriteString("data: ")
			b.Write(raw)
			b.WriteString("\n\n")
		}
	}
	write(chunk(map[string]any{"role": "assistant", "content": content}, "", nil))
	write(chunk(map[string]any{}, "stop", searchUsage(in, out)))
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

func searchID() string {
	return fmt.Sprintf("chatcmpl-search-%d", time.Now().UnixNano())
}

// collapseSpace flattens all whitespace runs to single spaces (snippets and
// queries are rendered single-line).
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// capBytes truncates s to at most n bytes without splitting a UTF-8 rune.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return strings.ToValidUTF8(cut, "")
}
