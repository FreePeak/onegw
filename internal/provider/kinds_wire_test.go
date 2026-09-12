package provider

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"onegw/internal/translat"
)

func TestNewKindsFormatAndDefaults(t *testing.T) {
	cases := []struct {
		kind   Kind
		format translat.Format
		base   string
		path   string
		forced bool
	}{
		{KindCommandCode, translat.FmtCommandCode, "https://api.commandcode.ai/alpha/generate", "", true},
		{KindOpenAIResponses, translat.FmtOpenAIResponses, "https://cli-chat-proxy.grok.com", "/v1/responses", true},
		{KindCursor, translat.FmtOpenAI, "https://api2.cursor.sh", "", true},
		{KindOpenAI, translat.FmtOpenAI, "https://api.openai.com", "/v1/chat/completions", false},
	}
	for _, c := range cases {
		if got := c.kind.Format(); got != c.format {
			t.Errorf("%s: Format()=%s want %s", c.kind, got, c.format)
		}
		if got := c.kind.DefaultBaseURL(); got != c.base {
			t.Errorf("%s: DefaultBaseURL()=%s want %s", c.kind, got, c.base)
		}
		d := &Def{Kind: c.kind}
		if got := d.Path("chat", ""); got != c.path {
			t.Errorf("%s: Path(chat)=%q want %q", c.kind, got, c.path)
		}
		if got := c.kind.ForcedStream(); got != c.forced {
			t.Errorf("%s: ForcedStream()=%v want %v", c.kind, got, c.forced)
		}
	}
}

// joinURL must produce the exact upstream endpoints for both base_url
// conventions (bare host and host with the version segment).
func TestNewKindEndpointJoining(t *testing.T) {
	cases := []struct {
		kind Kind
		base string
		want string
	}{
		{KindCommandCode, "https://api.commandcode.ai/alpha/generate", "https://api.commandcode.ai/alpha/generate"},
		{KindOpenAIResponses, "https://cli-chat-proxy.grok.com", "https://cli-chat-proxy.grok.com/v1/responses"},
		{KindOpenAIResponses, "https://cli-chat-proxy.grok.com/v1", "https://cli-chat-proxy.grok.com/v1/responses"},
	}
	for _, c := range cases {
		d := &Def{Kind: c.kind, BaseURL: c.base}
		got := joinURL(d.Base(nil), d.Path("chat", ""))
		if got != c.want {
			t.Errorf("%s base %s: got %s want %s", c.kind, c.base, got, c.want)
		}
	}
}

func TestNewRequestUUIDShape(t *testing.T) {
	a, b := newRequestUUID(), newRequestUUID()
	if a == b {
		t.Fatalf("uuids not unique: %s", a)
	}
	if len(a) != 36 || a[14] != '4' {
		t.Fatalf("not a v4 uuid: %q", a)
	}
}

// TestGrokCliFingerprintHeaders pins the Grok Build proxy contract: the
// credential-type marker it meters by (its own 401 body reports
// x_xai_token_auth=none when absent, so dropping it breaks every request),
// and the two endpoints — Responses for chat, /v1/models for discovery.
func TestGrokCliFingerprintHeaders(t *testing.T) {
	type call struct {
		path string
		hdr  http.Header
	}
	var mu sync.Mutex
	var seen []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, call{path: r.URL.Path, hdr: r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	d := &Def{Name: "grokbuild", Kind: KindOpenAIResponses, BaseURL: srv.URL,
		Accounts: []Account{{Name: "main", APIKey: "jwt-bearer-token"}}}

	if _, err := d.Do(t.Context(), &d.Accounts[0], "grok-build", http.Header{},
		bytes.NewReader([]byte(`{"model":"grok-build","input":"hi"}`)), false); err != nil {
		t.Fatalf("Do: %+v", err)
	}
	if _, _, err := d.FetchModels(t.Context(), &d.Accounts[0]); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	want := map[string]string{
		"X-Xai-Token-Auth":         "xai-grok-cli",
		"X-Grok-Client-Identifier": "xai-grok-cli",
		"X-Grok-Client-Version":    "0.2.99",
		"X-Grok-Cli-Version":       "0.2.97",
		"Authorization":            "Bearer jwt-bearer-token",
	}
	if len(seen) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(seen))
	}
	if seen[0].path != "/v1/responses" {
		t.Fatalf("chat path = %q, want /v1/responses", seen[0].path)
	}
	if seen[1].path != "/v1/models" {
		t.Fatalf("discovery path = %q, want /v1/models", seen[1].path)
	}
	for i, c := range seen {
		for k, v := range want {
			if got := c.hdr.Get(k); got != v {
				t.Fatalf("call %d header %s = %q, want %q", i, k, got, v)
			}
		}
	}
}
