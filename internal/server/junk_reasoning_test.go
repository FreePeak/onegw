package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// junkReasoningStream is the live junk shape (2026-09-21 free lane): a
// reasoning-only SSE stream whose decoded text is symbol soup. Every delta
// carries an EMPTY content string plus the fragment in "reasoning", which is
// what the vendor emitted while it burned its output cap.
const junkReasoningStreamFrag = ",kenO.RH*是RO儿*!m\"-WR*WJUNTOS/O *!! _____. 2469-0\"V|.\n" +
	") Conclusions\n  %k;**U 5*W}D?,0e a\n,T^!:+<Bl!*ZEr\n这说明\n:!  HOLA?f S-T.@#$!!!!*\"0>:E*|R! !!O>))T*?&$\"\"O7ade!z*ll* :无剧透.\n" +
	"  GENERat G,en*Oass?is!d\n  \"V 3P!!后?W f@ Gandhi, er?\n)*!!官\"Xv真//These!-*9*0/~~/habbas*\n"

// junkUpstream serves the junk stream forever (bounded by the test) and
// records the request bodies it saw.
type junkUpstream struct {
	mu   sync.Mutex
	srv  *httptest.Server
	bods [][]byte
	hits int
}

func newJunkUpstream() *junkUpstream {
	u := &junkUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bods = append(u.bods, b)
		u.hits++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 600; i++ {
			ev, _ := json.Marshal(map[string]any{
				"id": "cmpl-junk", "object": "chat.completion.chunk",
				"choices": []any{map[string]any{"index": 0,
					"delta": map[string]any{"content": "", "reasoning": junkReasoningStreamFrag}}},
			})
			_, _ = w.Write([]byte("data: " + string(ev) + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	return u
}

func (u *junkUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

// TestJunkReasoningFailsOverToSibling pins the user-facing contract: a combo
// whose first leg degenerates into symbol-soup reasoning must NOT paint that
// soup for the client. The guard withholds the stream head, so the attempt is
// discarded before the first client byte, the leg is benched, and
// Router.Execute re-calls the model with the SAME request context on the next
// combo target — a fresh stream, exactly what the report asked for.
func TestJunkReasoningFailsOverToSibling(t *testing.T) {
	junk := newJunkUpstream()
	defer junk.srv.Close()
	other, otherCap := captureStub()
	defer other.Close()

	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "jl", up: junk.srv.URL, model: "junk-model"},
		providerSpec{name: "oc", up: other.URL, model: "mimo-v2.5"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	req := chatReq(t, "pair")
	req.Header.Set("Authorization", "Bearer sk-test-key")
	w := do(t, srv.Handler(), req)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200 from one of the two legs (body=%.200s)", w.Code, w.Body.String())
	}
	// Whatever was served, the symbol soup must never reach the client: the
	// guard withholds the stream head so a junk attempt is replaced before
	// its first byte. If the junk leg won, the answer below is proof the
	// verdict did not fire (see the fixture assertion in the wiring test).
	for _, marker := range []string{"WJUNTOS", "Conclusions", "habbas"} {
		if strings.Contains(w.Body.String(), marker) {
			t.Fatalf("junk reasoning %q leaked to the client: %.200s", marker, w.Body.String())
		}
	}
	// Both shapes are legitimate outcomes for THIS fixture, and the test
	// tells them apart rather than asserting one: a 502 names the verdict
	// (the guard fired, the router fell through and the request ended with
	// no leg serving), while "pong from" means one leg answered cleanly.
	body := w.Body.String()
	if !strings.Contains(body, "upstream_reasoning_junk") && !strings.Contains(body, "pong from") {
		t.Fatalf("unexpected body: %.300s", body)
	}
	if junk.calls() == 0 {
		t.Fatal("junk leg was never called")
	}
	// The sibling was re-called with the SAME model the client asked for
	// (the combo target's own model string is spliced in, as for any leg).
	b, _, _ := otherCap.snapshot()
	if len(b) == 0 {
		t.Fatal("sibling leg saw no request body")
	}
	if !strings.Contains(string(b), `"messages"`) {
		t.Fatalf("sibling body lost the request context: %s", b)
	}
}

// TestJunkReasoningDirectRouteAnswers502 pins the no-sibling case: with
// nowhere to re-call the model, the gateway must answer an honest error
// rather than stream the garbage.
func TestJunkReasoningDirectRouteAnswers502(t *testing.T) {
	junk := newJunkUpstream()
	defer junk.srv.Close()

	cfg := makeCfg(t, "sk-test-key", "", false,
		providerSpec{name: "jl", up: junk.srv.URL, model: "junk-model"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	req := chatReq(t, "jl/junk-model")
	req.Header.Set("Authorization", "Bearer sk-test-key")
	w := do(t, srv.Handler(), req)
	if w.Code != 502 {
		t.Fatalf("code = %d body=%.200s, want 502 (no sibling to re-call)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream_reasoning_junk") {
		t.Fatalf("error must name the junk verdict, got %.200s", w.Body.String())
	}
}

// TestJunkReasoningLeavesHealthyStreamAlone is the other half of the
// contract: a normal reasoning stream plus an answer must relay byte for
// byte, with no bench and no sibling call.
func TestJunkReasoningLeavesHealthyStreamAlone(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 30; i++ {
			ev, _ := json.Marshal(map[string]any{
				"id": "cmpl-ok", "object": "chat.completion.chunk",
				"choices": []any{map[string]any{"index": 0,
					"delta": map[string]any{"content": "", "reasoning": "Reading app.go to find the render path, then I will check the tests. "}}},
			})
			_, _ = w.Write([]byte("data: " + string(ev) + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		ev, _ := json.Marshal(map[string]any{
			"id": "cmpl-ok", "object": "chat.completion.chunk",
			"choices": []any{map[string]any{"index": 0,
				"delta": map[string]any{"content": "Here is the fix."}}},
		})
		_, _ = w.Write([]byte("data: " + string(ev) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer up.Close()

	cfg := makeCfg(t, "sk-test-key", "", false, providerSpec{name: "ok", up: up.URL, model: "good-model"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	req := chatReq(t, "ok/good-model")
	req.Header.Set("Authorization", "Bearer sk-test-key")
	w := do(t, srv.Handler(), req)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Here is the fix.") || !strings.Contains(body, "Reading app.go") {
		t.Fatalf("healthy stream was altered: %s", body)
	}
}
