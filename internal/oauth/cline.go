package oauth

// Cline (api.cline.bot) — and its ClinePass plan, which is a plan inside the
// same account, not a separate IdP — signs in through an API that is
// OAuth-shaped at the front door and proprietary at the back:
//
//   - The authorize page takes `client_type` + `callback_url` and no
//     client_id, and there is NO PKCE: the blob that comes back in the
//     redirect's `code` is the signed credential itself, not a code to redeem
//     (9router src/lib/oauth/providers/cline.js:14-49 base64-decodes it with a
//     JSON-POST fallback; the vendor's own client agrees —
//     sdk/packages/core/src/auth/cline.ts exchangeAuthorizationCode).
//   - The vendor does not echo `state`: it returns to the exact callback_url it
//     was handed. Measured 2026-09-16: the redirect landed as
//     `http://127.0.0.1:56121/callback/<token>?code=…`, the query carrying
//     `code` only. So the single-use login token rides in the redirect PATH,
//     and handleOAuthCallback resolves a stateless callback from it.
//   - Refresh is a camelCase JSON POST at its own endpoint and the answer is
//     wrapped in `{"success":true,"data":{…}}` — the same envelope the chat API
//     uses, which is why provider.KindCline forces streaming.
//   - The resulting access token is accepted ONLY as `Bearer workos:<jwt>`: the
//     upstream answers the bare JWT with 401 "make sure you're using the latest
//     version of Cline and re-authenticate" (9router re-prefixes unconditionally
//     in open-sse/shared/clineAuth.js:5-15, and again after every refresh). The
//     prefix is therefore baked into the STORED token, so provider.applyAuth
//     stays vendor-agnostic — and a manually configured dashboard key (`sk_…`,
//     sent bare) is untouched by any of this.
//
// No device flow is implemented. The Cline SDK also speaks one (WorkOS
// user_management device authorize → authenticate → POST /api/v1/auth/register
// with that pair, sdk/packages/core/src/auth/cline.ts:275-420, and it is the
// SDK's default), which is what a headless host would need; 9router ships the
// browser flow only, so that is what is ported here. Manager.Login says so
// rather than POSTing to an empty device endpoint.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	clineAuthorizeURL = "https://api.cline.bot/api/v1/auth/authorize"
	clineTokenURL     = "https://api.cline.bot/api/v1/auth/token"
	clineRefreshURL   = "https://api.cline.bot/api/v1/auth/refresh"

	// clineBearerPrefix marks a WorkOS-issued session token. The vendor is
	// inconsistent about which form it hands back — bare inside the authorize
	// blob, prefixed out of refresh — so every path here normalizes on the way
	// in rather than trusting the shape of the last response.
	clineBearerPrefix = "workos:"

	// clineFallbackTTL is what 9router assumes when the payload carries no
	// expiry at all (src/lib/oauth/providers/cline.js:54-56). A cline session
	// JWT is signed for ~1 h; recording "no expiry known" instead would leave
	// the refresh loop asleep while every upstream call 401s.
	clineFallbackTTL = time.Hour
)

// clineRefreshEndpoint resolves the profile's refresh URL, falling back to the
// vendor's. It is a field rather than a constant so a test (or a self-hosted
// stand-in) can point the whole dialect at its own server.
func (p Provider) clineRefreshEndpoint() string {
	if p.ClineRefreshURL != "" {
		return p.ClineRefreshURL
	}
	return clineRefreshURL
}

// clineBearer normalizes an access token into the only form the upstream
// accepts for an account session: `workos:<jwt>`, which applyAuth sends as
// `Authorization: Bearer workos:<jwt>`.
func clineBearer(tok string) string {
	if strings.HasPrefix(tok, clineBearerPrefix) {
		return tok
	}
	return clineBearerPrefix + tok
}

// clinePayload is the credential in each of its shapes: the JSON inside the
// redirect's base64 `code`, and the body of the token/refresh endpoints (bare
// or nested under "data"). Names are the vendor's camelCase; expiresAt is
// usually an RFC 3339 string with sub-second precision
// ("2026-09-15T16:16:30.118547421Z", measured), and 9router also meets an
// epoch-ms number, so both are accepted.
type clinePayload struct {
	AccessToken  string          `json:"accessToken"`
	RefreshToken string          `json:"refreshToken"`
	TokenType    string          `json:"tokenType"`
	ExpiresAt    json.RawMessage `json:"expiresAt"`
	ExpiresIn    int             `json:"expiresIn"`
	UserInfo     struct {
		Email string `json:"email"`
	} `json:"userInfo"`
}

// token turns a decoded payload into a stored Token. fallbackRefresh keeps the
// previous refresh token when a response omits one (Cline rotates, but
// 9router's `tokens.refreshToken || refreshToken` guard exists for a reason).
func (cp clinePayload) token(fallbackRefresh string) (*Token, error) {
	if cp.AccessToken == "" {
		return nil, fmt.Errorf("cline auth response carried no accessToken")
	}
	tok := &Token{AccessToken: clineBearer(cp.AccessToken), RefreshToken: cp.RefreshToken}
	if tok.RefreshToken == "" {
		tok.RefreshToken = fallbackRefresh
	}
	tok.ExpiresAt = cp.expiry()
	return tok, nil
}

// expiry resolves whichever lifetime field the payload carried; zero means the
// vendor said nothing, and the caller decides (the fallback TTL for a fresh
// credential, the stored expiry for a refresh that kept one).
func (cp clinePayload) expiry() time.Time {
	s := strings.Trim(strings.TrimSpace(string(cp.ExpiresAt)), `"`)
	if s != "" && s != "null" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		if ms, err := strconv.ParseFloat(s, 64); err == nil && ms > 0 {
			return time.UnixMilli(int64(ms))
		}
	}
	if cp.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(cp.ExpiresIn) * time.Second)
	}
	return time.Time{}
}

// newClineSession builds the sign-in URL for a browser login. The PKCE builder
// is not used: cline has no client_id, no scope and no challenge, and its
// callback carries no `state`, so the login token goes into the path instead.
// redirectURI arrives as base+RedirectPath ("http://127.0.0.1:56121/callback").
func newClineSession(p Provider, redirectURI string) (PKCESession, error) {
	state, err := randomB64URL(16)
	if err != nil {
		return PKCESession{}, fmt.Errorf("cline state: %w", err)
	}
	if redirectURI == "" {
		redirectURI = p.RedirectURI(0)
	}
	redirectURI = strings.TrimSuffix(redirectURI, "/") + "/" + state
	q := url.Values{
		"client_type":  {"extension"},
		"callback_url": {redirectURI},
		"redirect_uri": {redirectURI},
	}
	return PKCESession{
		State:       state,
		RedirectURI: redirectURI,
		AuthURL:     p.AuthURL + "?" + q.Encode(),
	}, nil
}

// decodeClineCredential unwraps the `code` of a cline redirect: possibly
// UNPADDED base64 (either alphabet) of a JSON object with vendor trailer bytes
// after the final brace — both repairs are 9router's (cline.js:17-23). nil
// means "not a blob", and the caller falls back to the documented token POST.
func decodeClineCredential(code string) *clinePayload {
	if code == "" {
		return nil
	}
	padded := code + strings.Repeat("=", (4-len(code)%4)%4)
	var found *clinePayload
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		b, err := enc.DecodeString(padded)
		if err != nil {
			continue
		}
		i := bytes.LastIndexByte(b, '}')
		if i < 0 {
			continue
		}
		var cp clinePayload
		if json.Unmarshal(b[:i+1], &cp) == nil && cp.AccessToken != "" {
			found = &cp
			break
		}
	}
	return found
}

// exchangeCline finishes a browser login. Usually the credential is already in
// the redirect and no network call happens at all; only a real code (or a
// pasted callback URL) reaches the token POST.
func (p Provider) exchangeCline(ctx context.Context, hc *http.Client, code, redirectURI string) (*Token, error) {
	if code == "" {
		return nil, fmt.Errorf("cline sign-in: the callback carried no authorization code")
	}
	if cp := decodeClineCredential(code); cp != nil {
		tok, err := cp.token("")
		if err != nil {
			return nil, fmt.Errorf("cline sign-in: %w", err)
		}
		if tok.ExpiresAt.IsZero() {
			tok.ExpiresAt = time.Now().Add(clineFallbackTTL)
		}
		return tok, nil
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	// The vendor's own validator names the fields it wants: posting the Cline
	// SDK's snake_case body answers 400 with
	// {"data":[{"field":"code","tag":"required"},{"field":"granttype","tag":"required"}]}
	// (measured 2026-09-16) — i.e. camelCase `grantType`, not `grant_type`.
	body, err := json.Marshal(map[string]string{
		"code":        code,
		"grantType":   "authorization_code",
		"clientType":  "extension",
		"redirectUri": redirectURI,
	})
	if err != nil {
		return nil, err
	}
	status, resp, err := postJSONRaw(ctx, hc, p.TokenURL, body)
	if err != nil {
		return nil, fmt.Errorf("cline code exchange: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("cline code exchange: HTTP %d: %s", status, truncate(resp))
	}
	tok, err := clineTokenOf(resp, "")
	if err != nil {
		return nil, fmt.Errorf("cline code exchange: %w", err)
	}
	return tok, nil
}

// clineRefresh exchanges the stored refresh token. Nothing here matches the
// RFC 8628 form post every other profile in this package uses: camelCase JSON,
// its own endpoint, and the credential nested under "data".
func (p Provider) clineRefresh(ctx context.Context, hc *http.Client, old Token) (*Token, error) {
	body, err := json.Marshal(map[string]string{
		"refreshToken": old.RefreshToken,
		"grantType":    "refresh_token",
		"clientType":   "extension",
	})
	if err != nil {
		return nil, err
	}
	status, resp, err := postJSONRaw(ctx, hc, p.clineRefreshEndpoint(), body)
	if err != nil {
		return nil, fmt.Errorf("cline token refresh: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("cline token refresh: HTTP %d: %s", status, truncate(resp))
	}
	tok, err := clineTokenOf(resp, old.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("cline token refresh: %w", err)
	}
	if tok.ExpiresAt.IsZero() {
		tok.ExpiresAt = old.ExpiresAt
	}
	return tok, nil
}

// clineTokenOf parses either response shape: the payload bare, or wrapped in
// the {success,data} envelope the account API uses everywhere.
func clineTokenOf(body []byte, fallbackRefresh string) (*Token, error) {
	var cp clinePayload
	if err := json.Unmarshal(body, &cp); err == nil && cp.AccessToken != "" {
		return cp.token(fallbackRefresh)
	}
	var env struct {
		Success bool         `json:"success"`
		Data    clinePayload `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Data.AccessToken == "" {
		return nil, fmt.Errorf("no accessToken in response: %s", truncate(body))
	}
	return env.Data.token(fallbackRefresh)
}

// postJSONRaw POSTs a JSON body and returns (status, body) — the JSON twin of
// postFormRaw, which is every other profile's grant transport.
func postJSONRaw(ctx context.Context, hc *http.Client, endpoint string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}
