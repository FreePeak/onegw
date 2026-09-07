package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	res, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.2", "", []byte(`{}`), false)
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
	res, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.2", "ses_from-client", []byte(`{}`), false)
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
	if len(m) == 0 {
		t.Fatal("opencode kind must advertise the Go catalog by default")
	}
	for _, id := range m {
		if strings.HasPrefix(id, "muse-spark") {
			t.Fatalf("muse-spark is responses-only and must not be in the chat catalog: %s", id)
		}
	}
	if DefaultModels(KindOpenAI) != nil {
		t.Fatal("openai kind has no default catalog")
	}
}
