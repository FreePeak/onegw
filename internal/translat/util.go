package translat

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"

	"onegw/internal/types"
)

func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// randHex returns n hex chars from crypto/rand.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, n)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0xf])
	}
	return string(out[:n])
}

// statusFromOAErr maps an in-band upstream error object onto an HTTP
// status. one-api proxies (b-ai family) answer HTTP 200 and deliver the
// real failure — often a 429 rate limit — as an error object mid-stream
// or in a 200 body; reporting those as bare 502 both lied on the
// dashboard and starved the account-cooldown ladder of its input.
// Recognized codes map to their status; anything unrecognized stays 502.
func statusFromOAErr(code any, typ, msg string) int {
	switch c := code.(type) {
	case float64:
		if c >= 400 && c < 600 {
			return int(c)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(c)); err == nil && n >= 400 && n < 600 {
			return n
		}
	}
	probe := strings.ToLower(errCodeString(code) + " " + typ + " " + msg)
	switch {
	case strings.Contains(probe, "rate limit"), strings.Contains(probe, "rate_limit"),
		strings.Contains(probe, "too many requests"), strings.Contains(probe, "overloaded"),
		strings.Contains(probe, "insufficient_quota"):
		return 429
	}
	return 502
}

// UpstreamAuthVerifyFailed reports whether an upstream 401 is actually a
// TRANSIENT failure of the upstream's own credential-verification
// service, not an invalid gateway key. Resellers like b-ai (one-api)
// forward our bearer token to an internal auth/verify endpoint on every
// request; when that call fails (network blip, connection reset, 5xx),
// the proxy answers 401 with the transport error text, e.g.
// 鉴权服务请求失败: Post "http://.../v1/internal/auth/verify": read tcp ...: connection reset by peer.
// These self-heal on the next request and must not surface to clients as
// a terminal authentication failure; callers downgrade them to a
// retryable 502-class upstream fault so combos fall through.
func UpstreamAuthVerifyFailed(status int, typ, msg string) bool {
	if status != 401 {
		return false
	}
	probe := strings.ToLower(typ + " " + msg)
	// The proxied auth-service failure arrives as a Go http client error
	// quoted in the message (Chinese "auth service request failed:" or
	// the English mirror) pointing at an auth/verify URL.
	return strings.Contains(probe, "鉴权服务请求失败") ||
		strings.Contains(probe, "auth/verify")
}

// UpstreamParseRejected reports whether an upstream 400 is a distributor
// node's generic body-parse rejection rather than a genuine request
// fault. b-ai's one-api distributor fans each request to heterogeneous
// GLM backend nodes; live RCA (2026-09-09): 22 client-visible failures
// in ~40 h, every one answered by the same backend node (all 22 request
// ids share the c955d568 marker), bodies 228 KB-2.3 MB, while
// byte-identical replays of the same bodies served 200 through other
// nodes minutes later. A body onegw itself parsed and rewrote is
// well-formed JSON — the rejection is the node's, not the client's.
// Callers rewrite it to a retryable 502-class fault so Router.Execute
// retries the target (a fresh node may serve) and combos fall through
// instead of surfacing a lying invalid_request. Deliberately narrow:
// the one-api edge's exact "Invalid request body" phrasing plus its
// "(request id: …)" trailer; genuine schema 400s (type
// invalid_request_error, code 400001, named-parameter messages) keep
// failing fast.
func UpstreamParseRejected(status int, typ, msg string) bool {
	if status != 400 || typ != "api_error" {
		return false
	}
	return strings.Contains(msg, "Invalid request body") &&
		strings.Contains(msg, "request id:")
}

// NormalizeInStreamError applies the same transient-fault rewrites to an
// in-band error object (one-api proxies answer HTTP 200 and deliver the
// real failure as an error object mid-stream or in a 200 body) that
// provider.Do applies to HTTP-level error responses: transient upstream
// auth-verify outages rewrite to a retryable 502, and distributor-node
// parse rejections of valid bodies (peer RCA 84fd1c9: 22 client-visible
// failures in ~40 h) rewrite to upstream_parse_rejected. Without this,
// both shapes would surface terminally on streaming paths even though
// the HTTP-level path already downgrades them. Mutates and returns e for
// call-site convenience; nil-safe.
func NormalizeInStreamError(e *types.APIError) *types.APIError {
	if e == nil {
		return nil
	}
	if UpstreamAuthVerifyFailed(e.Status, e.Type, e.Message) {
		e.Status = 502
		e.Type = "upstream_auth_verify_failed"
	}
	if UpstreamParseRejected(e.Status, e.Type, e.Message) {
		e.Status = 502
		e.Type = "upstream_parse_rejected"
	}
	return e
}
