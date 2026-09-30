package oauth

// Codex (ChatGPT Plus/Pro) credential facts and CLI identity.
//
// Two things live here because BOTH the inference path and the quota probe
// need them and neither may drift from the other:
//
//   - The workspace id (`chatgpt-account-id`). ChatGPT access tokens are JWTs
//     whose namespaced `https://api.openai.com/auth` block carries it
//     (openai/codex codex-rs/login/src/token_data.rs:78 IdClaims). Decoded
//     from the bearer onegw actually sends — never stored, never invented, so
//     a token rotation cannot strand an account on a stale workspace.
//   - The CLI identity triple the backend feature-gates on. `Version` in
//     particular is load-bearing: a newer model 400s with "requires a newer
//     version of Codex" against a stale one.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const (
	// CodexClientVersion is the reported codex-cli version. It must track a
	// recent Codex CLI release; operators on a newer one override it per
	// provider with extra_headers.
	CodexClientVersion = "0.155.0"

	// CodexOriginator is the CLI's own originator marker
	// (openai/codex codex-rs/login/src/auth/default_client.rs
	// DEFAULT_ORIGINATOR).
	CodexOriginator = "codex_cli_rs"

	// CodexUserAgent is the CLI User-Agent. Without this shape the backend
	// treats the caller as an anonymous browser session.
	CodexUserAgent = "codex-cli/" + CodexClientVersion + " (Windows 10.0.26200; x64)"
)

// codexClaims decodes the ChatGPT identity claims out of a JWT. The claims
// live in the ID token, NOT the access token: the CLI's TokenData
// (login/src/token_data.rs) reads chatgpt_account_id / chatgpt_plan_type from
// id_token, and its get_account_id falls back to TokenData.account_id (a
// separate stored field) rather than to the access token. So onegw reads the
// id_token first, and only then the access token — some deployments and
// older CLI versions put the block in both.
//
// A JWT that is not a ChatGPT credential (an api key, a truncated store
// entry) simply has no block: "" is the honest answer and callers must not
// substitute a default, because a wrong workspace id is a 403 on every
// request.
func codexClaims(jwt string) (accountID, planType string) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return strings.TrimSpace(claims.Auth.AccountID), strings.TrimSpace(claims.Auth.PlanType)
}

// CodexAccountID returns the ChatGPT workspace id bound to this credential.
// tok is the stored token pair: the id_token is authoritative, the access
// token is the fallback for a store written before id_token was kept. "" when
// neither carries one.
func CodexAccountID(tok Token) string {
	if id, _ := codexClaims(tok.IDToken); id != "" {
		return id
	}
	id, _ := codexClaims(tok.AccessToken)
	return id
}

// CodexPlan returns the subscription plan label ("plus", "pro", "team", ...)
// carried by the same claims, or "" when the credential carries none. Used
// for the Quota page's plan cell; never guessed.
func CodexPlan(tok Token) string {
	if _, plan := codexClaims(tok.IDToken); plan != "" {
		return plan
	}
	_, plan := codexClaims(tok.AccessToken)
	return plan
}
