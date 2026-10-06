package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"onegw/internal/config"
)

// respUpstream is a minimal OpenAI Responses upstream: every POST answers
// one response (id resp_<name>_<n>) whose output carries an encrypted
// reasoning item and an assistant message, with OpenAI-shaped usage
// (reasoning inside output_tokens, total = input + output). It records
// every request body and serves GET/DELETE on the ids it minted.
type respUpstream struct {
	name  string
	srv   *httptest.Server
	mu    sync.Mutex
	n     int
	reqs  []map[string]any
	paths []string
	auth  []string
}

func newRespUpstream(t *testing.T, name string) *respUpstream {
	t.Helper()
	u := &respUpstream{name: name}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *respUpstream) object(n int, model string) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("resp_%s_%d", u.name, n), "object": "response", "model": model, "status": "completed",
		"output": []any{
			map[string]any{"type": "reasoning", "id": fmt.Sprintf("rs_%s_%d", u.name, n), "summary": []any{}, "encrypted_content": "opaque"},
			map[string]any{"type": "message", "id": fmt.Sprintf("msg_%s_%d", u.name, n), "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("answer %d from %s", n, u.name), "annotations": []any{}}}},
		},
		"usage": map[string]any{"input_tokens": 40, "output_tokens": 20, "total_tokens": 60,
			"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 8}},
	}
}

func (u *respUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.paths = append(u.paths, r.Method+" "+r.URL.RequestURI())
	u.auth = append(u.auth, r.Header.Get("Authorization"))
	u.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			fmt.Fprintf(w, `{"id":%q,"object":"response","deleted":true}`, strings.TrimPrefix(r.URL.Path, "/v1/responses/"))
			return
		}
		fmt.Fprintf(w, `{"id":%q,"object":"response","served_by":%q}`, strings.TrimPrefix(r.URL.Path, "/v1/responses/"), u.name)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	u.mu.Lock()
	u.n++
	n := u.n
	u.reqs = append(u.reqs, body)
	u.mu.Unlock()
	model, _ := body["model"].(string)
	obj := u.object(n, model)
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		created := map[string]any{"id": obj["id"], "object": "response", "status": "in_progress", "output": []any{}, "usage": nil}
		for _, ev := range []map[string]any{
			{"type": "response.created", "sequence_number": 0, "response": created},
			{"type": "response.output_text.delta", "sequence_number": 1, "delta": "answer"},
			{"type": "response.completed", "sequence_number": 2, "response": obj},
		} {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
			w.(http.Flusher).Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(obj)
}

func (u *respUpstream) last() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		return nil
	}
	return u.reqs[len(u.reqs)-1]
}

func (u *respUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

// respCfg: providers p1, p2 (openai, one account each), "jev" (systemone),
// combo "pair" = [p1/m, p2/m]; key "open" unrestricted, key "pool" limited
// to the combo. p1 gets a daily token limit of p1Limit (0 = none).
func respCfg(t *testing.T, p1, p2 *respUpstream, p1Limit int64, history bool) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.AdminPassword = "admin"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "open-key", Name: "open"}, {Key: "pool-key", Name: "pool", Models: []string{"pair"}}}
	p1cfg := config.ProviderCfg{Name: "p1", Kind: "openai", BaseURL: p1.srv.URL, APIKey: "sk-p1", Models: []string{"m"}}
	if p1Limit > 0 {
		p1cfg.QuotaWindow, p1cfg.QuotaLimitTokens = "daily", p1Limit
	}
	cfg.Providers = []config.ProviderCfg{
		p1cfg,
		{Name: "p2", Kind: "openai", BaseURL: p2.srv.URL, APIKey: "sk-p2", Models: []string{"m"}},
		{Name: "jev", Kind: "systemone", BaseURL: p2.srv.URL, APIKey: "sk-jev"},
	}
	cfg.Combos = []config.ComboCfg{{Name: "pair", Targets: []string{"p1/m", "p2/m"}}}
	cfg.Responses.History = history
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	return cfg
}

func respReq(t *testing.T, key string, body map[string]any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

func respID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil || obj.ID == "" {
		t.Fatalf("no response id in %s (%v)", w.Body.String(), err)
	}
	return obj.ID
}

// waitQuotaUsed polls until provider name's window shows want tokens.
func waitQuotaUsed(t *testing.T, srv *Server, name string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, ok := srv.cur().quota.Status(name, time.Now())
		if ok && st.UsedTokens == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota %s used = %d, want %d", name, st.UsedTokens, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResponsesNonStreamRelaysAndCharges(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 1000, false))
	w := do(t, srv.Handler(), respReq(t, "open-key", map[string]any{
		"model": "p1/m", "input": "hi", "store": true, "tools": []any{map[string]any{"type": "function", "name": "f"}}}))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if got := respID(t, w); got != "resp_p1_1" {
		t.Fatalf("client got id %s", got)
	}
	up := p1.last()
	if up["model"] != "m" || up["input"] != "hi" || up["store"] != true || up["tools"] == nil {
		t.Fatalf("upstream body not relayed verbatim with the routed model: %v", up)
	}
	if p1.paths[0] != "POST /v1/responses" || p1.auth[0] != "Bearer sk-p1" {
		t.Fatalf("upstream call = %s auth %s", p1.paths[0], p1.auth[0])
	}
	// total_tokens (60) is charged, not 40+20+8.
	waitQuotaUsed(t, srv, "p1", 60)
	if reqs, in, out, _ := srv.cur().usage.Totals(); reqs != 1 || in != 40 || out != 20 {
		t.Fatalf("usage totals = %d/%d/%d", reqs, in, out)
	}
}

func TestResponsesStreamRelaysAndCharges(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 1000, true))
	w := do(t, srv.Handler(), respReq(t, "open-key", map[string]any{"model": "p1/m", "input": "hi", "stream": true}))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d ct %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, ev := range []string{"event: response.created", "response.output_text.delta", "event: response.completed", "answer 1 from p1"} {
		if !strings.Contains(w.Body.String(), ev) {
			t.Fatalf("stream missing %q:\n%s", ev, w.Body.String())
		}
	}
	waitQuotaUsed(t, srv, "p1", 60)
	row, ok, _ := srv.st.GetResponse("resp_p1_1")
	if !ok || row.Provider != "p1" || !strings.Contains(row.Output, "answer 1 from p1") || !strings.Contains(row.Input, `"hi"`) {
		t.Fatalf("stream affinity/history not recorded: %+v", row)
	}
}

func TestResponsesAllowlistAndTargets(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 0, false))
	h := srv.Handler()
	for _, model := range []string{"m", "p1/m", "p2/m"} {
		if w := do(t, h, respReq(t, "pool-key", map[string]any{"model": model, "input": "x"})); w.Code != http.StatusForbidden {
			t.Fatalf("pool key on %q: want 403, got %d %s", model, w.Code, w.Body.String())
		}
	}
	if w := do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "input": "x"})); w.Code != http.StatusOK {
		t.Fatalf("pool key on its combo: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, respReq(t, "open-key", map[string]any{"model": "jev/jev-latest", "input": "x"})); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "responses_not_supported") {
		t.Fatalf("non-openai target: want 400 responses_not_supported, got %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, respReq(t, "nope", map[string]any{"model": "pair", "input": "x"})); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", w.Code)
	}
}

func TestResponsesComboFallsThroughOnExhaustedQuota(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 50, false))
	h := srv.Handler()
	if w := do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "input": "x"})); respID(t, w) != "resp_p1_1" {
		t.Fatalf("first turn should be served by p1: %s", w.Body.String())
	}
	waitQuotaUsed(t, srv, "p1", 60) // over the 50-token limit
	w := do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "input": "x"}))
	if respID(t, w) != "resp_p2_1" || p1.hits() != 1 {
		t.Fatalf("exhausted p1 must fall through to p2 without an upstream call: %s (p1 hits %d)", w.Body.String(), p1.hits())
	}
}

func TestResponsesPreviousIDStaysOnItsProvider(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 0, false))
	h := srv.Handler()
	first := respID(t, do(t, h, respReq(t, "open-key", map[string]any{"model": "p2/m", "input": "x"})))
	// "pair" would pick p1 first; the continuation must go to p2.
	w := do(t, h, respReq(t, "open-key", map[string]any{"model": "pair", "input": "y", "previous_response_id": first}))
	if respID(t, w) != "resp_p2_2" || p1.hits() != 0 {
		t.Fatalf("continuation left its provider: %s (p1 hits %d)", w.Body.String(), p1.hits())
	}
	if got := p2.last()["previous_response_id"]; got != first {
		t.Fatalf("pinned continuation must keep previous_response_id, upstream got %v", got)
	}
	// Another key cannot continue (or read) a response it did not create.
	if w := do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "input": "y", "previous_response_id": first})); w.Code != http.StatusNotFound {
		t.Fatalf("foreign previous_response_id: want 404, got %d %s", w.Code, w.Body.String())
	}
}

func TestResponsesMigratesWhenPinnedProviderExhausted(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 50, true))
	h := srv.Handler()
	first := respID(t, do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "input": "question one"})))
	if first != "resp_p1_1" {
		t.Fatalf("first turn not on p1: %s", first)
	}
	waitQuotaUsed(t, srv, "p1", 60)

	w := do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "previous_response_id": first,
		"input": []any{map[string]any{"role": "user", "content": "question two"}}}))
	second := respID(t, w)
	if second != "resp_p2_1" || p1.hits() != 1 {
		t.Fatalf("continuation should have moved to p2: %s (p1 hits %d)", w.Body.String(), p1.hits())
	}
	up := p2.last()
	if _, has := up["previous_response_id"]; has {
		t.Fatalf("moved request must not carry previous_response_id: %v", up)
	}
	in, _ := json.Marshal(up["input"])
	want := `[{"content":"question one","role":"user"},{"content":"answer 1 from p1","role":"assistant"},{"content":"question two","role":"user"}]`
	if string(in) != want {
		t.Fatalf("rebuilt input\n got %s\nwant %s", in, want)
	}
	// The moved turn chains to the original one, and the next turn stays
	// on p2 with a normal previous_response_id.
	if row, _, _ := srv.st.GetResponse(second); row.PrevID != first || row.Provider != "p2" {
		t.Fatalf("moved turn row = %+v", row)
	}
	w = do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "previous_response_id": second, "input": "three"}))
	if respID(t, w) != "resp_p2_2" || p2.last()["previous_response_id"] != second {
		t.Fatalf("turn after the move should stay pinned on p2: %s / %v", w.Body.String(), p2.last())
	}
}

func TestResponsesMoveNeedsHistory(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 50, false))
	h := srv.Handler()
	first := respID(t, do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "input": "q"})))
	waitQuotaUsed(t, srv, "p1", 60)
	w := do(t, h, respReq(t, "pool-key", map[string]any{"model": "pair", "previous_response_id": first, "input": "q2"}))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "history") || p2.hits() != 0 {
		t.Fatalf("want 409 naming history, got %d %s (p2 hits %d)", w.Code, w.Body.String(), p2.hits())
	}
}

func TestResponsesBackgroundAndConversationGuards(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 1000, false))
	h := srv.Handler()
	if w := do(t, h, respReq(t, "open-key", map[string]any{"model": "pair", "input": "x", "background": true})); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "background") {
		t.Fatalf("background on a token-limited route: want 400, got %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, respReq(t, "open-key", map[string]any{"model": "p2/m", "input": "x", "background": true})); w.Code != http.StatusOK {
		t.Fatalf("background on an unlimited provider must pass: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, respReq(t, "open-key", map[string]any{"model": "pair", "input": "x", "conversation": "conv_1"})); w.Code != http.StatusBadRequest {
		t.Fatalf("conversation on a combo: want 400, got %d", w.Code)
	}
	if w := do(t, h, respReq(t, "open-key", map[string]any{"model": "p2/m", "input": "x", "conversation": "conv_1"})); w.Code != http.StatusOK {
		t.Fatalf("conversation on a single provider must pass: %d %s", w.Code, w.Body.String())
	}
}

func TestResponsesResourceEndpointsFollowAffinity(t *testing.T) {
	p1, p2 := newRespUpstream(t, "p1"), newRespUpstream(t, "p2")
	srv := ptSrv(t, respCfg(t, p1, p2, 0, false))
	h := srv.Handler()
	id := respID(t, do(t, h, respReq(t, "open-key", map[string]any{"model": "p2/m", "input": "x"})))
	call := func(method, path, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		return do(t, h, r)
	}
	if w := call(http.MethodGet, "/v1/responses/"+id+"/input_items?limit=5", "open-key"); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"served_by":"p2"`) {
		t.Fatalf("input_items: %d %s", w.Code, w.Body.String())
	}
	if got := p2.paths[len(p2.paths)-1]; got != "GET /v1/responses/"+id+"/input_items?limit=5" {
		t.Fatalf("upstream path = %s", got)
	}
	if w := call(http.MethodGet, "/v1/responses/"+id, "pool-key"); w.Code != http.StatusNotFound {
		t.Fatalf("other key must not see the response: %d", w.Code)
	}
	if w := call(http.MethodGet, "/v1/responses/resp_unknown", "open-key"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", w.Code)
	}
	if w := call(http.MethodPost, "/v1/responses/"+id+"/cancel", "open-key"); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d", w.Code)
	}
	if w := call(http.MethodDelete, "/v1/responses/"+id, "open-key"); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, ok, _ := srv.st.GetResponse(id); ok {
		t.Fatal("a deleted response must be forgotten")
	}
	if p1.hits() != 0 || len(p1.paths) != 0 {
		t.Fatalf("p1 must never be called: %v", p1.paths)
	}
}

func TestPortableItemsDropsAccountBoundState(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"q"}`),
		json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"x"}`),
		json.RawMessage(`{"type":"web_search_call","id":"ws_1","status":"completed"}`),
		json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_1","output":"42"}`),
		json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"it is 42"}]}`),
		json.RawMessage(`{"type":"item_reference","id":"fc_1"}`),
	}
	out, err := portableItems(items)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out)
	want := `[{"content":"q","role":"user"},{"arguments":"{}","call_id":"call_1","name":"f","type":"function_call"},{"call_id":"call_1","output":"42","type":"function_call_output"},{"content":"it is 42","role":"assistant"},{"arguments":"{}","call_id":"call_1","name":"f","type":"function_call"}]`
	if string(got) != want {
		t.Fatalf("portable items\n got %s\nwant %s", got, want)
	}
	if _, err := portableItems([]json.RawMessage{json.RawMessage(`{"type":"item_reference","id":"nope"}`)}); err == nil {
		t.Fatal("a dangling item_reference must fail the move")
	}
}
