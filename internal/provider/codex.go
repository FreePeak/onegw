package provider

// Codex — a ChatGPT Plus/Pro subscription served through the Codex CLI's own
// backend (chatgpt.com/backend-api/codex).
//
// Three things about this wire are NOT the plain Responses dialect:
//
//  1. Upstream ALWAYS streams. The server passes stream=true upstream for this
//     kind (ForcedStream) and aggregates the SSE back into one completion.
//  2. The backend gates on the CLI identity triple — User-Agent, originator
//     and `Version` — so the version is load-bearing: a newer model 400s with
//     "requires a newer version of Codex" against a stale one. An operator on
//     a newer CLI overrides it with extra_headers.
//  3. The workspace id (`chatgpt-account-id`) is decoded off the bearer the
//     same way the CLI reads it out of its id_token, so no id is ever invented
//     and a token rotation cannot strand the account on a stale workspace.
//
// The request/response body is the EXISTING FmtOpenAIResponses dialect
// (translat.EncodeGrokCliRequest + the shared Responses SSE decoder), so there
// is no custom executor here — only the fingerprint, the path, and the curated
// catalog. Reasoning effort rides the client's own reasoning_effort knob; the
// catalog therefore advertises bare model ids. Ported from OmniRoute's codex
// provider (open-sse/executors/codex.ts buildHeaders, config/codexClient.ts,
// config/providers/registry/codex/index.ts).

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"onegw/internal/oauth"
)

const (
	// codexDefaultBase is the host serving the Codex backend-api.
	codexDefaultBase = "https://chatgpt.com"

	// codexResponsesPath is the inference endpoint the CLI posts to.
	codexResponsesPath = "/backend-api/codex/responses"
)

// codexSessionRe is the shape the backend accepts in `session_id`
// (OmniRoute's normalizeCodexSessionId). An off-shape client value is not
// forwarded: it would 403 the whole request.
var codexSessionRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

// codexSession returns the prompt-cache session id for one call. The client's
// own id wins (per-conversation affinity, issue #36); otherwise a stable
// per-ACCOUNT id is derived, never a per-request random one — the backend
// partitions its prompt cache by session, so a rotating id would silently
// destroy the hit rate on every turn. Derived from the workspace id, so it
// survives a token refresh.
func codexSession(clientVal, accountID string) string {
	if s := strings.TrimSpace(clientVal); codexSessionRe.MatchString(s) {
		return s
	}
	sum := sha256.Sum256([]byte("onegw-codex-session\x00" + accountID))
	return "ses_" + hex.EncodeToString(sum[:16])
}

// setCodexFingerprint applies the CLI identity headers for one outbound call
// — the single owner for the chat POST. chatgpt-account-id and session_id are
// added only when the bearer actually carries a ChatGPT workspace: a
// non-ChatGPT key gets the CLI identity and no invented workspace.
func setCodexFingerprint(h http.Header, bearer, clientSession string) {
	h.Set("User-Agent", oauth.CodexUserAgent)
	h.Set("Version", oauth.CodexClientVersion)
	h.Set("originator", oauth.CodexOriginator)
	if id := oauth.CodexAccountID(bearer); id != "" {
		h.Set("chatgpt-account-id", id)
		h.Set("session_id", codexSession(clientSession, id))
	}
}

// codexModels is the served catalog: BARE ids (the server qualifies them with
// the provider name) and NO reasoning suffix — effort rides the client's
// reasoning_effort knob, so `gpt-5.6-sol-high` would be sent upstream verbatim
// and 400. Sourced from OmniRoute's registry
// (config/providers/registry/codex/index.ts) minus the suffixed aliases.
// ChatGPT rotates these without notice, so an operator who needs an id this
// list lacks sets `models` explicitly.
var codexModels = []string{
	"gpt-5.5",
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
	"gpt-6-astra", "gpt-6-sol", "gpt-6-luna",
}
