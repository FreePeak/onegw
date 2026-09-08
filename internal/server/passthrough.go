// Passthrough surfaces: narrow OpenAI-format endpoints served verbatim
// without translation (issue #9). Providers opt in per surface via the
// `passthrough` config list ("embeddings", "stt", "tts"); requests whose
// routed provider lacks the capability are refused. Bodies are relayed
// byte-for-byte — multipart STT uploads stream, never fully buffered —
// and JSON requests share the chat path's byte-budget gating. Only the
// model selector is rewritten so "provider/model" strings never leak.
package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"onegw/internal/provider"
	"onegw/internal/router"
	"onegw/internal/translat"
	"onegw/internal/types"
	"onegw/internal/usage"
)

// surface describes one passthrough endpoint: op is the upstream path token
// (provider.Def.Path), cap the config capability marker.
type surface struct {
	op  string
	cap string
}

var (
	surfEmbeddings     = surface{op: "embeddings", cap: "embeddings"}
	surfTranscriptions = surface{op: "transcriptions", cap: "stt"}
	surfSpeech         = surface{op: "speech", cap: "tts"}
)

// multipartPeekLimit bounds the leading bytes of a streamed multipart body
// inspected for the "model" field. The peek is replayed upstream verbatim,
// so the audio part never enters gateway memory beyond the copy buffer.
const multipartPeekLimit = 8 << 10

// handlePassthrough is wired for POST /v1/embeddings, /v1/audio/transcriptions
// and /v1/audio/speech. Routing reuses the shared resolver ("provider/model"
// direct, combo, bare model); combo targets that lack the surface capability
// are dropped so fallback skips them.
func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request, sf surface) {
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

	st := s.cur()

	// The model selector routes; the body relays apart from it.
	// embeddings/speech: buffered JSON, model from the body.
	// transcriptions: multipart streamed byte-for-byte — only a bounded
	// prefix is peeked to find the model field, then replayed upstream.
	var (
		model       string
		body        []byte
		mp          *multipartPeek
		contentType = "application/json"
	)
	if sf == surfTranscriptions {
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request", "multipart/form-data content type required"))
			return
		}
		contentType = ct // the boundary must reach the upstream intact
		mp = splitMultipartModel(r.Body)
		if mp == nil {
			writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request", "unreadable multipart body"))
			return
		}
		model = mp.model
	} else {
		b, err := s.readBody(r)
		if err != nil {
			writeErr(w, translat.FmtOpenAI, errAPI(413, "body_too_large", err.Error()))
			return
		}
		body = b
		model = peekModel(b)
	}

	res, rerr := st.router.Resolve(model)
	if rerr != nil {
		writeErr(w, translat.FmtOpenAI, rerr)
		return
	}
	if res.IsCombo {
		kept := res.Targets[:0]
		for _, t := range res.Targets {
			if def, ok := st.pool.Get(t.Provider); ok && def.AllowsPassthrough(sf.cap) {
				kept = append(kept, t)
			}
		}
		res.Targets = kept
		if len(kept) == 0 {
			writeErr(w, translat.FmtOpenAI, errAPI(http.StatusNotFound, "passthrough_not_supported",
				"no provider in combo serves passthrough \""+sf.cap+"\""))
			return
		}
	}

	// Streaming multipart cannot be replayed for a retry or fallback, so
	// those requests get exactly one attempt against one target; buffered
	// JSON bodies take the router's normal retry/fallback path.
	if sf == surfTranscriptions {
		if res.IsCombo || len(res.Targets) != 1 {
			writeErr(w, translat.FmtOpenAI, errAPI(400, "invalid_request",
				"audio transcriptions requires a single provider/model route (streamed bodies cannot be replayed)"))
			return
		}
		t := res.Targets[0]
		def, ok := st.pool.Get(t.Provider)
		if !ok {
			writeErr(w, translat.FmtOpenAI, errAPI(404, "unknown_provider", "unknown provider "+t.Provider))
			return
		}
		if !def.AllowsPassthrough(sf.cap) {
			writeErr(w, translat.FmtOpenAI, errAPI(http.StatusNotFound, "passthrough_not_supported",
				"provider "+def.Name+" does not declare passthrough \""+sf.cap+"\""))
			return
		}
		// Mirror the JSON path: splice the routed model into the streamed
		// prefix so "provider/model" never leaks upstream.
		mp.retarget(t.Model)
		id := requestIdentity(r, ak)
		aerr := s.passthroughCall(r.Context(), w, def, def.NextAccount(id), sf, t.Model,
			mp.replay(), mp.length(r.ContentLength), contentType)
		if aerr != nil {
			def.Unpin(id) // failed one-shot attempt must not keep its pin
			if w.Header().Get("Content-Type") == "" {
				writeErr(w, translat.FmtOpenAI, aerr)
			}
		}
		return
	}

	execErr := st.router.Execute(router.WithIdentity(r.Context(), requestIdentity(r, ak)), res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		if !def.AllowsPassthrough(sf.cap) {
			return nil, errAPI(http.StatusNotFound, "passthrough_not_supported",
				"provider "+def.Name+" does not declare passthrough \""+sf.cap+"\"")
		}
		// Rewrite the routed model into the JSON body (surgical: every other
		// byte is preserved) so the client's "provider/model" never leaks.
		out, _ := rewriteModel(body, m)
		return nil, s.passthroughCall(ctx, w, def, acct, sf, m, bytes.NewReader(out), int64(len(out)), contentType)
	}, func(v any) {})
	if execErr != nil && w.Header().Get("Content-Type") == "" {
		writeErr(w, translat.FmtOpenAI, execErr)
	}
}

// passthroughCall performs one upstream attempt for a passthrough surface and
// relays the response. Returns nil on success (response already written); on
// upstream failure before any byte was written it returns the error so the
// router can fall back.
func (s *Server) passthroughCall(ctx context.Context, w http.ResponseWriter, def *provider.Def,
	acct *provider.Account, sf surface, m string, src io.Reader, srcLen int64, contentType string) *types.APIError {

	resp, apiErr := def.DoPassthrough(ctx, acct, sf.op, m, contentType, src, srcLen)
	if apiErr != nil {
		return apiErr
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Relay the upstream error payload verbatim (JSON or text) and let
		// the router see the failure (4xx never retries).
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(limited)
		return &types.APIError{Status: resp.StatusCode, Type: "upstream_error", Message: strings.TrimSpace(string(limited))}
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(resp.StatusCode)

	var rec types.Usage
	if sf == surfSpeech {
		// Binary audio: stream untouched, no sniffing. Requests are counted.
		_, _ = io.Copy(w, resp.Body)
		rec = types.Usage{Estimated: true}
	} else {
		// JSON (embeddings, transcriptions): sniff usage while relaying.
		sn := usage.NewSniffer(resp.Body, 0)
		_, _ = io.Copy(w, sn)
		in, out, cr, cw, rs, seen := sn.Usage()
		if seen {
			rec = types.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw, ReasoningTokens: rs}
		} else {
			rec = types.Usage{Estimated: true, InputTokens: srcLen / 4}
		}
	}
	rec.UpstreamFormat = string(def.Kind.Format())
	s.cur().usage.Observe(usage.Key{Provider: def.Name, Model: m}, rec, 0)
	return nil
}

// multipartPeek is a streamed multipart body split into a bounded inspected
// prefix (containing the model field) and the untouched remainder. The two
// are replayed as one stream, so the audio part is never buffered; retarget
// splices the routed model into the prefix only.
type multipartPeek struct {
	prefix   []byte
	origLen  int
	rest     io.Reader
	valueOff int // model value span within prefix
	valueEnd int
	model    string
}

// splitMultipartModel peeks up to multipartPeekLimit bytes and locates the
// `model` form field. Returns nil when the body is unreadable.
func splitMultipartModel(body io.Reader) *multipartPeek {
	br := bufio.NewReaderSize(body, multipartPeekLimit)
	prefix, _ := br.Peek(multipartPeekLimit) // shorter prefix at EOF is fine
	if len(prefix) == 0 {
		return nil
	}
	if _, err := br.Discard(len(prefix)); err != nil {
		return nil
	}
	off, end := findMultipartModel(prefix)
	if off < 0 {
		return &multipartPeek{prefix: prefix, origLen: len(prefix), rest: br}
	}
	model := strings.TrimSpace(string(prefix[off:end]))
	return &multipartPeek{prefix: prefix, origLen: len(prefix), rest: br, valueOff: off, valueEnd: end, model: model}
}

// retarget replaces the model field value inside the inspected prefix.
func (m *multipartPeek) retarget(model string) {
	if m.valueOff < 0 || model == "" || model == m.model {
		return
	}
	next := make([]byte, 0, len(m.prefix)-(m.valueEnd-m.valueOff)+len(model))
	next = append(next, m.prefix[:m.valueOff]...)
	next = append(next, model...)
	next = append(next, m.prefix[m.valueEnd:]...)
	m.prefix = next
	m.valueOff = -1 // consumed: a second retarget must not corrupt the splice
	m.model = model
}

// replay yields the full original (or retargeted) body as one stream.
func (m *multipartPeek) replay() io.Reader {
	return io.MultiReader(bytes.NewReader(m.prefix), m.rest)
}

// length adjusts the declared body size for a retarget splice.
func (m *multipartPeek) length(base int64) int64 {
	if base < 0 {
		return -1
	}
	return base + int64(len(m.prefix)-m.origLen)
}

// findMultipartModel returns the byte span of the `model` field value in a
// multipart prefix, or (-1, -1) when absent or truncated.
func findMultipartModel(prefix []byte) (off, end int) {
	const marker = `name="model"`
	i := bytes.Index(prefix, []byte(marker))
	if i < 0 {
		return -1, -1
	}
	tail := prefix[i+len(marker):]
	const term = "\r\n\r\n"
	j := bytes.Index(tail, []byte(term))
	if j < 0 {
		return -1, -1
	}
	start := i + len(marker) + j + len(term)
	val := prefix[start:]
	if e := bytes.Index(val, []byte("\r\n")); e >= 0 {
		return start, start + e
	}
	return -1, -1 // value runs past the peek window: treat as absent
}
