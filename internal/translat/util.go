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
		if n, err := strconv.Atoi(strings.TrimSpace(c)); err ***REMOVED*** nil && n >= 400 && n < 600 {
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
// parse rejections of valid bodies (peer RCA b38eab4: 22 client-visible
// failures in ~40 h) rewrite to upstream_parse_rejected. Without this,
// both shapes would surface terminally on streaming paths even though
// the HTTP-level path already downgrades them. Mutates and returns e for
// call-site convenience; nil-safe.
func NormalizeInStreamError(e *types.APIError) *types.APIError {
	if e ***REMOVED*** nil {
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

// ---------------------------------------------------------------------------
// Tool-call / tool-result pairing
// ---------------------------------------------------------------------------

// toolResultMissing answers an assistant tool call whose result never arrived:
// the client interrupted the tool, or its own history rewrite kept the
// tool_result turn and dropped the tool_use turn. Strict tool-calling upstreams
// reject an unanswered tool_calls turn outright, and because the offending turn
// already sits in the client's history every retry 400s too — so the gap has to
// be closed here rather than surfaced.
const toolResultMissing = "[no tool result recorded]"

// NormalizeToolPairs rewrites the unified message list into the shape every
// tool-calling wire demands: one contiguous run of tool results immediately
// after the assistant turn that made the calls, that turn's other content after
// the run, and exactly one result per call id.
//
// Client histories do not arrive in that shape. Anthropic lets a user turn mix
// tool_result blocks with free text — Claude Code appends its
// <system-reminder> text after the results — and after a mid-turn interjection
// it answers the calls of a single assistant turn across SEPARATE user turns,
// text in between. Translated verbatim the body reads
// assistant(tool_calls) -> user(text) -> tool(result), which every strict
// validator rejects, from both directions: scanning forward from the assistant
// finds too few replies ("insufficient tool messages following tool_calls
// message", opencode "Console Go") and scanning back from the reply finds the
// wrong predecessor ("`messages[N]` tool message must follow an assistant
// message", z.ai). Both live 2026-09-14 from one Claude Code session, whose
// history kept failing at index 47 and index 161.
//
// The repair only moves whole turns: results keep arrival order, deferred turns
// keep theirs, and parts keep their cache breakpoints, so a history already in
// shape comes back byte-identical (needsPairRepair makes that a no-op with no
// reallocation — a rewritten prefix would cost the provider's prompt cache).
// Nothing is dropped or reworded; a result without an id (Gemini names its
// responses instead, legacy OpenAI "function" role names its caller) is matched
// to the oldest open call positionally.
func NormalizeToolPairs(u *types.ChatRequest) {
	if !needsPairRepair(u.Messages) {
		return
	}
	out := make([]types.Message, 0, len(u.Messages)+2)
	var (
		open    bool            // an assistant turn awaits its results
		pending []string        // its unanswered call ids, in call order
		results []types.Part    // the run being assembled, in arrival order
		hold    []types.Message // turns deferred behind the run
		name    string
	)
	flush := func() {
		if !open {
			return
		}
		for _, id := range pending {
			results = append(results, types.Part{Type: types.PartToolResult, ToolUseID: id, Text: toolResultMissing})
		}
		if len(results) > 0 {
			out = append(out, types.Message{Role: types.RoleUser, Name: name, Content: results})
		}
		out = append(out, hold...)
		open, pending, results, hold, name = false, nil, nil, nil, ""
	}
	for _, m := range u.Messages {
		if m.Role ***REMOVED*** types.RoleAssistant {
			flush()
			out = append(out, m)
			for _, p := range m.Content {
				// Only a real id is trackable: an id-less tool_use block
				// (a malformed client) takes each wire's own fallback at
				// encode time, and answering it here would mean inventing an
				// id the call never had.
				if p.Type ***REMOVED*** types.PartToolUse && p.ID != "" {
					pending = append(pending, p.ID)
				}
			}
			open = len(pending) > 0
			continue
		}
		if !open {
			// Nothing awaits this turn: leave it as the client wrote it (the
			// encoders still order a mixed turn's own parts correctly).
			out = append(out, m)
			continue
		}
		res, rest := splitToolResults(m)
		for _, p := range res {
			if name ***REMOVED*** "" {
				name = m.Name
			}
			results = append(results, pairResultID(p, &pending))
		}
		if rest != nil {
			hold = append(hold, *rest)
		}
	}
	flush()
	u.Messages = out
}

// needsPairRepair reports whether any turn carries tool traffic at all. Plain
// chat (the overwhelming majority of requests) skips the rewrite entirely.
func needsPairRepair(msgs []types.Message) bool {
	for i := range msgs {
		m := &msgs[i]
		if m.Role ***REMOVED*** types.RoleTool {
			return true
		}
		for _, p := range m.Content {
			if p.Type ***REMOVED*** types.PartToolUse || p.Type ***REMOVED*** types.PartToolResult {
				return true
			}
		}
	}
	return false
}

// splitToolResults lifts the tool results out of a turn. results carries every
// tool_result part with its message-level id fallback resolved (so what reaches
// the wire is explicit), and rest is the same turn without them — nil when
// nothing but the results was there, which is the ordinary case. A unified
// tool-role message (hand-built requests, not decoder output) flattens to one
// result part and no rest.
func splitToolResults(m types.Message) (results []types.Part, rest *types.Message) {
	if m.Role ***REMOVED*** types.RoleTool {
		return []types.Part{{
			Type:      types.PartToolResult,
			Name:      orDefault(m.Name, m.ToolCallID),
			ToolUseID: orDefault(m.ToolCallID, m.Name),
			Text:      m.FlattenText(),
		}}, nil
	}
	keep := make([]types.Part, 0, len(m.Content))
	for _, p := range m.Content {
		if p.Type != types.PartToolResult {
			keep = append(keep, p)
			continue
		}
		if p.ToolUseID ***REMOVED*** "" {
			p.ToolUseID = orDefault(m.ToolCallID, m.Name)
		}
		results = append(results, p)
	}
	if len(results) ***REMOVED*** 0 {
		return nil, &m
	}
	if len(keep) ***REMOVED*** 0 {
		return results, nil
	}
	m.Content = keep
	return results, &m
}

// pairResultID attaches a result to a call the assistant really made: an id
// still awaiting its answer wins, otherwise the oldest open call takes it
// positionally. A surplus result (every call already answered, extra block in
// the same turn) keeps its own id and still travels with the run.
func pairResultID(p types.Part, pending *[]string) types.Part {
	for i, id := range *pending {
		if p.ToolUseID != "" && p.ToolUseID != id {
			continue
		}
		if p.ToolUseID ***REMOVED*** "" {
			p.ToolUseID = id
		}
		*pending = append((*pending)[:i], (*pending)[i+1:]...)
		return p
	}
	return p
}
