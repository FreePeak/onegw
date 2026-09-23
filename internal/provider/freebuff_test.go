package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFreebuffAgentID(t *testing.T) {
	if got := freebuffAgentID("deepseek/deepseek-v4-flash"); got != "base2-free-deepseek-flash" {
		t.Fatalf("agent = %q", got)
	}
	if got := freebuffAgentID("freebuff/openai/gpt-5.6-luna"); got != "base2-free-luna" {
		t.Fatalf("prefixed agent = %q", got)
	}
	if got := freebuffAgentID("unknown/model"); got != freebuffDefaultAgent {
		t.Fatalf("fallback agent = %q", got)
	}
}

func TestFreebuffPrepareBodyInjectsBuffyAndMetadata(t *testing.T) {
	raw := []byte(`{"model":"ignored","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`)
	out, apiErr := freebuffPrepareBody(raw, "deepseek/deepseek-v4-flash", "run-1", "inst-9", true)
	if apiErr != nil {
		t.Fatalf("prepare: %+v", apiErr)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["stream"] != true {
		t.Fatalf("stream = %v", payload["stream"])
	}
	msgs := payload["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.HasPrefix(first["content"].(string), "You are Buffy") {
		t.Fatalf("first message = %+v", first)
	}
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("user message lost: %+v", msgs)
	}
	meta := payload["codebuff_metadata"].(map[string]any)
	if meta["run_id"] != "run-1" || meta["freebuff_instance_id"] != "inst-9" || meta["cost_mode"] != "free" {
		t.Fatalf("metadata = %+v", meta)
	}
	if cid, _ := meta["client_id"].(string); len(cid) != 13 {
		t.Fatalf("client_id = %q", cid)
	}
	// Pre-existing Buffy prompt must not be duplicated.
	raw2 := []byte(`{"messages":[{"role":"system","content":"You are Buffy, already here."},{"role":"user","content":"x"}]}`)
	out2, apiErr := freebuffPrepareBody(raw2, "m", "", "", false)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	var p2 map[string]any
	_ = json.Unmarshal(out2, &p2)
	if len(p2["messages"].([]any)) != 2 {
		t.Fatalf("duplicated Buffy prompt: %+v", p2["messages"])
	}
	if p2["stream"] != false {
		t.Fatalf("stream false = %v", p2["stream"])
	}
}

func TestDoFreebuffEndToEnd(t *testing.T) {
	var sessionHits, startHits, finishHits, chatHits atomic.Int32
	var chatAuth, chatUA, chatInst, chatRun, chatAgent, sessionModel, sessionUA string
	var chatBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/freebuff/session"):
			sessionHits.Add(1)
			sessionModel = r.Header.Get("x-freebuff-model")
			sessionUA = r.Header.Get("User-Agent")
			if r.Header.Get("Authorization") != "Bearer tok-fb" {
				t.Errorf("session auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"instanceId":"inst-abc","status":"active"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-runs"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if body["action"] == "START" {
				startHits.Add(1)
				if body["agentId"] != "base2-free-deepseek-flash" {
					t.Errorf("agentId = %v", body["agentId"])
				}
				_, _ = w.Write([]byte(`{"runId":"run-xyz"}`))
				return
			}
			if body["action"] == "FINISH" {
				finishHits.Add(1)
				w.WriteHeader(http.StatusOK)
				return
			}
			t.Errorf("unexpected agent-runs body: %s", raw)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chatHits.Add(1)
			chatAuth = r.Header.Get("Authorization")
			chatUA = r.Header.Get("User-Agent")
			chatInst = r.Header.Get("x-freebuff-instance-id")
			chatRun = r.Header.Get("x-codebuff-run-id")
			chatAgent = r.Header.Get("x-codebuff-agent-id")
			chatBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := &Def{
		Name:    "freebuff",
		Kind:    KindFreebuff,
		BaseURL: srv.URL,
	}
	acct := &Account{Name: "me", APIKey: "tok-fb"}
	body := bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"ping"}]}`))
	cr, apiErr := d.doFreebuff(context.Background(), acct, "deepseek/deepseek-v4-flash", body, true)
	if apiErr != nil {
		t.Fatalf("doFreebuff: %+v", apiErr)
	}
	defer cr.Resp.Body.Close()
	if cr.Format != "openai" {
		t.Fatalf("format = %q", cr.Format)
	}
	if sessionHits.Load() != 1 || startHits.Load() != 1 || chatHits.Load() != 1 {
		t.Fatalf("hits session=%d start=%d chat=%d", sessionHits.Load(), startHits.Load(), chatHits.Load())
	}
	if sessionModel != "deepseek/deepseek-v4-flash" || !strings.HasPrefix(sessionUA, "codebuff/") {
		t.Fatalf("session headers model=%q ua=%q", sessionModel, sessionUA)
	}
	if chatAuth != "Bearer tok-fb" || chatUA != freebuffChatUA {
		t.Fatalf("chat auth/ua = %q %q", chatAuth, chatUA)
	}
	if chatInst != "inst-abc" || chatRun != "run-xyz" || chatAgent != "base2-free-deepseek-flash" {
		t.Fatalf("chat headers inst=%q run=%q agent=%q", chatInst, chatRun, chatAgent)
	}
	var payload map[string]any
	if err := json.Unmarshal(chatBody, &payload); err != nil {
		t.Fatal(err)
	}
	meta := payload["codebuff_metadata"].(map[string]any)
	if meta["freebuff_instance_id"] != "inst-abc" || meta["run_id"] != "run-xyz" {
		t.Fatalf("chat body metadata = %+v", meta)
	}
	// FINISH is async — wait briefly.
	deadline := time.Now().Add(2 * time.Second)
	for finishHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if finishHits.Load() == 0 {
		t.Fatal("FINISH agent-run never called")
	}
}

func TestDoFreebuffSessionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/freebuff/session") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		t.Errorf("should stop after session: %s", r.URL.Path)
	}))
	defer srv.Close()
	d := &Def{Name: "freebuff", Kind: KindFreebuff, BaseURL: srv.URL}
	_, apiErr := d.doFreebuff(context.Background(), &Account{APIKey: "bad"}, "deepseek/deepseek-v4-flash",
		bytes.NewReader([]byte(`{"messages":[]}`)), false)
	if apiErr == nil || apiErr.Status != 401 {
		t.Fatalf("want 401 session error, got %+v", apiErr)
	}
}

func TestKindFreebuff_Defaults(t *testing.T) {
	if KindFreebuff.DefaultBaseURL() != freebuffDefaultBase {
		t.Fatalf("base = %q", KindFreebuff.DefaultBaseURL())
	}
	if got := DefaultModels(KindFreebuff); len(got) != len(freebuffModels) {
		t.Fatalf("models = %v", got)
	}
	if KindFreebuff.Format() != "openai" {
		t.Fatalf("format = %q", KindFreebuff.Format())
	}
}
