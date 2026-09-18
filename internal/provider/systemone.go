package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"onegw/internal/translat"
	"onegw/internal/types"
)

// errAPI builds a typed upstream error. Mirrors internal/server's
// errAPI; kept here so the provider package stays self-contained
// (server-level errAPI lives in internal/server/server.go).
func errAPI(status int, typ, msg string) *types.APIError {
	return &types.APIError{Status: status, Type: typ, Message: msg}
}

// doSystemOne performs one TypeSafe Jev upstream call (POST /v1/systemone).
//
// Same-shape mirror: the client and the upstream speak the same OpenAI
// Chat Completions wire, so the gateway forwards the client's request
// body verbatim and passes the upstream response back unchanged — no
// translation in either direction (see the Format() contract below).
//
// Auth: Bearer token via Authorization header (applyAuth default case).
//
// Response shape: a plain JSON object {model, answers, usage} — not a
// stream; the Jev endpoint answers synchronously with both turns
// accumulated. Non-streaming clients receive it directly; streaming
// clients get a single SSE chunk assembled by the server's
// ForcedStream path (aggregate), which is correct because the upstream
// already carries the complete answer.
func (d *Def) doSystemOne(ctx context.Context, acct *Account, model string, body io.Reader, stream bool) (*CallResult, *types.APIError) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, errAPI(400, "invalid_request", err.Error())
	}
	url := joinURL(d.Base(acct), d.Path("chat", model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, errAPI(500, "internal", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// applyAuth is the single credential owner for every kind;
	// systemone uses the default Bearer scheme.
	applyAuth(req.Header, d.Kind, acct.bearerToken(), model)
	// Forward any operator extra_headers (keys, etc.) verbatim.
	for k, v := range d.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, transportErr(ctx, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		apiErr := decodeUpstreamError(d.Kind, limited, resp.StatusCode)
		if apiErr ***REMOVED*** nil {
			apiErr = errAPI(resp.StatusCode, "upstream_error", string(limited))
		}
		return nil, apiErr
	}
	return &CallResult{Resp: resp, Format: translat.FmtSystemOne, Acct: acct}, nil
}
