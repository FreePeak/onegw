package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"onegw/internal/config"
)

// An OpenAI thinking model reports reasoning_tokens as a breakdown OF
// completion_tokens; the quota window must charge the vendor total
// (prompt + completion), not prompt + completion + reasoning — that
// double-charged every reasoning token and exhausted windows early.
func TestQuotaChargesOpenAIReasoningOnce(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-r", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 20}},
		})
	}))
	defer up.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{{Name: "oa", Kind: "openai", BaseURL: up.URL, APIKey: "sk-up",
		Models: []string{"m"}, QuotaWindow: "daily", QuotaLimitTokens: 1000}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	if w := do(t, srv.Handler(), authed(t, "oa/m", "sk-test-gw")); w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, ok := srv.cur().quota.Status("oa", time.Now())
		if ok && st.UsedTokens == 80 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota used tokens = %d, want 80 (reasoning is inside completion_tokens)", st.UsedTokens)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
