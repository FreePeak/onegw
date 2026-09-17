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

// A keyless free Def sends NO credential material and ALWAYS sends a session
// id in the ONE shape the upstream edge accepts, plus the CLI User-Agent that
// edge also requires: a client value already in that shape is forwarded
// verbatim, anything else falls back to the stable per-account derivation.
// Both halves are live-probed contracts (2026-09-17; see openCodeCLIUserAgent
// and openCodeCLISessionRe for the 403 matrix).
func TestDoOpenCodeFreeKeyless(t *testing.T) {
	var gotAuth, gotSession, gotPath, gotUA, gotBodyModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("X-Opencode-Session")
		gotUA = r.Header.Get("User-Agent")
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
	// Go's transport trims the OWS after "Bearer " — the anonymous shape
	// the live free tier accepts (probe 2026-09-17: 200). What must NOT
	// appear is key material: the value is exactly the bare scheme.
	if gotAuth != "Bearer" {
		t.Fatalf("Authorization = %q, want the bare \"Bearer\" scheme (anonymous)", gotAuth)
	}
	// The CLI User-Agent is half the gate: without it the edge 403s even a
	// perfectly shaped session id.
	if gotUA != openCodeCLIUserAgent {
		t.Fatalf("User-Agent = %q, want the CLI fingerprint %q", gotUA, openCodeCLIUserAgent)
	}
	if !openCodeCLISessionRe.MatchString(gotSession) {
		t.Fatalf("session %q is not in the shape the edge accepts (%s)", gotSession, openCodeCLISessionRe)
	}
	if a := openCodeFreeSession("", "default"); a != gotSession {
		t.Fatalf("session %q not the stable per-account derivation %q", gotSession, a)
	}
	// A client-supplied id is forwarded verbatim when it already has the
	// accepted shape...
	clientHdr := http.Header{}
	clientHdr.Set("X-Opencode-Session", "ses_0123456789abZz0123456789ab")
	res2, apiErr := def.Do(t.Context(), &def.Accounts[0], "big-pickle", clientHdr, bytes.NewReader([]byte(`{}`)), false)
	if apiErr != nil {
		t.Fatalf("Do(client session) failed: %+v", apiErr)
	}
	defer res2.Resp.Body.Close()
	if gotSession != "ses_0123456789abZz0123456789ab" {
		t.Fatalf("client session not forwarded: %q", gotSession)
	}
	// ...and replaced by the derived one when it does NOT: forwarding an
	// off-shape id would 403 the request the client could otherwise serve.
	clientHdr.Set("X-Opencode-Session", "ses_client_id")
	res3, apiErr := def.Do(t.Context(), &def.Accounts[0], "big-pickle", clientHdr, bytes.NewReader([]byte(`{}`)), false)
	if apiErr != nil {
		t.Fatalf("Do(off-shape client session) failed: %+v", apiErr)
	}
	defer res3.Resp.Body.Close()
	if gotSession != openCodeFreeSession("", "default") {
		t.Fatalf("off-shape client session survived: %q", gotSession)
	}
	if gotBodyModel == "" {
		t.Fatal("body never reached stub")
	}
}

// The derived session is stable per account, differs across accounts, and
// always lands in the accepted shape.
func TestOpenCodeFreeSession(t *testing.T) {
	a := openCodeFreeSession("", "default")
	if a != openCodeFreeSession("", "default") {
		t.Fatalf("derivation not stable: %q", a)
	}
	if b := openCodeFreeSession("", "second"); a == b {
		t.Fatalf("distinct accounts share a session: %q", a)
	}
	for _, s := range []string{a, openCodeFreeSession("", "second")} {
		if !openCodeCLISessionRe.MatchString(s) {
			t.Fatalf("derived session %q not in the accepted shape", s)
		}
	}
	if got := openCodeFreeSession("  ses_0123456789abZz0123456789ab  ", "default"); got != "ses_0123456789abZz0123456789ab" {
		t.Fatalf("padded client session not trimmed/forwarded: %q", got)
	}
}
