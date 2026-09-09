package provider

// Cursor transport (issue #12 follow-up): both Cursor services are
// HTTP/2-only Connect-RPC endpoints, and the AgentService is FULL-DUPLEX —
// live evidence (2026-09-09, three independent probes):
//   1. Held-open stream + reply written only after the context question:
//      turn completes, text + usage delivered (the working 9router flow).
//   2. Pre-answered constant reply + request half-close: upstream NEVER
//      proceeds — repeated agent_error heartbeats, then 45s timeout. The
//      reply must FOLLOW the question on the stream.
//   3. The checksum/headers are the same in both; only the handshake
//      ordering differs. (JS shift masking is inherent to the reference
//      checksum — the Go port emulates it via translat.jsShift, pinned by
//      a node-computed vector.)
//
// Go's net/http CAN duplex an h2 request: an io.Pipe-backed Request.Body
// streams DATA frames while the response flows; the client returns on
// response HEADERS. So cursorUpstream runs: pipe body → write run_request
// → read response frames → on exec_server_request(10) write the constant
// reply into the pipe → forward every payload downstream → on stream end
// close the pipe (END_STREAM). ChatService stays a one-shot POST (9router
// makeHttp2Request shape).
//
// Route selection (cursorUsesChatService): tool schemas or "-thinking"
// model ids → ChatService (tools path); everything else → AgentService
// (text path; system content folded into the user turn by
// EncodeCursorAgentRequest — a non-empty system prompt in the run request
// kills the turn upstream, live-verified 3/3 probes).
//
// Response shape: translat.CursorSSEStream decodes frames and re-emits a
// synthetic OpenAI chat.completion.chunk SSE stream (the SearXNG pattern),
// so the whole existing pipeline serves cursor unchanged — same-format
// passthrough, cross-format translation, usage accounting, and combo
// fall-through (all cursor failures are retryable 502/429-class; empty
// streams surface as retryable 502 "cursor: empty response").

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"

	"onegw/internal/translat"
	"onegw/internal/types"
)

// cursorTLSOverride is a test hook: when non-nil, cursorUpstream dials with
// this TLS config (the httptest h2 mock's self-signed cert). Production
// leaves it nil — real cursor upstreams use public CAs.
var cursorTLSOverride *tls.Config

// cursorUsesChatService reports whether this request must ride the
// ChatService (tools) path: any OpenAI tool schema, or a "-thinking" model
// id (thinking-bearing ids route to the tool-capable service, matching
// 9router's registry behavior).
func cursorUsesChatService(model string, body []byte) bool {
	if strings.Contains(model, "-thinking") {
		return true
	}
	return bytes.Contains(body, []byte(`"tools"`)) && bytes.Contains(body, []byte(`"function"`))
}

// doCursor performs one Cursor upstream call. The returned CallResult body
// is ALWAYS a synthetic OpenAI SSE stream (Format FmtOpenAI): the server's
// ForcedStream path aggregates it for non-streaming clients and
// TranslateStream/passthrough serve streaming ones.
func (d *Def) doCursor(ctx context.Context, acct *Account, model string, body io.Reader) (*CallResult, *types.APIError) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: err.Error()}
	}
	token := acct.bearerToken()
	if token == "" {
		return nil, &types.APIError{Status: 500, Type: "internal", Message: "cursor: account has no credential"}
	}
	u, perr := translat.DecodeOpenAIRequest(raw)
	if perr != nil {
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: "cursor: " + perr.Error()}
	}
	u.Model = model

	var reqBody []byte
	var url string
	var agent bool
	if cursorUsesChatService(model, raw) {
		reqBody, err = translat.EncodeCursorChatRequest(u)
		url = joinURL(d.Base(acct), translat.CursorChatPath)
	} else {
		reqBody, err = translat.EncodeCursorAgentRequest(u)
		agentHost := translat.CursorAgentEndpointHost
		if base := d.Base(acct); base != translat.CursorChatEndpointHost {
			// Operator base_url override applies to BOTH services (tests,
			// mitm proxies); the kind default keeps the split endpoints.
			agentHost = base
		}
		url = agentHost + translat.CursorAgentRunPath
		agent = true
	}
	if err != nil {
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: err.Error()}
	}

	frames, closer, apiErr := d.cursorUpstream(ctx, url, token, reqBody, agent)
	if apiErr != nil {
		if apiErr.OverQuota() && acct != nil {
			// Cursor 429 (resource_exhausted): bench on the adaptive ladder
			// exactly like a generic upstream 429 — no Retry-After hint.
			d.pool.rateLimited(acct, 0)
		}
		return nil, apiErr
	}
	d.pool.ok(acct) // success resets the 429 ladder
	// cancel tears the upstream down at the agent done frame (cursor never
	// EOFs the response — see CursorSSEStream doc).
	var cancel func()
	if closer != nil {
		cancel = func() { _ = closer.Close() }
	}
	sse := translat.CursorSSEStream(frames, model, agent, cancel)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(sse),
	}
	return &CallResult{Resp: resp, Format: translat.FmtOpenAI, Acct: acct}, nil
}

// cursorUpstream opens the HTTP/2 call and returns a reader over decoded
// Connect-RPC payload frames (re-framed for CursorSSEStream).
//
//   - agent: full-duplex (io.Pipe request body; the context handshake reply
//     is written into the pipe exactly when the question arrives — see the
//     file header for the live evidence that pre-answering fails).
//   - chat: one-shot POST, response streams back (9router shape).
func (d *Def) cursorUpstream(ctx context.Context, url, token string, reqBody []byte, agent bool) (io.Reader, io.Closer, *types.APIError) {
	var bodyReader io.Reader
	var doneWrite func()
	var replies chan []byte
	if agent {
		pr, pw := io.Pipe()
		bodyReader = pr
		// ONE replies channel bridges the response pump to the request
		// writer: the pump feeds the handshake reply the moment the exec
		// question arrives; closing it ends the run (END_STREAM).
		replies = make(chan []byte)
		doneWrite = func() { close(replies) }
		go func() {
			defer pw.Close()
			if _, err := pw.Write(reqBody); err != nil {
				return
			}
			for f := range replies {
				if _, err := pw.Write(f); err != nil {
					return
				}
			}
		}()
	} else {
		bodyReader = bytes.NewReader(reqBody)
		doneWrite = func() {}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
	if err != nil {
		doneWrite()
		return nil, nil, &types.APIError{Status: 502, Type: "upstream_error", Message: "cursor: " + err.Error()}
	}
	if agent {
		req.ContentLength = -1 // unknown length: h2 streams DATA until EOF
	}
	for _, h := range translat.CursorHeaders(token, d.cursorMachineID()) {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	cl := d.httpClient()
	if cursorTLSOverride != nil {
		tr := cl.Transport.(*http.Transport).Clone()
		tr.TLSClientConfig = cursorTLSOverride
		cl = &http.Client{Transport: tr, Timeout: cl.Timeout}
	}
	resp, err := cl.Do(req)
	if err != nil {
		doneWrite()
		return nil, nil, transportErr(ctx, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		doneWrite()
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, nil, translat.DecodeCursorError(limited, resp.StatusCode)
	}
	if !agent {
		return resp.Body, resp.Body, nil
	}
	// Duplex pump: parse response frames; answer the context handshake the
	// moment it arrives; forward every payload re-framed downstream.
	out, outW := io.Pipe()
	go func() {
		var werr error
		func() {
			defer resp.Body.Close()
			werr = translat.ReadCursorFrames(resp.Body, func(payload []byte) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				if translat.CursorAgentNeedsReply(payload) {
					select {
					case replies <- translat.CursorAgentReply:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				_, werr2 := outW.Write(translat.CursorFrame(payload))
				return werr2
			})
		}()
		close(replies) // END_STREAM: the run is over
		outW.CloseWithError(werr)
	}()
	return out, resp.Body, nil
}

// cursorMachineID resolves the machine id: the operator pin via
// extra_headers x-cursor-machine-id (recommended: the account's real
// storage.serviceMachineId), else "" (translat derives one from the token).
func (d *Def) cursorMachineID() string {
	return d.ExtraHeaders["x-cursor-machine-id"]
}

// SetCursorTLSOverrideForTest installs a TLS config for cursor upstream
// dials (tests only).
func SetCursorTLSOverrideForTest(cfg *tls.Config) { cursorTLSOverride = cfg }

// ResetCursorTLSOverrideForTest clears the test TLS override.
func ResetCursorTLSOverrideForTest() { cursorTLSOverride = nil }
