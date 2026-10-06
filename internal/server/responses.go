package server

// OpenAI Responses API surface: POST /v1/responses for clients that speak
// Responses themselves, plus the stored-object endpoints (GET/DELETE
// /v1/responses/{id}, POST …/cancel, GET …/input_items).
//
// The request is relayed verbatim to a kind = "openai" provider's
// /v1/responses — only the model selector is rewritten — through the normal
// routing (combos, aliases, bare models), the client key's model allowlist,
// the provider quota gate and combo fall-through. Usage comes from the reply
// body or the stream's terminal response.* event and is charged exactly like
// a chat completion.
//
// A Responses object lives in the upstream ACCOUNT that created it, so the
// gateway keeps affinity (internal/store/responses.go): a previous_response_id
// continuation and every call on a stored id go back to the provider and
// account that served it. When that provider cannot serve any more (quota
// window exhausted, account refused for billing, or the model no longer
// routes to it) and [responses] history is on, the conversation is rebuilt
// from the stored turns and sent as a full input to the next combo target.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"onegw/internal/config"
	"onegw/internal/provider"
	"onegw/internal/router"
	"onegw/internal/store"
	"onegw/internal/translat"
	"onegw/internal/types"
)

// maxChainTurns bounds a history walk (a cycle cannot exist — ids are
// upstream-minted — but a corrupt store must not loop forever).
const maxChainTurns = 10000

// respRequest is what the gateway reads from a client Responses body; the
// body itself is relayed as-is.
type respRequest struct {
	Model              string          `json:"model"`
	Stream             bool            `json:"stream"`
	Background         bool            `json:"background"`
	PreviousResponseID string          `json:"previous_response_id"`
	Conversation       json.RawMessage `json:"conversation"`
	Input              json.RawMessage `json:"input"`
}

// respCall carries one client request through attempts and a migration.
type respCall struct {
	w     http.ResponseWriter
	r     *http.Request
	ak    *config.AuthKey
	owner string // keyOwner(ak)
	req   respRequest
	// input is the client's own input as an item array (a bare string
	// becomes one user message) — what history stores for this turn.
	input []json.RawMessage
	// committed is set once headers reached the client: no further
	// attempt may write.
	committed bool
}

// keyOwner identifies the client key that created a response without
// storing the key: labels may collide (policy.go), a digest prefix does not.
// The open gateway (no keys) owns everything as "".
func keyOwner(ak *config.AuthKey) string {
	if ak == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(ak.Key))
	return "k:" + hex.EncodeToString(sum[:8])
}

// responsesEligible reports whether def can serve the client's own
// Responses wire. Only OpenAI-compatible providers relay it verbatim.
func responsesEligible(def *provider.Def) bool {
	return def != nil && def.Kind == provider.KindOpenAI
}

// inputItems normalizes a Responses input into an item array: a string is
// one user message, an array is taken as-is, absent/null is empty.
func inputItems(raw json.RawMessage) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		msg, _ := json.Marshal(map[string]any{"role": "user", "content": s})
		return []json.RawMessage{msg}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
	return items, nil
}

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	ak, ok := s.authorize(w, r)
	if !ok {
		return
	}
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	release, ok := s.acquireForBody(r)
	if !ok {
		s.rejectSaturated(w, translat.FmtOpenAI)
		return
	}
	defer release()

	body, err := s.readBody(r)
	if err != nil {
		s.m.tooLarge()
		writeErr(w, translat.FmtOpenAI, errAPI(413, "body_too_large", err.Error()))
		return
	}
	rc := &respCall{w: w, r: r, ak: ak, owner: keyOwner(ak)}
	if err := json.Unmarshal(body, &rc.req); err != nil {
		writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request", "request body must be a JSON object: "+err.Error()))
		return
	}
	if rc.req.Model == "" {
		writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request", "missing model"))
		return
	}
	if rc.input, err = inputItems(rc.req.Input); err != nil {
		writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request", err.Error()))
		return
	}
	if !s.enforceRateLimits(w, translat.FmtOpenAI, ak) {
		return
	}

	st := s.cur()
	res, rerr := st.router.Resolve(rc.req.Model)
	if rerr != nil {
		s.m.noRoute(rerr.Status, rerr.Message)
		writeErr(w, translat.FmtOpenAI, rerr)
		return
	}
	if !s.enforceAllowlist(w, translat.FmtOpenAI, ak, rc.req.Model, res) {
		return
	}
	kept := res.Targets[:0:0]
	for _, t := range res.Targets {
		if def, ok := st.pool.Get(t.Provider); ok && responsesEligible(def) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		writeErr(w, translat.FmtOpenAI, errAPI(400, "responses_not_supported",
			fmt.Sprintf("model %q routes to no kind = \"openai\" provider; /v1/responses is relayed to OpenAI-compatible providers only", rc.req.Model)))
		return
	}
	res.Targets = kept
	if rc.req.Background {
		// A background response finishes after this request returns: its
		// usage never passes through the gateway, so a token-limited
		// provider would serve it outside its quota window.
		for _, t := range kept {
			if s.tokenLimited(t.Provider) {
				writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request",
					fmt.Sprintf("background responses are not allowed on %q: provider %s has a token quota and background usage cannot be counted", rc.req.Model, t.Provider)))
				return
			}
		}
	}
	if len(rc.req.Conversation) > 0 && !bytes.Equal(bytes.TrimSpace(rc.req.Conversation), []byte("null")) && len(kept) > 1 {
		// A Conversations object lives in one upstream account and the
		// gateway does not track or move it: on a multi-target route the
		// next turn could land on an account that has never seen it.
		writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request",
			fmt.Sprintf("the conversation parameter needs a single-provider route; %q is a combo of %d providers (use previous_response_id instead)", rc.req.Model, len(kept))))
		return
	}

	if prevID := rc.req.PreviousResponseID; prevID != "" && s.st != nil {
		row, found, err := s.st.GetResponse(prevID)
		if err != nil {
			log.Printf("server: responses affinity lookup %s: %v", prevID, err)
		}
		if found {
			if row.APIKey != rc.owner {
				writeErr(w, translat.FmtOpenAI, errAPI(404, "invalid_request_error",
					fmt.Sprintf("Previous response with id '%s' not found.", prevID)))
				return
			}
			s.continueResponse(rc, res, body, row)
			return
		}
		// Unknown id (created before this gateway, or pruned): route
		// normally and let the upstream answer for it.
	}
	if aerr := s.serveResponses(rc, res, body, rc.req.PreviousResponseID); aerr != nil && !rc.committed {
		writeErr(w, translat.FmtOpenAI, aerr)
	}
}

// tokenLimited reports whether provider name enforces a token quota.
func (s *Server) tokenLimited(name string) bool {
	for _, p := range s.cur().cfg.Providers {
		if p.Name == name {
			return p.QuotaWindow != "" && p.QuotaLimitTokens > 0
		}
	}
	return false
}

// serveResponses runs body through the router over res (combo fall-through
// included). prevID is recorded as the new response's predecessor.
func (s *Server) serveResponses(rc *respCall, res *router.Resolution, body []byte, prevID string) *types.APIError {
	attempts := 0
	ctx := router.WithIdentity(rc.r.Context(), identityFromBody(rc.r.Header, rc.ak, body))
	return s.cur().router.Execute(ctx, res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		if rc.committed {
			// Defensive: a committed reply ends the request; nothing
			// may be written twice.
			return nil, nil
		}
		attempts++
		return nil, s.responsesAttempt(ctx, rc, def, acct, m, body, attempts, prevID)
	}, func(any) {})
}

// continueResponse serves a previous_response_id continuation: on the
// provider and account that stored the previous response when they can
// still serve, else by moving the conversation (migrateResponse).
func (s *Server) continueResponse(rc *respCall, res *router.Resolution, body []byte, row store.ResponseRow) {
	st := s.cur()
	reason := ""
	def, ok := st.pool.Get(row.Provider)
	var target *router.Target
	if ok {
		for i := range res.Targets {
			if res.Targets[i].Provider == row.Provider {
				target = &res.Targets[i]
				break
			}
		}
	}
	acct := findAccount(def, row.Account)
	switch {
	case !ok || acct == nil:
		reason = fmt.Sprintf("provider %s account %s is no longer configured", row.Provider, row.Account)
	case target == nil:
		reason = fmt.Sprintf("model %q no longer routes to provider %s", rc.req.Model, row.Provider)
	case slices.Contains(def.Invalidated(), acct.Name):
		reason = fmt.Sprintf("provider %s account %s is refused for billing", row.Provider, acct.Name)
	}
	var pinErr *types.APIError
	if reason == "" {
		pinErr = s.responsesAttempt(rc.r.Context(), rc, def, acct, target.Model, body, 1, row.ID)
		if pinErr == nil || rc.committed {
			return
		}
		if pinErr.Type != "provider_quota_exhausted" && !pinErr.PaymentRequired() {
			// Rate limits and upstream faults are not a reason to move
			// a conversation (moving drops reasoning and the prompt
			// cache): the client retries on the same account.
			writeErr(rc.w, translat.FmtOpenAI, pinErr)
			return
		}
		reason = pinErr.Message
	}
	s.migrateResponse(rc, res, body, row, reason, pinErr)
}

// findAccount returns def's account named name (nil when gone).
func findAccount(def *provider.Def, name string) *provider.Account {
	if def == nil {
		return nil
	}
	for i := range def.Accounts {
		if def.Accounts[i].Name == name {
			return &def.Accounts[i]
		}
	}
	return nil
}

// migrateResponse rebuilds the conversation ending at row from history and
// serves it as a full input on the route's other targets.
func (s *Server) migrateResponse(rc *respCall, res *router.Resolution, body []byte, row store.ResponseRow, reason string, cause *types.APIError) {
	conflict := func(msg string) {
		writeErr(rc.w, translat.FmtOpenAI, errAPI(409, "conversation_not_movable",
			fmt.Sprintf("previous response %s cannot continue on provider %s (%s) and %s", row.ID, row.Provider, reason, msg)))
	}
	if !s.cur().cfg.Responses.History {
		conflict("moving the conversation needs [responses] history = true")
		return
	}
	others := res.Targets[:0:0]
	for _, t := range res.Targets {
		if t.Provider != row.Provider {
			others = append(others, t)
		}
	}
	if len(others) == 0 {
		if cause != nil {
			writeErr(rc.w, translat.FmtOpenAI, cause)
			return
		}
		conflict(fmt.Sprintf("model %q has no other provider to move it to", rc.req.Model))
		return
	}
	history, err := s.rebuildChain(row)
	if err != nil {
		conflict(err.Error())
		return
	}
	full, err := portableItems(append(history, rc.input...))
	if err != nil {
		conflict(err.Error())
		return
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		writeErr(rc.w, translat.FmtOpenAI, errAPI(400, "invalid_request", err.Error()))
		return
	}
	delete(root, "previous_response_id")
	root["input"], _ = json.Marshal(full)
	moved, err := json.Marshal(root)
	if err != nil {
		writeErr(rc.w, translat.FmtOpenAI, errAPI(500, "internal", err.Error()))
		return
	}
	log.Printf("server: responses: moving conversation %s off %s (%s): %d history items", row.ID, row.Provider, reason, len(full)-len(rc.input))
	mres := *res
	mres.Targets = others
	if aerr := s.serveResponses(rc, &mres, moved, row.ID); aerr != nil && !rc.committed {
		writeErr(rc.w, translat.FmtOpenAI, aerr)
	}
}

// rebuildChain walks the stored turns ending at row and returns their
// items oldest first: each turn's input, then its output.
func (s *Server) rebuildChain(row store.ResponseRow) ([]json.RawMessage, error) {
	var turns []store.ResponseRow
	cur := row
	for {
		if cur.Input == "" && cur.Output == "" {
			return nil, fmt.Errorf("its history is incomplete (turn %s was served without [responses] history)", cur.ID)
		}
		turns = append(turns, cur)
		if cur.PrevID == "" {
			break
		}
		if len(turns) >= maxChainTurns {
			return nil, fmt.Errorf("its history exceeds %d turns", maxChainTurns)
		}
		next, found, err := s.st.GetResponse(cur.PrevID)
		if err != nil {
			return nil, fmt.Errorf("its history could not be read: %v", err)
		}
		if !found {
			return nil, fmt.Errorf("its history is incomplete (turn %s is unknown or expired)", cur.PrevID)
		}
		cur = next
	}
	var out []json.RawMessage
	for i := len(turns) - 1; i >= 0; i-- {
		for _, part := range []string{turns[i].Input, turns[i].Output} {
			if part == "" {
				continue
			}
			var items []json.RawMessage
			if err := json.Unmarshal([]byte(part), &items); err != nil {
				return nil, fmt.Errorf("stored turn %s is unreadable: %v", turns[i].ID, err)
			}
			out = append(out, items...)
		}
	}
	return out, nil
}

// serverSideToolItems are built-in tool calls the upstream executed and
// stored itself; they cannot be replayed into another account, and the
// model's following message already carries what it took from them.
var serverSideToolItems = map[string]bool{
	"web_search_call": true, "file_search_call": true, "image_generation_call": true,
	"code_interpreter_call": true, "mcp_call": true, "mcp_list_tools": true,
	"mcp_approval_request": true, "mcp_approval_response": true,
}

// portableItems turns a conversation's items into an input another account
// accepts: item_reference entries are resolved to the referenced item,
// reasoning items and anything carrying encrypted_content are dropped (they
// are encrypted for the account that produced them), server-side tool calls
// are dropped, assistant output messages become plain assistant messages,
// and upstream item ids are removed (they name objects the new account
// does not have).
func portableItems(items []json.RawMessage) ([]json.RawMessage, error) {
	byID := map[string]json.RawMessage{}
	for _, it := range items {
		var head struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal(it, &head) == nil && head.ID != "" && head.Type != "item_reference" {
			byID[head.ID] = it
		}
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(it, &m); err != nil {
			return nil, fmt.Errorf("a stored item is not an object: %v", err)
		}
		typ := rawString(m["type"])
		if typ == "item_reference" {
			ref, ok := byID[rawString(m["id"])]
			if !ok {
				return nil, fmt.Errorf("item_reference %s points outside the stored history", rawString(m["id"]))
			}
			m = nil
			if err := json.Unmarshal(ref, &m); err != nil {
				return nil, err
			}
			typ = rawString(m["type"])
		}
		if typ == "reasoning" || serverSideToolItems[typ] {
			continue
		}
		if _, enc := m["encrypted_content"]; enc {
			continue
		}
		if typ == "message" && rawString(m["role"]) == "assistant" {
			out = append(out, assistantText(m))
			continue
		}
		delete(m, "id")
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// assistantText renders an assistant output message as a plain assistant
// input message (its output_text and refusal parts, in order).
func assistantText(m map[string]json.RawMessage) json.RawMessage {
	var text strings.Builder
	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	if json.Unmarshal(m["content"], &parts) == nil {
		for _, p := range parts {
			text.WriteString(p.Text)
			text.WriteString(p.Refusal)
		}
	} else {
		var s string
		_ = json.Unmarshal(m["content"], &s)
		text.WriteString(s)
	}
	b, _ := json.Marshal(map[string]any{"role": "assistant", "content": text.String()})
	return b
}

// rawString decodes a JSON string value ("" for anything else).
func rawString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// responsesAttempt makes one upstream call for a Responses request and
// relays the answer. A nil return means the reply was written; an error
// before any byte reached the client lets the router try again or fall
// through.
func (s *Server) responsesAttempt(ctx context.Context, rc *respCall, def *provider.Def, acct *provider.Account,
	model string, body []byte, attempts int, prevID string) *types.APIError {
	if qerr := s.quotaGate(def); qerr != nil {
		return qerr
	}
	out, _ := rewriteModel(body, model)
	setDecisionHeader(rc.w, def, acct, model, attempts)
	res, apiErr := def.DoResponses(ctx, acct, model, rc.r.Header, bytes.NewReader(out), rc.req.Stream)
	if apiErr != nil {
		s.noteAttemptErr(def, acct, model, s.boundedModel(model), apiErr)
		return apiErr
	}
	defer res.Resp.Body.Close()

	var got respCapture
	if rc.req.Stream {
		got = s.relayResponsesStream(ctx, rc, res.Resp)
	} else {
		raw, err := io.ReadAll(res.Resp.Body)
		if err != nil {
			herr := errAPI(502, "upstream_unreachable", err.Error())
			s.m.upstreamErr(def.Name, model, acctName(acct), herr)
			return herr
		}
		ct := res.Resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		rc.w.Header().Set("Content-Type", ct)
		rc.w.WriteHeader(res.Resp.StatusCode)
		rc.committed = true
		_, _ = rc.w.Write(raw)
		got.object(raw)
	}
	s.recordSuccess(ctx, res, def, model, translat.FmtResponses, got.usage, len(out), 0, rc.ak)
	s.rememberResponse(rc, def, acct, model, prevID, got)
	return nil
}

// respCapture is what the gateway keeps from a served response.
type respCapture struct {
	id     string
	usage  types.Usage
	output json.RawMessage
}

// object reads a Responses object (a non-stream body, or the response
// field of a stream's response.* event).
func (c *respCapture) object(raw []byte) {
	var obj struct {
		ID     string          `json:"id"`
		Output json.RawMessage `json:"output"`
		Usage  *struct {
			InputTokens        int64 `json:"input_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	if obj.ID != "" {
		c.id = obj.ID
	}
	if len(obj.Output) > 0 && !bytes.Equal(obj.Output, []byte("null")) {
		c.output = obj.Output
	}
	if u := obj.Usage; u != nil {
		c.usage = types.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens,
			UpstreamFormat: string(translat.FmtResponses)}
		if u.InputTokensDetails != nil {
			c.usage.CacheReadTokens = u.InputTokensDetails.CachedTokens
		}
		if u.OutputTokensDetails != nil {
			c.usage.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
		}
	}
}

// relayResponsesStream relays an SSE Responses stream byte for byte,
// flushing at every event boundary, and captures the response id
// (response.created) and the final object (response.completed /
// .incomplete / .failed).
func (s *Server) relayResponsesStream(ctx context.Context, rc *respCall, resp *http.Response) respCapture {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	rc.w.Header().Set("Content-Type", ct)
	rc.w.Header().Set("Cache-Control", "no-cache")
	rc.w.WriteHeader(resp.StatusCode)
	rc.committed = true
	flusher, _ := rc.w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	var got respCapture
	var data bytes.Buffer
	event := func() {
		if data.Len() == 0 {
			return
		}
		var ev struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(data.Bytes(), &ev) == nil && len(ev.Response) > 0 {
			switch ev.Type {
			case "response.created", "response.in_progress", "response.completed",
				"response.incomplete", "response.failed":
				got.object(ev.Response)
			}
		}
		data.Reset()
	}
	br := bufio.NewReaderSize(newIdleBreak(resp.Body, resp.Body), 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			_, _ = rc.w.Write(line)
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				event()
				flush()
			case bytes.HasPrefix(trimmed, []byte("data:")):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(bytes.TrimPrefix(trimmed, []byte("data:")), []byte(" ")))
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				log.Printf("server: responses stream from upstream interrupted: %v", err)
			}
			break
		}
	}
	event()
	flush()
	return got
}

// rememberResponse stores where a response was served (and, with history
// on, the turn's items). Best-effort: a failed write only costs the
// conversation's movability.
func (s *Server) rememberResponse(rc *respCall, def *provider.Def, acct *provider.Account, model, prevID string, got respCapture) {
	if s.st == nil || got.id == "" {
		return
	}
	row := store.ResponseRow{ID: got.id, Provider: def.Name, Account: acctName(acct), Model: model,
		APIKey: rc.owner, PrevID: prevID, Created: time.Now()}
	if s.cur().cfg.Responses.History {
		in := rc.input
		if in == nil {
			in = []json.RawMessage{}
		}
		b, _ := json.Marshal(in)
		row.Input = string(b)
		row.Output = "[]"
		if len(got.output) > 0 {
			row.Output = string(got.output)
		}
	}
	if err := s.st.PutResponse(row); err != nil {
		log.Printf("server: responses: remember %s: %v", got.id, err)
	}
}

// responsesResource serves the stored-object endpoints: the call goes to
// the provider and account that created the response, and only for the
// client key that created it.
func (s *Server) responsesResource(method, suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ak, ok := s.authorize(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		notFound := func() {
			writeErr(w, translat.FmtOpenAI, errAPI(404, "invalid_request_error",
				fmt.Sprintf("No response found with id '%s'.", id)))
		}
		if s.st == nil {
			notFound()
			return
		}
		row, found, err := s.st.GetResponse(id)
		if err != nil {
			log.Printf("server: responses affinity lookup %s: %v", id, err)
		}
		if !found || row.APIKey != keyOwner(ak) {
			notFound()
			return
		}
		def, _ := s.cur().pool.Get(row.Provider)
		acct := findAccount(def, row.Account)
		if acct == nil {
			writeErr(w, translat.FmtOpenAI, errAPI(404, "invalid_request_error",
				fmt.Sprintf("response '%s' was served by provider %s account %s, which is no longer configured", id, row.Provider, row.Account)))
			return
		}
		pq := "/v1/responses/" + url.PathEscape(id) + suffix
		if r.URL.RawQuery != "" {
			pq += "?" + r.URL.RawQuery
		}
		resp, aerr := def.DoResponsesResource(r.Context(), acct, method, pq, r.Header)
		if aerr != nil {
			writeErr(w, translat.FmtOpenAI, aerr)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(flushWriter{w, func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}}, resp.Body)
		if method == http.MethodDelete && resp.StatusCode < 300 {
			if err := s.st.DeleteResponse(id); err != nil {
				log.Printf("server: responses: forget %s: %v", id, err)
			}
		}
	}
}

// pruneResponsesOnce drops affinity/history rows past [responses]
// retention. Returns the rows deleted.
func (s *Server) pruneResponsesOnce() (int64, error) {
	if s.st == nil {
		return 0, nil
	}
	st := s.cur()
	if st == nil {
		return 0, nil
	}
	return s.st.PruneResponses(time.Now().Add(-st.cfg.Responses.RetentionDur()))
}
