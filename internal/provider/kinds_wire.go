package provider

// Per-kind wire helpers for the custom-format executors (commandcode,
// openai-responses). Kept out of provider.go to leave the shared plumbing
// file untouched beyond the dispatch cases.

import (
	"crypto/rand"
	"fmt"
	"net/http"
)

// ForcedStream reports whether the upstream only answers in streaming mode,
// regardless of what the client asked for (CommandCode /alpha/generate and
// Grok CLI /responses have no non-streaming shape). The server passes
// stream=true upstream for these kinds and aggregates the stream back into
// one completion for non-streaming clients.
func (k Kind) ForcedStream() bool {
	switch k {
	case KindCommandCode, KindOpenAIResponses, KindCursor:
		// Cursor (issue #12 follow-up): both services stream Connect-RPC
		// frames only; the synthetic OpenAI SSE body doCursor returns is
		// aggregated for non-streaming clients.
		return true
	default:
		return false
	}
}

// newRequestUUID returns a fresh RFC 4122 v4 UUID for per-request session /
// request id headers (x-session-id, x-grok-req-id, ...).
func newRequestUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// setGrokFingerprint applies the Grok Build proxy (cli-chat-proxy.grok.com)
// CLI fingerprint to an outbound request — the single owner of the header
// set for BOTH the chat POST and the /v1/models GET (its old byte-duplication
// across the two paths is how a wrong client-identifier value once shipped).
// Values mirror 9router's open-sse/config/grokCli.js + registry/grok-cli.js,
// which spreads them over every surface via BaseExecutor.buildHeaders. The
// User-Agent matters most: without it onegw sends Go's default, the single
// biggest non-CLI tell. X-XAI-Token-Auth and x-grok-cli-version are onegw
// extras 9router omits on chat — kept because the proxy reports
// "x_xai_token_auth=none" in its 401s when the credential-type marker is
// absent (live OmniRoute evidence); a working path beats capture fidelity.
// chat=true adds the per-attempt ids 9router's executors/grok-cli.js
// buildHeaders sets only on chat turns: a fresh x-grok-req-id UUID and
// x-grok-model-override (the CLI always sets it; onegw has no virtual
// effort-suffix catalog, so the routed model string IS the upstream model).
func setGrokFingerprint(h http.Header, model string, chat bool) {
	h.Set("User-Agent", "grok-shell/0.2.99 (linux; x86_64)")
	h.Set("x-grok-client-identifier", "grok-shell")
	h.Set("x-grok-client-version", "0.2.99")
	h.Set("x-grok-cli-version", "0.2.97")
	h.Set("X-XAI-Token-Auth", "xai-grok-cli")
	if chat {
		h.Set("x-grok-req-id", newRequestUUID())
		h.Set("x-grok-model-override", model)
	}
}
