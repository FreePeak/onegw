package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"onegw/internal/translat"
	"onegw/internal/types"
)

// freebuff is Codebuff's free-tier OpenAI-compatible surface
// (https://www.codebuff.com/api/v1). OmniRoute's FreebuffExecutor is the
// reference: chat is NOT a plain proxy — every turn needs a freebuff
// session, an agent-run START, Buffy system prompt + codebuff_metadata on
// the body, and freebuff/codebuff headers on the completions POST, with a
// best-effort FINISH in the background.
//
// Auth is the Freebuff/Codebuff CLI auth token (Bearer), not a normal
// vendor API key. See ~/.config/manicode/credentials.json authToken.

const (
	freebuffDefaultBase = "https://www.codebuff.com/api/v1"
	freebuffSessionUA   = "codebuff/0.1.0 (darwin-arm64)"
	freebuffChatUA      = "ai-sdk/openai-compatible/1.0.25/codebuff"
	freebuffBuffyPrompt = "You are Buffy, the strategic coding assistant."
	freebuffDefaultAgent = "base2-free"
)

// freebuffModelToAgent maps Freebuff model ids onto Codebuff agent ids
// (OmniRoute open-sse/executors/freebuff.ts MODEL_TO_AGENT). Unknown models
// fall back to freebuffDefaultAgent.
var freebuffModelToAgent = map[string]string{
	"deepseek/deepseek-v4-flash":          "base2-free-deepseek-flash",
	"deepseek/deepseek-v4-pro":            "base2-free-deepseek",
	"openai/gpt-5.6-luna":                 "base2-free-luna",
	"minimax/minimax-m3":                  "base2-free-minimax-m3",
	"mimo/mimo-v2.5":                      "base2-free-mimo",
	"z-ai/glm-5.2":                        "base2-free-glm",
	"crof/kimi-k3-eco":                    "base2-free-kimi-k3-eco",
	"anthropic/claude-fable-5":            "base2-free-fable",
	"meta/muse-spark-1.2-contributor":     "base2-free-muse-spark",
}

// freebuffModels is the stock Freebuff free-tier catalog (OmniRoute
// registry/freebuff). Upstream rotates ids without notice; operators who
// want a pinned list set `models` explicitly.
var freebuffModels = []string{
	"deepseek/deepseek-v4-flash",
	"deepseek/deepseek-v4-pro",
	"openai/gpt-5.6-luna",
	"minimax/minimax-m3",
	"mimo/mimo-v2.5",
	"z-ai/glm-5.2",
	"crof/kimi-k3-eco",
	"anthropic/claude-fable-5",
	"meta/muse-spark-1.2-contributor",
}

// freebuffAgentID returns the Codebuff agent id for a Freebuff model id.
func freebuffAgentID(model string) string {
	model = strings.TrimPrefix(model, "freebuff/")
	if id, ok := freebuffModelToAgent[model]; ok {
		return id
	}
	return freebuffDefaultAgent
}

// freebuffClientSessionID is a 13-char base36 id (OmniRoute generateClientSessionId).
func freebuffClientSessionID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var b [13]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}

// doFreebuff runs the multi-step Freebuff chat path and returns the
// upstream chat/completions response as a normal OpenAI CallResult.
func (d *Def) doFreebuff(ctx context.Context, acct *Account, model string, body io.Reader, stream bool) (*CallResult, *types.APIError) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, errAPI(400, "invalid_request", err.Error())
	}
	token := acct.bearerToken()
	if token == "" {
		return nil, errAPI(401, "authentication_error", "Freebuff Auth Token required")
	}
	requestedModel := strings.TrimPrefix(model, "freebuff/")
	if requestedModel == "" {
		requestedModel = "deepseek/deepseek-v4-flash"
	}
	agentID := freebuffAgentID(requestedModel)
	base := d.Base(acct)
	if base == "" {
		base = freebuffDefaultBase
	}
	// Drop a trailing slash so path joins stay clean.
	base = strings.TrimRight(base, "/")

	instanceID, apiErr := d.freebuffSession(ctx, base, token, requestedModel)
	if apiErr != nil {
		return nil, apiErr
	}
	runID := d.freebuffStartRun(ctx, base, token, agentID)

	upstreamBody, apiErr := freebuffPrepareBody(raw, requestedModel, runID, instanceID, stream)
	if apiErr != nil {
		return nil, apiErr
	}

	url := base + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, errAPI(500, "internal", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", freebuffChatUA)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("x-freebuff-instance-id", instanceID)
	req.Header.Set("x-codebuff-agent-id", agentID)
	if runID != "" {
		req.Header.Set("x-codebuff-run-id", runID)
	}
	for k, v := range d.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, transportErr(ctx, err)
	}
	if runID != "" {
		// Best-effort FINISH; do not block the client on it (OmniRoute).
		go d.freebuffFinishRun(base, token, runID)
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
	// OpenAI Chat Completions wire on the way back.
	return &CallResult{Resp: resp, Format: translat.FmtOpenAI, Acct: acct}, nil
}

// freebuffSession POSTs /freebuff/session and returns instanceId.
func (d *Def) freebuffSession(ctx context.Context, base, token, model string) (string, *types.APIError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/freebuff/session", strings.NewReader("{}"))
	if err != nil {
		return "", errAPI(500, "internal", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", freebuffSessionUA)
	req.Header.Set("x-freebuff-model", model)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return "", transportErr(ctx, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", errAPI(502, "upstream_error", "read freebuff session: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// OmniRoute's executor only treats 2xx as usable for chat (409 is
		// fine for the quota probe, but carries no fresh instanceId).
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = fmt.Sprintf("Freebuff session failed (%d)", resp.StatusCode)
		} else {
			msg = fmt.Sprintf("Freebuff session failed (%d): %s", resp.StatusCode, msg)
		}
		return "", errAPI(resp.StatusCode, "upstream_error", msg)
	}
	var data struct {
		InstanceID string `json:"instanceId"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", errAPI(502, "upstream_error", "freebuff session response is not valid JSON")
	}
	return data.InstanceID, nil
}

// freebuffStartRun POSTs agent-runs START. Failures are swallowed: chat can
// still proceed without a run id (OmniRoute).
func (d *Def) freebuffStartRun(ctx context.Context, base, token, agentID string) string {
	payload, _ := json.Marshal(map[string]string{"action": "START", "agentId": agentID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/agent-runs", bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", freebuffSessionUA)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var data struct {
		RunID string `json:"runId"`
	}
	if json.Unmarshal(body, &data) != nil {
		return ""
	}
	return data.RunID
}

// freebuffFinishRun best-effort closes an agent run. Uses a detached
// context so a cancelled client request still FINISHes.
func (d *Def) freebuffFinishRun(base, token, runID string) {
	payload, _ := json.Marshal(map[string]any{
		"action":        "FINISH",
		"runId":         runID,
		"status":        "completed",
		"totalSteps":    1,
		"directCredits": 0,
		"totalCredits":  0,
	})
	req, err := http.NewRequest(http.MethodPost, base+"/agent-runs", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", freebuffSessionUA)
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}

// freebuffPrepareBody injects the Buffy system prompt and codebuff_metadata
// onto the client's OpenAI chat body (OmniRoute freebuff executor).
func freebuffPrepareBody(raw []byte, model, runID, instanceID string, stream bool) ([]byte, *types.APIError) {
	var payload map[string]any
	if len(raw) == 0 {
		payload = map[string]any{}
	} else if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errAPI(400, "invalid_request", "request body is not valid JSON")
	}
	msgs, _ := payload["messages"].([]any)
	incoming := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		if obj, ok := m.(map[string]any); ok {
			incoming = append(incoming, obj)
		}
	}
	hasBuffy := false
	if len(incoming) > 0 {
		if first, ok := incoming[0].(map[string]any); ok {
			role, _ := first["role"].(string)
			content, _ := first["content"].(string)
			if role == "system" && strings.HasPrefix(strings.TrimSpace(content), "You are Buffy") {
				hasBuffy = true
			}
		}
	}
	if !hasBuffy {
		incoming = append([]any{map[string]any{
			"role":    "system",
			"content": freebuffBuffyPrompt,
		}}, incoming...)
	}

	meta := map[string]any{}
	if existing, ok := payload["codebuff_metadata"].(map[string]any); ok {
		for k, v := range existing {
			meta[k] = v
		}
	}
	// Freebuff-owned fields win over any client-supplied collision.
	meta["run_id"] = runID
	meta["cost_mode"] = "free"
	meta["client_id"] = freebuffClientSessionID()
	meta["freebuff_instance_id"] = instanceID

	payload["model"] = model
	payload["messages"] = incoming
	payload["stream"] = stream
	payload["codebuff_metadata"] = meta

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, errAPI(500, "internal", err.Error())
	}
	return out, nil
}
