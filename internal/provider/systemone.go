package provider

import (
	"bytes"
	"context"
	"encoding/json"
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
		if apiErr == nil {
			apiErr = errAPI(resp.StatusCode, "upstream_error", string(limited))
		}
		return nil, apiErr
	}
	return &CallResult{Resp: resp, Format: translat.FmtSystemOne, Acct: acct}, nil
}

// EvalCfg is one evaluator leg inside a combo: TypeSafe's
// /v1/systemone endpoint answers these POSTs with a
// {score|choice|noul} verdict that the router turns into a
// combo reorder (strategy = "jev-eval"). Optional fields fall
// back to sensible defaults.
type EvalCfg struct {
	// Questions are authored systemone POST bodies forwarded
	// verbatim as the verdict request's state. At least one
	// required — an empty leg is skipped.
	Questions []string
	// ChoiceTargets maps a ChoiceVerdict.Option value to the
	// combo target it promotes to the front. Router-owned;
	// not typed here (avoid provider→router import cycle).
	ChoiceTargets map[string]string
}

// EvalResponse is what TypeSafe returns: score/choice/noul
// are mutually exclusive — a verdict is exactly one of
// {score, choice, noul}. An empty body carries no verdict.
type EvalResponse struct {
	Model  string          `json:"model"`
	Score  *ScoreVerdict   `json:"score,omitempty"`
	Choice *ChoiceVerdict  `json:"choice,omitempty"`
	Noul   *NoulVerdict    `json:"noul,omitempty"`
	Usage  json.RawMessage `json:"usage,omitempty"`
}

// ScoreVerdict is a numeric level verdict: the position on
// TypeSafe's severity spectrum the request scored at. Lower
// positions are safer models — combo reorder promotes the
// target matching the verdict's level first.
type ScoreVerdict struct {
	Level int `json:"level"`
}

// ChoiceVerdict names a specific option TypeSafe prefers;
// matched (case-insensitive, trimmed) against the evaluator's
// ChoiceTargets to pick a combo target by name.
type ChoiceVerdict struct {
	Option string `json:"option"`
}

// NoulVerdict means TypeSafe had no opinion: the combo keeps
// its configured order.
type NoulVerdict struct{}
