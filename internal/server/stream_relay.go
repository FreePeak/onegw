package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"onegw/internal/config"
	"onegw/internal/translat"
	"onegw/internal/types"
)

const (
	// streamReserveBytes is the fixed byte reservation each streamed request
	// takes from the global budget: the peeked prefix plus splice headroom.
	// Streamed bodies never accumulate in memory, so a flat per-request cap
	// bounds the pipeline instead of the buffered path's 4x body multiple.
	streamReserveBytes = 256 << 10

	// streamPeekLimit caps the prefix read used to find "model"/"stream".
	// The prefix is relayed as part of the body either way, so the limit is
	// detection accuracy, not a body cap.
	streamPeekLimit = 16 << 10
)

// streamScan is what the prefix scanner learned about a request body.
type streamScan struct {
	model      string // decoded top-level "model" string
	modelOK    bool
	stream     bool // top-level "stream" literal (absent = false, like peekStream)
	streamOK   bool // the stream literal was actually seen in the prefix
	ineligible bool // buffered pipeline owns this body (normalizeRoles trigger)

	modelQuoteStart int // offset of the "model" value's opening quote
	modelQuoteEnd   int // offset of the "model" value's closing quote
}

// proxyStream, when eligible, relays the request body to the upstream
// without a full read: the peeked prefix is inspected for routing metadata,
// the routed model is spliced into it if needed, and the prefix plus the
// live remainder of r.Body go to the transport chunked.
//
// It returns handled=true when it wrote a response (success or error).
// handled=false means "not eligible": the body is restored intact as
// prefix+rest and the buffered pipeline below runs unchanged.
//
// Streaming is single-shot: the body can be read once, so there is no retry
// and no account rotation on this path. Saver, always-thinking adaptation
// and normalizeRoles need the full body, so those requests stay buffered.
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, clientFmt translat.Format, ak *config.AuthKey) (handled bool) {
	st := s.cur()
	// Delivery clock starts here; the ctx is tagged at the relay call —
	// NOT via r.WithContext, which would fork the request object and let
	// streamFallback's body restoration land on a copy proxy() never sees.
	d := &delivery{start: time.Now()}

	// Declared-size gate mirrors the buffered path's 413 semantics. Chunked
	// bodies (unknown length) are not capped mid-stream: bytes are never
	// accumulated, and the fixed reservation bounds memory.
	if r.ContentLength > st.cfg.Server.MaxBody {
		writeErr(w, clientFmt, errAPI(413, "body_too_large", fmt.Sprintf("body exceeds %d bytes", st.cfg.Server.MaxBody)))
		return true
	}

	release, ok := s.acquireForStream(r.Context())
	if !ok {
		s.rejectSaturated(w, clientFmt)
		return true
	}
	defer release()

	prefix, whole, err := readPrefix(r.Body)
	if err != nil {
		// Keep the consumed bytes reachable; readBody below re-reads the
		// body, hits the same error, and surfaces it like the buffered
		// path always has.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), r.Body))
		return false
	}
	sc, ok := scanTopLevel(prefix)
	if !ok || !sc.modelOK || sc.ineligible {
		return s.streamFallback(r, prefix, whole)
	}
	var model string
	if err := json.Unmarshal(prefix[sc.modelQuoteStart:sc.modelQuoteEnd+1], &model); err != nil || model == "" {
		return s.streamFallback(r, prefix, whole)
	}
	d.model = s.boundedModel(model)

	res, rerr := st.router.Resolve(model)
	if rerr != nil {
		writeErr(w, clientFmt, rerr)
		return true
	}
	if res.IsCombo || len(res.Targets) != 1 {
		// Streaming consumes the body once; combo fallback chains and
		// cross-target replay are impossible here — leave them buffered.
		return s.streamFallback(r, prefix, whole)
	}
	t := res.Targets[0]
	def, ok := st.pool.Get(t.Provider)
	if !ok || def.UpstreamFormat(t.Model) != clientFmt || def.AlwaysThinkingModel(t.Model) ||
		// Echo synthesis needs the buffered pipeline (2026-09-11): the
		// raw fast path bypasses prepareUpstreamBody entirely, so an
		// echo_reasoning or runtime-learned model must not ride it —
		// the refusal would repeat on every client retry.
		def.ReasoningEchoModel(t.Model) ||
		// Cache-profile anchoring requires the buffered pipeline
		// (issue #34): the raw fast path bypasses prepareUpstreamBody.
		(def.CacheProfile != "" && def.CacheProfile != "none") ||
		// Per-model lockout: a benched model must be skipped by
		// Router.Execute (503 provider_model_benched or combo
		// fall-through), which this single-shot path cannot do —
		// fall back to the buffered pipeline.
		func() bool { b, _ := def.ModelBenched(t.Model); return b }() {
		return s.streamFallback(r, prefix, whole)
	}
	if !s.enforceAllowlist(w, clientFmt, ak, model, res) {
		return true // enforceAllowlist wrote the response
	}
	// Rate limits and quota are enforced here only on the handled path:
	// requests that fall back get them from the buffered pipeline below,
	// so counters are never double-counted.
	if !s.enforceRateLimits(w, clientFmt, ak) {
		return true
	}
	// Quota enforcement (issue #7), mirrored from attempt(): an exhausted
	// provider cools its whole account pool until the window ends and
	// answers 503. Retry-After rides on the error.
	if q := st.quota; q != nil {
		if qs, ok := q.Status(def.Name, time.Now()); ok && qs.Exhausted {
			cool := time.Until(qs.WindowEnd)
			if cool < 0 {
				cool = 0
			}
			for i := range def.Accounts {
				def.Cool(&def.Accounts[i], cool)
			}
			writeErr(w, clientFmt, &types.APIError{Status: 503, Type: "provider_quota_exhausted", Code: "quota_exceeded",
				RetryAfter: strconv.FormatInt(int64(cool.Seconds())+1, 10),
				Message: fmt.Sprintf("provider %s quota exhausted (%s window); resets %s",
					def.Name, qs.Window, qs.WindowEnd.UTC().Format(time.RFC3339))})
			return true
		}
	}

	relayPrefix := prefix
	if model != t.Model {
		relayPrefix = spliceModelValue(prefix, sc, t.Model)
	}
	src := &countingReader{r: io.MultiReader(bytes.NewReader(relayPrefix), r.Body)}

	id := requestIdentity(r.Header, ak)
	acct, poolReady := def.NextAccount(id)
	if acct == nil {
		// Whole account pool cooling from upstream 429s: never send a
		// doomed upstream call from the single-shot fast path. Answer
		// 429 with the pool's soonest recovery, exactly like the
		// buffered pipeline's fall-through (server.go attempt()).
		cool := time.Until(poolReady)
		if cool < 0 {
			cool = 0
		}
		writeErr(w, clientFmt, &types.APIError{Status: 429, Type: "provider_rate_limited",
			Code:       "rate_limit_exceeded",
			RetryAfter: strconv.FormatInt(int64(cool.Seconds())+1, 10),
			Message: fmt.Sprintf("provider %s: all accounts rate-limited upstream; retry after %ds",
				def.Name, int64(cool.Seconds())+1)})
		return true
	}
	// Single-shot fast path: stamp the decision before the upstream call
	// (no body written yet). A failure falls back to the buffered
	// pipeline, whose attempt() re-stamps it.
	setDecisionHeader(w, def, acct, t.Model, 1)
	cres, apiErr := def.Do(r.Context(), acct, t.Model, r.Header, src, sc.stream)
	if apiErr != nil {
		s.m.upstreamErr(def.Name, t.Model, acctName(acct), apiErr)
		def.Unpin(id) // failed fast-path attempt must not keep its pin
		// Transient, retryable failures get a second chance. whole=true
		// means the complete body is still buffered in the prefix —
		// streamFallback reconstitutes r.Body from it and re-enters the
		// buffered pipeline, where Execute's retry backoff (1s steps for
		// shared model-concurrency windows) and combo fall-through apply
		// with full semantics. This rides out the observed ~2-5s upstream
		// windows that today surface as raw terminal 429s.
		// whole=false cannot replay: the transport consumed part of the
		// live body stream and those bytes are gone — buffering every
		// request up front would forfeit the fast path's RAM contract.
		// For those, answer a managed retryable error with an honest
		// short Retry-After: the client's fresh retry re-enters the fast
		// path whole and lands post-window.
		if apiErr.Retryable() {
			if whole {
				return s.streamFallback(r, prefix, true)
			}
			writeErr(w, clientFmt, &types.APIError{Status: apiErr.Status,
				Type: "upstream_transient", Code: apiErr.Code,
				RetryAfter: "1", Message: apiErr.Message})
			return true
		}
		if apiErr.Fallbackable {
			// Pre-body gated 403 (issue #48): Do benched the account, but
			// this single-shot path cannot rotate — the transport already
			// consumed the streamed body, so a replay would be truncated.
			// Answer the same cooling-pool 429 the empty-pool branch
			// above produces (the buffered path's fall-through analog):
			// the client's retry lands on the next account or falls
			// through the combo, and the raw 403 never surfaces while
			// the pool can still serve. Retry-After comes from a fresh
			// pool probe: ~1s when another account is live, the soonest
			// ladder expiry when the whole pool is benched.
			_, ready := def.NextAccount(id)
			cool := time.Until(ready)
			if cool < 0 {
				cool = 0
			}
			writeErr(w, clientFmt, &types.APIError{Status: 429, Type: "provider_rate_limited",
				Code:       "rate_limit_exceeded",
				RetryAfter: strconv.FormatInt(int64(cool.Seconds())+1, 10),
				Message: fmt.Sprintf("provider %s: account gated upstream; retry after %ds",
					def.Name, int64(cool.Seconds())+1)})
			return true
		}
		if alwaysThinking400(apiErr) {
			// Single-shot path: no replay/retry is possible, but the
			// learned flag makes every future request for this model
			// fall back to the buffered pipeline above (eligible guard
			// consults AlwaysThinkingModel), where the body is coerced.
			if def.LearnAlwaysThinking(t.Model) {
				log.Printf("server: learned always-thinking %s/%s from upstream 400; future stream requests go buffered", def.Name, t.Model)
			}
		}
		if apiErr.ReasoningEchoRequired() {
			// Same medicine as always-thinking above: no replay is
			// possible single-shot, but the learned flag reroutes every
			// future stream request for this model to the buffered
			// pipeline (the eligibility guard consults
			// ReasoningEchoModel), where synthesizeReasoningEcho fills
			// the missing echoes before the upstream call.
			if def.LearnReasoningEcho(t.Model) {
				log.Printf("server: learned reasoning-echo %s/%s from upstream 400; future stream requests go buffered", def.Name, t.Model)
			}
		}
		if w.Header().Get("Content-Type") == "" {
			writeErr(w, clientFmt, apiErr)
		}
		return true
	}
	// Same format by construction; the saver is off (checked by the
	// caller), so savedTokens is 0. The byte count is read after Do
	// returns: the transport has consumed the body by then. Used only for
	// the len/4 usage estimate, exactly like the buffered path.
	_ = s.relayResponse(w, cres, def, t.Model, clientFmt, clientFmt, sc.stream, int(src.n), 0, ak, withDelivery(r.Context(), d))
	return true
}

// streamFallback reconstitutes the request body and hands control back to
// the buffered pipeline.
func (s *Server) streamFallback(r *http.Request, prefix []byte, whole bool) bool {
	if whole {
		r.Body = io.NopCloser(bytes.NewReader(prefix))
	} else {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), r.Body))
	}
	return false
}

// readPrefix consumes up to streamPeekLimit bytes from r. whole=true means
// the body ended inside the prefix (EOF observed).
func readPrefix(r io.Reader) (prefix []byte, whole bool, err error) {
	buf := make([]byte, streamPeekLimit)
	n := 0
	for n < len(buf) {
		m, rerr := r.Read(buf[n:])
		n += m
		if rerr != nil {
			if rerr == io.EOF {
				return buf[:n], true, nil
			}
			return buf[:n], false, rerr
		}
	}
	return buf[:n], false, nil
}

// acquireForStream reserves the fixed streaming reservation from the global
// budget. ok=false means the gateway is saturated.
func (s *Server) acquireForStream(ctx context.Context) (func(), bool) {
	st := s.cur()
	if err := st.budget.Acquire(ctx, streamReserveBytes); err != nil {
		st.budget.Saturated()
		return nil, false
	}
	return func() { st.budget.Release(streamReserveBytes) }, true
}

// spliceModelValue replaces the byte range of the "model" value inside
// prefix with a fresh JSON string for upModel. Everything outside that
// range stays byte-identical: key order, whitespace and formatting are
// preserved (unlike the buffered path's map re-marshal).
func spliceModelValue(prefix []byte, sc streamScan, upModel string) []byte {
	q, err := json.Marshal(upModel)
	if err != nil {
		return prefix
	}
	out := make([]byte, 0, sc.modelQuoteStart+len(q)+len(prefix)-sc.modelQuoteEnd-1)
	out = append(out, prefix[:sc.modelQuoteStart]...)
	out = append(out, q...)
	return append(out, prefix[sc.modelQuoteEnd+1:]...)
}

// countingReader counts bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// hasDeveloperRole reports whether the visible bytes of a messages array
// contain a role:"developer" entry (a normalizeRoles trigger). It is a
// literal byte scan, so it tolerates a prefix-truncated array; string
// content merely quoting `"role": "developer"` also matches — that only
// causes a conservative fallback to the buffered path, never a wrong
// relay.
func hasDeveloperRole(visible []byte) bool {
	i := bytes.Index(visible, []byte(`"role"`))
	for i >= 0 {
		j := i + len(`"role"`)
		for j < len(visible) && (visible[j] == ' ' || visible[j] == '\t' || visible[j] == '\n' || visible[j] == '\r') {
			j++
		}
		if j < len(visible) && visible[j] == ':' {
			j++
			for j < len(visible) && (visible[j] == ' ' || visible[j] == '\t' || visible[j] == '\n' || visible[j] == '\r') {
				j++
			}
			if bytes.HasPrefix(visible[j:], []byte(`"developer"`)) {
				return true
			}
		}
		next := bytes.Index(visible[i+1:], []byte(`"role"`))
		if next < 0 {
			return false
		}
		i += 1 + next
	}
	return false
}

// scanTopLevel walks the leading bytes of a JSON object and records the
// top-level keys the streaming gate needs: "model" (string, with its exact
// byte range for splicing), "stream" (bool), and the normalizeRoles
// triggers (top-level "store", messages[].role=="developer") which force
// the buffered path.
//
// Values of uninteresting keys are skipped only while they fit in the
// prefix: scanning stops for good at the first value that runs past it.
// Only a complete top-level "model" string is required — bodies whose
// model field is not fully visible fall back to buffering. Triggers
// hiding beyond the prefix go undetected; streaming is opt-in and this
// tail risk is documented (normalizeRoles is a client-compat nicety, not
// a protocol requirement).
func scanTopLevel(prefix []byte) (sc streamScan, ok bool) {
	i, n := 0, len(prefix)
	skipSpace := func() {
		for i < n && (prefix[i] == ' ' || prefix[i] == '\t' || prefix[i] == '\n' || prefix[i] == '\r') {
			i++
		}
	}
	skipSpace()
	if i >= n || prefix[i] != '{' {
		return sc, false
	}
	i++
	// bail returns the scan result when the prefix ran out mid-value: an
	// unseen "stream" is then UNKNOWN (a buffer-pinned client may have
	// asked for SSE behind the prefix), so streaming requires having
	// definitively seen the literal. A stop at the closing brace is
	// different — absence there is definitive (handled by the '}' paths).
	bail := func() (streamScan, bool) {
		if !sc.streamOK {
			return sc, false
		}
		return sc, sc.modelOK
	}
	for {
		skipSpace()
		if i >= n {
			return bail()
		}
		if prefix[i] == '}' {
			return sc, sc.modelOK // whole object scanned: absence is definitive
		}
		if prefix[i] != '"' {
			return sc, false
		}
		keyStart := i
		after, ok := scanJSONString(prefix, i)
		if !ok {
			return sc, false // key name itself truncated: model can't be trusted
		}
		i = after
		skipSpace()
		if i >= n || prefix[i] != ':' {
			return sc, false
		}
		i++
		skipSpace()
		if i >= n {
			return sc, false
		}
		switch string(prefix[keyStart+1 : after-1]) {
		case "model":
			if prefix[i] != '"' {
				return sc, false // non-string model: buffered path reports it
			}
			sc.modelQuoteStart = i
			val, ok := scanJSONString(prefix, i)
			if !ok {
				return sc, false // value truncated: cannot route
			}
			sc.modelQuoteEnd = val - 1
			sc.modelOK = true
			i = val
		case "stream":
			switch {
			case bytes.HasPrefix(prefix[i:], []byte("true")):
				sc.stream = true
				sc.streamOK = true
				i += 4
			case bytes.HasPrefix(prefix[i:], []byte("false")):
				sc.streamOK = true
				i += 5
			default:
				return sc, false // truncated or malformed literal
			}
		case "store":
			sc.ineligible = true
			after, ok := skipJSONValue(prefix, i)
			if !ok {
				return bail()
			}
			i = after
		case "messages":
			// normalizeRoles triggers: a role:"developer" entry, or any
			// assistant "reasoning" echo key (the same-format rename needs
			// the full body). The literal `"reasoning"` also matches
			// string content and a trailing "reasoning_effort" — over-
			// matching only costs the fast path, never correctness.
			if hasDeveloperRole(prefix[i:]) || bytes.Contains(prefix[i:], []byte(`"reasoning"`)) {
				sc.ineligible = true
			}
			after, ok := skipJSONValue(prefix, i)
			if !ok {
				return bail()
			}
			i = after
		default:
			after, ok := skipJSONValue(prefix, i)
			if !ok {
				return bail()
			}
			i = after
		}
		skipSpace()
		if i < n && prefix[i] == ',' {
			i++
			continue
		}
		if i < n && prefix[i] == '}' {
			return sc, sc.modelOK // whole object scanned: absence is definitive
		}
		return sc, false // malformed separator
	}
}

// scanJSONString scans a string starting at its opening quote and returns
// the index just past the closing quote. ok=false when the prefix ends
// first.
func scanJSONString(prefix []byte, open int) (int, bool) {
	for j := open + 1; j < len(prefix); j++ {
		switch prefix[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// skipJSONValue skips any JSON value (string, number, bool, null, object,
// array) starting at i, returning the index just past it. ok=false when
// the value extends beyond the prefix or is malformed.
func skipJSONValue(prefix []byte, i int) (int, bool) {
	switch prefix[i] {
	case '"':
		return scanJSONString(prefix, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(prefix); j++ {
			switch prefix[j] {
			case '"':
				k, ok := scanJSONString(prefix, j)
				if !ok {
					return 0, false
				}
				j = k - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return 0, false
	default:
		j := i
		for j < len(prefix) {
			c := prefix[j]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			j++
		}
		if j == i {
			return 0, false
		}
		return j, true
	}
}
