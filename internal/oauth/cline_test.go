package oauth

// Cline's sign-in dialect is the one place in this package where the vendor is
// not OAuth: the credential arrives pre-encoded in the redirect's `code`, the
// exchange and refresh grants are camelCase JSON at their own endpoints, the
// answer is wrapped in {success,data}, the bearer must carry the `workos:`
// prefix the upstream demands, and `state` is never echoed back (so the
// per-login token rides in the callback path). Every one of those is a silent
// 401/400 away from a login that "works" against the stub but not the vendor,
// so each is pinned against a captured shape rather than an invented one.
//
// Real shapes come from the live vendor, 2026-09-15/16.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func clineProfile(refreshURL string) Provider {
	p, ok := Lookup("cline")
	if !ok {
		panic("Lookup(\"cline\") missing")
	}
	if refreshURL != "" {
		p.ClineRefreshURL = refreshURL
	}
	return p
}

// TestClineProfileIsBrowserOnly pins the registry wiring: a cline/clinepass
// profile exists, opts into the browser flow, carries no device endpoint, and
// is advertised by Providers() so the dashboard lists it.
func TestClineProfileIsBrowserOnly(t *testing.T) {
	for _, name := range []string{"cline", "clinepass"} {
		p, ok := Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) not found", name)
		}
		if !p.ClineFlow || !p.BrowserFlow() {
			t.Errorf("%s: ClineFlow=%v BrowserFlow=%v, want both true", name, p.ClineFlow, p.BrowserFlow())
		}
		if p.DeviceCodeURL != "" {
			t.Errorf("%s: DeviceCodeURL=%q, want empty (cline has no device flow)", name, p.DeviceCodeURL)
		}
		if p.AuthURL != clineAuthorizeURL || p.TokenURL != clineTokenURL {
			t.Errorf("%s endpoints: %s / %s", name, p.AuthURL, p.TokenURL)
		}
	}
	var listed bool
	for _, n := range Providers() {
		if n == "cline" {
			listed = true
		}
	}
	if !listed {
		t.Errorf("Providers() = %v, missing cline", Providers())
	}

	// The CLI device path must refuse loudly instead of POSTing to "".
	mgr := NewManager(NewTokenStore("memory"))
	p, _ := Lookup("cline")
	if _, err := mgr.Login(context.Background(), AccountSpec{Key: "cline/me", Provider: p}, func(DeviceStart) {}); err == nil {
		t.Fatal("Login for a browser-only profile must error")
	} else if !strings.Contains(err.Error(), "browser flow") {
		t.Errorf("Login error = %q, want it to name the browser flow", err)
	}
}

// TestClineSessionDropsPKCEAndCarriesStateInThePath: the authorize URL must
// look like the vendor's own (client_type + callback_url + redirect_uri) and
// must not carry PKCE/client_id/scope — the page rejects what it does not
// recognize, and a leaked code_challenge would also tell the auditor we are
// not the real client. The single-use token has to be in the PATH, because the
// callback comes back with no `state` (measured).
func TestClineSessionDropsPKCEAndCarriesStateInThePath(t *testing.T) {
	p := clineProfile("")
	sess, err := newClineSession(p, "http://127.0.0.1:56121/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(sess.AuthURL)
	if err != nil {
		t.Fatalf("auth url: %v (%s)", err, sess.AuthURL)
	}
	q := u.Query()
	if q.Get("client_type") != "extension" {
		t.Errorf("client_type = %q", q.Get("client_type"))
	}
	for _, forbidden := range []string{"code_challenge", "code_challenge_method", "client_id", "scope", "response_type", "state"} {
		if q.Has(forbidden) {
			t.Errorf("authorize URL carries %s=%q; cline accepts neither PKCE nor state", forbidden, q.Get(forbidden))
		}
	}
	for _, param := range []string{"callback_url", "redirect_uri"} {
		if q.Get(param) != sess.RedirectURI {
			t.Errorf("%s = %q, want the session redirect %q", param, q.Get(param), sess.RedirectURI)
		}
	}
	if sess.RedirectURI != "http://127.0.0.1:56121/callback/"+sess.State {
		t.Errorf("redirect = %q, want the login token as the last path segment", sess.RedirectURI)
	}
	if sess.Verifier != "" {
		t.Errorf("verifier = %q, want none", sess.Verifier)
	}
	// Two logins must not collide.
	other, _ := newClineSession(p, "http://127.0.0.1:56121/callback")
	if other.State == sess.State {
		t.Error("two sessions share a state token")
	}
}

// TestClineExchangeReadsCredentialOutOfCode is the happy path: no HTTP at all.
// The captured redirect carried an UNPADDED base64 blob of JSON plus vendor
// trailer bytes after the closing brace, and the access token came back BARE
// (refresh wrapped) — so the prefix normalization is asserted here too.
func TestClineExchangeReadsCredentialOutOfCode(t *testing.T) {
	var touched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		touched = true
		w.WriteHeader(500)
	}))
	defer srv.Close()

	payload := `{"accessToken":"eyJhbGciOiwi-refresh","refreshToken":"EMJ7a7AjrjaKppYayUoCXpDZg","email":"me@example.com","expiresAt":"2026-09-15T16:16:30.118547421Z"}` + "\x1f\x8b trailer"
	code := strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(payload)), "=")

	p := clineProfile(srv.URL)
	tok, err := p.exchangeCline(context.Background(), srv.Client(), code, p.RedirectURI(0))
	if err != nil {
		t.Fatal(err)
	}
	if touched {
		t.Error("the blob path must not hit the network")
	}
	if tok.AccessToken != "workos:eyJhbGciOiwi-refresh" {
		t.Errorf("access = %q, want the workos: prefix baked in", tok.AccessToken)
	}
	if tok.RefreshToken != "EMJ7a7AjrjaKppYayUoCXpDZg" {
		t.Errorf("refresh = %q", tok.RefreshToken)
	}
	if want := time.Date(2026, 9, 15, 16, 16, 30, 118547421, time.UTC); !tok.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %v, want the vendor's signed %v", tok.ExpiresAt, want)
	}
}

// TestClineExchangeFallsBackToCamelCaseTokenPOST covers a pasted/real code:
// the vendor's validator rejected the Cline SDK's snake_case body with
// {"field":"granttype","tag":"required"} (measured), so the field names are the
// contract, and the answer arrives under "data".
func TestClineExchangeFallsBackToCamelCaseTokenPOST(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"accessToken":"jwt-2","refreshToken":"rt-2","expiresAt":"2026-09-15T18:00:00Z"}}`))
	}))
	defer srv.Close()

	p := clineProfile("")
	p.TokenURL = srv.URL + "/api/v1/auth/token"
	tok, err := p.exchangeCline(context.Background(), srv.Client(), "plain-code-not-a-blob", "http://127.0.0.1:56121/callback/abc")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/auth/token" {
		t.Errorf("posted to %s", path)
	}
	if got["grantType"] != "authorization_code" {
		t.Errorf("body = %v, want camelCase grantType (snake_case is what the vendor 400s)", got)
	}
	if _, ok := got["grant_type"]; ok {
		t.Errorf("body still carries snake_case grant_type: %v", got)
	}
	if got["code"] != "plain-code-not-a-blob" || got["redirectUri"] != "http://127.0.0.1:56121/callback/abc" {
		t.Errorf("body = %v", got)
	}
	if tok.AccessToken != "workos:jwt-2" || tok.RefreshToken != "rt-2" {
		t.Errorf("token = %+v", tok)
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("expiry lost from the wrapped response")
	}
}

// TestClineRefreshIsCamelCaseJSONAtItsOwnEndpoint: a plain OAuth form post to
// TokenURL returns 400 for this vendor, so a silent fallback there would look
// like "the token just expired faster". Rotation is kept when the vendor
// returns a new refresh token and the old one preserved when it does not —
// measured live: Cline rotates the ACCESS token and hands back the SAME refresh
// token, so an implementation that trusted the response would strand itself
// with an empty refresh grant.
func TestClineRefreshIsCamelCaseJSONAtItsOwnEndpoint(t *testing.T) {
	var bodies []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		paths = append(paths, r.URL.Path)
		resp := `{"success":true,"data":{"accessToken":"jwt-new"}}`
		if r.URL.Path == "/with-rotation" {
			resp = `{"success":true,"data":{"accessToken":"jwt-new","refreshToken":"rt-new","expiresAt":"2026-09-16T00:00:00Z"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	old := Token{AccessToken: "workos:jwt-old", RefreshToken: "rt-v1", ExpiresAt: time.Now().Add(time.Hour)}

	// No rotation in the response: keep the stored refresh token.
	p := clineProfile(srv.URL + "/no-rotation")
	tok, err := p.clineRefresh(context.Background(), srv.Client(), old)
	if err != nil {
		t.Fatal(err)
	}
	if paths[0] != "/no-rotation" {
		t.Errorf("refresh went to %s, want the profile's own endpoint", paths[0])
	}
	b := bodies[0]
	if b["grantType"] != "refresh_token" || b["refreshToken"] != "rt-v1" {
		t.Errorf("body = %v, want camelCase refreshToken + grantType", b)
	}
	if _, ok := b["grant_type"]; ok || b["client_id"] != nil {
		t.Errorf("body is OAuth-form-shaped, not cline's: %v", b)
	}
	if tok.AccessToken != "workos:jwt-new" || tok.RefreshToken != "rt-v1" {
		t.Errorf("token = %+v (refresh must survive an unrotated response)", tok)
	}
	if !tok.ExpiresAt.Equal(old.ExpiresAt) {
		t.Errorf("expiry = %v, want the stored one kept when the response omits it", tok.ExpiresAt)
	}

	// Rotation: the new refresh token wins.
	p2 := clineProfile(srv.URL + "/with-rotation")
	tok2, err := p2.clineRefresh(context.Background(), srv.Client(), old)
	if err != nil {
		t.Fatal(err)
	}
	if tok2.RefreshToken != "rt-new" || tok2.ExpiresAt.IsZero() {
		t.Errorf("rotated token = %+v", tok2)
	}
}

// TestManagerRefreshRoutesThroughTheClineDialect proves the seam in
// Manager.refresh: without it the manager posts an OAuth form to TokenURL, the
// vendor 400s, and the account cools on every tick with a token that only looks
// alive.
func TestManagerRefreshRoutesThroughTheClineDialect(t *testing.T) {
	var sawJSON, sawForm bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			sawJSON = true
		} else {
			sawForm = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"accessToken":"jwt-rotated","refreshToken":"rt-v2","expiresAt":"2026-09-16T06:00:00Z"}}`))
	}))
	defer srv.Close()

	store := NewTokenStore("memory")
	key := "cline/me"
	if err := store.Put(key, Token{AccessToken: "workos:jwt-old", RefreshToken: "rt-v1", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store)
	p := clineProfile(srv.URL)
	p.TokenURL = srv.URL + "/form-endpoint-should-not-be-used"
	// RefreshNow resolves the account through the synced specs, exactly as the
	// refresh loop does: a store entry alone is not a configured account.
	mgr.Sync([]AccountSpec{{Key: key, Provider: p}})
	t.Cleanup(mgr.Stop)
	if err := mgr.RefreshNow(context.Background(), key); err != nil {
		t.Fatalf("RefreshNow through the cline dialect: %v", err)
	}
	if !sawJSON || sawForm {
		t.Errorf("refresh transport: json=%v form=%v, want JSON only", sawJSON, sawForm)
	}
	got, ok := store.Get(key)
	if !ok || got.AccessToken != "workos:jwt-rotated" || got.RefreshToken != "rt-v2" {
		t.Errorf("stored token = %+v (ok=%v)", got, ok)
	}
}

// TestClineBearerIsIdempotent: the vendor hands back the bare form from
// authorize and the prefixed form from refresh; double-prefixing yields a
// bearer nothing accepts.
func TestClineBearerIsIdempotent(t *testing.T) {
	if got := clineBearer("workos:jwt"); got != "workos:jwt" {
		t.Errorf("double prefix: %q", got)
	}
	if got := clineBearer("jwt"); got != "workos:jwt" {
		t.Errorf("bare token: %q", got)
	}
}

// TestDecodeClineCredentialRejectsNonBlobs keeps the fallback reachable: a
// plain code, an empty one, and a blob with no JSON in it must all return nil
// rather than a half-parsed credential.
func TestDecodeClineCredentialRejectsNonBlobs(t *testing.T) {
	for _, s := range []string{"", "not-a-code", base64.StdEncoding.EncodeToString([]byte("no json here"))} {
		if cp := decodeClineCredential(s); cp != nil {
			t.Errorf("decodeClineCredential(%q) = %+v, want nil", s, cp)
		}
	}
}
