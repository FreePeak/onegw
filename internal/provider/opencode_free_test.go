package provider

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"onegw/internal/translat"
)

// The free tier advertises a curated keyless catalog (verified live
// 2026-09-12) and resolves its default base to the public zen/v1 endpoint.
func TestOpenCodeFreeDefaults(t *testing.T) {
	if got := KindOpenCodeFree.DefaultBaseURL(); got != "https://opencode.ai/zen/v1" {
		t.Fatalf("free base = %q, want https://opencode.ai/zen/v1", got)
	}
	models := DefaultModels(KindOpenCodeFree)
	if len(models) == 0 {
		t.Fatal("free kind must ship a curated catalog")
	}
	var hasBigPickle bool
	for _, m := range models {
		if m == "big-pickle" {
			hasBigPickle = true
		}
	}
	if !hasBigPickle {
		t.Fatalf("free catalog missing big-pickle: %v", models)
	}
	if DefaultModels(KindOpenCode) == nil {
		t.Fatal("paid Go catalog must stay")
	}
	// Per-model routing (advisor follow-up, live-probed 2026-09-12: the
	// muse-spark-*-free pair serves keyless on /zen/v1/responses, while
	// big-pickle lives on chat-completions).
	if got := (&Def{Kind: KindOpenCodeFree}).Path("chat", "big-pickle"); got != "/v1/chat/completions" {
		t.Fatalf("free chat path = %q", got)
	}
	if got := (&Def{Kind: KindOpenCodeFree}).Path("chat", "muse-spark-1.3-contributor-free"); got != "/v1/responses" {
		t.Fatalf("free muse-spark path = %q, want /v1/responses", got)
	}
	if got := (&Def{Kind: KindOpenCodeFree}).UpstreamFormat("muse-spark-1.2-contributor-free"); got != translat.FmtResponses {
		t.Fatalf("free muse-spark format = %v, want FmtResponses", got)
	}
	if got := (&Def{Kind: KindOpenCodeFree}).UpstreamFormat("big-pickle"); got != translat.FmtOpenAI {
		t.Fatalf("free big-pickle format = %v, want FmtOpenAI", got)
	}
}

// A keyless free Def sends NO credential and ALWAYS sends a session id:
// client value forwarded verbatim when present, else a stable ses_ id
// derived from the (keyless) account name. A bare "Authorization: Bearer "
// header is what the free tier treats as anonymous (verified live).
func TestDoOpenCodeFreeKeyless(t *testing.T) {
	var gotAuth, gotSession, gotPath, gotBodyModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("X-Opencode-Session")
		gotPath = r.URL.Path
		b := new(bytes.Buffer)
		io.Copy(b, r.Body)
		gotBodyModel = b.String()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"big-pickle","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	def := &Def{Name: "ocfree", Kind: KindOpenCodeFree, BaseURL: srv.URL}
	def.Accounts = []Account{{Name: "default"}} // keyless
	res, apiErr := def.Do(t.Context(), &def.Accounts[0], "big-pickle", nil, bytes.NewReader([]byte(`{"model":"big-pickle","messages":[]}`)), false)
	if apiErr != nil {
		t.Fatalf("Do failed: %+v", apiErr)
	}
	defer res.Resp.Body.Close()

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	// Go's transport trims the OWS after "Bearer " — the anonymous shape
	// the live free tier accepts (probe 2026-09-12: 200). What must NOT
	// appear is key material: the value is exactly the bare scheme.
	if gotAuth != "Bearer" {
		t.Fatalf("Authorization = %q, want the bare \"Bearer\" scheme (anonymous)", gotAuth)
	}
	if gotSession == "" || len(gotSession) < len("ses_") {
		t.Fatalf("session = %q, want derived ses_ id", gotSession)
	}
	if a := opencodeSession("", "default"); a != gotSession {
		t.Fatalf("session %q not the stable per-account derivation %q", gotSession, a)
	}
	// Client-supplied session header wins over the derived id.
	clientHdr := http.Header{}
	clientHdr.Set("X-Opencode-Session", "ses_client_id")
	res2, apiErr := def.Do(t.Context(), &def.Accounts[0], "big-pickle", clientHdr, bytes.NewReader([]byte(`{}`)), false)
	if apiErr != nil {
		t.Fatalf("Do(client session) failed: %+v", apiErr)
	}
	defer res2.Resp.Body.Close()
	if gotSession != "ses_client_id" {
		t.Fatalf("client session not forwarded: %q", gotSession)
	}
	if gotBodyModel == "" {
		t.Fatal("body never reached stub")
	}
}
