package provider

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"onegw/internal/translat"
)

func TestOpencodeSessionDerivation(t *testing.T) {
	// Client-supplied session (within the length cap) is forwarded verbatim.
	if got := opencodeSession("ses_client", "key-a"); got != "ses_client" {
		t.Fatalf("client session = %q, want passthrough", got)
	}
	// Derived fallback: stable per credential, opaque ses_<32 hex>.
	a := opencodeSession("", "key-a")
	if a != opencodeSession("", "key-a") {
		t.Fatal("derived session must be stable for the same key")
	}
	if b := opencodeSession("", "key-b"); a == b {
		t.Fatal("different keys must derive different sessions")
	}
	if !strings.HasPrefix(a, "ses_") || len(a) != len("ses_")+32 {
		t.Fatalf("derived session %q must be ses_<32 hex>", a)
	}
	// Whitespace-only or overlong client values fall back to derived.
	if got := opencodeSession("   ", "key-a"); got != a {
		t.Fatalf("whitespace client session = %q, want derived fallback", got)
	}
	if got := opencodeSession(strings.Repeat("x", maxOpenCodeSessionLen+1), "key-a"); got == strings.Repeat("x", maxOpenCodeSessionLen+1) {
		t.Fatal("overlong client session must be rejected, not forwarded")
	}
}

func TestDoOpencodeHeaders(t *testing.T) {
	var gotAuth, gotSession, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("X-Opencode-Session")
		gotPath = r.URL.Path
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	def := &Def{Name: "opencode", Kind: KindOpenCode, BaseURL: srv.URL, Accounts: []Account{{Name: "k1", APIKey: "oc-test"}}}
	res, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.2", "", bytes.NewReader([]byte(`{}`)), false)
	if apiErr != nil {
		t.Fatalf("Do failed: %+v", apiErr)
	}
	defer res.Resp.Body.Close()

	if gotAuth != "Bearer oc-test" {
		t.Fatalf("Authorization = %q, want bearer key", gotAuth)
	}
	if gotSession == "" || len(gotSession) != len("ses_")+32 || !strings.HasPrefix(gotSession, "ses_") {
		t.Fatalf("X-Opencode-Session = %q, want derived ses_<32 hex>", gotSession)
	}
	if want := "/v1/chat/completions"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
}

func TestDoOpencodeClientSessionForwarded(t *testing.T) {
	var gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-Opencode-Session")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	def := &Def{Name: "opencode", Kind: KindOpenCode, BaseURL: srv.URL, Accounts: []Account{{Name: "k1", APIKey: "k"}}}
	res, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.2", "ses_from-client", bytes.NewReader([]byte(`{}`)), false)
	if apiErr != nil {
		t.Fatalf("Do failed: %+v", apiErr)
	}
	defer res.Resp.Body.Close()
	if gotSession != "ses_from-client" {
		t.Fatalf("client session = %q, want forwarded verbatim", gotSession)
	}
}

func TestDefaultModelsOpenCode(t *testing.T) {
	m := DefaultModels(KindOpenCode)
	if len(m) != 35 {
		t.Fatalf("catalog = %d models, want the live 35", len(m))
	}
	// Responses-only families are advertised and routed to /v1/responses.
	for _, id := range []string{"grok-4.5", "grok-4.6", "gpt-5.6-luna", "muse-spark-1.3-contributor"} {
		if !slices.Contains(m, id) {
			t.Fatalf("catalog missing %s", id)
		}
	}
	if DefaultModels(KindOpenAI) != nil {
		t.Fatal("openai kind has no default catalog")
	}
}

func TestResponsesOnlyRouting(t *testing.T) {
	def := &Def{Name: "opencode", Kind: KindOpenCode}
	for model, wantFmt := range map[string]translat.Format{
		"grok-4.5":                   translat.FmtResponses,
		"grok-4.6":                   translat.FmtResponses,
		"gpt-5.6-luna":               translat.FmtResponses,
		"muse-spark-1.3-contributor": translat.FmtResponses,
		"mimo-v2.5":                  translat.FmtOpenAI,
		"deepseek-v4-flash":          translat.FmtOpenAI,
	} {
		if got := def.UpstreamFormat(model); got != wantFmt {
			t.Fatalf("UpstreamFormat(%s) = %s, want %s", model, got, wantFmt)
		}
	}
	if got := def.Path("chat", "grok-4.5"); got != "/v1/responses" {
		t.Fatalf("grok path = %s", got)
	}
	if got := def.Path("chat", "mimo-v2.5"); got != "/v1/chat/completions" {
		t.Fatalf("mimo path = %s", got)
	}
	// Other kinds never vary per model.
	oai := &Def{Name: "x", Kind: KindOpenAI}
	if oai.UpstreamFormat("grok-4.5") != translat.FmtOpenAI {
		t.Fatal("openai kind must not vary by model")
	}
}

// Grok calls must hit /v1/responses with the session header and bearer key.
func TestDoOpencodeResponsesPath(t *testing.T) {
	var gotPath, gotAuth, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("X-Opencode-Session")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	def := &Def{Name: "opencode", Kind: KindOpenCode, BaseURL: srv.URL, Accounts: []Account{{Name: "k1", APIKey: "oc"}}}
	res, apiErr := def.Do(t.Context(), &def.Accounts[0], "grok-4.6", "", bytes.NewReader([]byte(`{"model":"grok-4.6","input":[]}`)), false)
	if apiErr != nil {
		t.Fatalf("Do failed: %+v", apiErr)
	}
	defer res.Resp.Body.Close()
	if gotPath != "/v1/responses" {
		t.Fatalf("grok path = %q, want /v1/responses", gotPath)
	}
	if gotAuth != "Bearer oc" || !strings.HasPrefix(gotSession, "ses_") {
		t.Fatalf("auth=%q session=%q", gotAuth, gotSession)
	}
	if res.Format != translat.FmtResponses {
		t.Fatalf("CallResult.Format = %s, want responses", res.Format)
	}
}
