package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"onegw/internal/idempotency"
	"onegw/internal/translat"
)

// withIdempotency wraps one chat surface handler with request dedup. Only
// requests that opt in with an Idempotency-Key (or X-Request-Id fallback)
// header participate: every other request pays a single header lookup and
// runs untouched.
//
// The middleware runs above authorize (which writes its own 401 inside the
// handler), so it scopes entries by a hash of the raw credential rather
// than by the matched key's label — same client namespace isolation,
// without touching the auth path.
func (s *Server) withIdempotency(clientFmt translat.Format, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.cur()
		if st == nil || st.ido == nil {
			next(w, r)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			key = strings.TrimSpace(r.Header.Get("X-Request-Id"))
		}
		if key == "" {
			next(w, r)
			return
		}
		// Participating requests are hashed up front, so the body is read
		// here and restored for the handler (one extra transient copy,
		// bounded by max_body_bytes, for opt-in requests only).
		body, ok := s.idempotencyBody(w, r)
		if !ok {
			next(w, r) // read failure: the handler answers with its usual 413
			return
		}
		scope := idempotency.ScopeFor(credentialOf(r))
		hash := idempotency.BodyHash(body)
		cache := st.ido

		for attempt := 0; ; attempt++ {
			decision, res, waiter := cache.Claim(scope, key, hash)
			switch decision {
			case idempotency.Wait:
				got := waiter.Wait(r.Context())
				if got == nil {
					// The client's deadline expired while coalescing:
					// run the handler unrecorded; its upstream call
					// aborts immediately on the dead context.
					next(w, r)
					return
				}
				if got.Gone {
					if attempt == 0 {
						continue // origin vanished: re-claim and execute
					}
					next(w, r)
					return
				}
				serveIdempotentResult(w, clientFmt, got)
				return
			case idempotency.Replay:
				serveIdempotentResult(w, clientFmt, res)
				return
			case idempotency.Conflict:
				serveIdempotentResult(w, clientFmt, &idempotency.Result{Stream: true})
				return
			case idempotency.Miss:
				rec := &idemRecorder{ResponseWriter: w}
				defer func() { cache.Finish(scope, key, hash, rec.result()) }()
				next(rec, r)
				return
			default:
				serveIdempotentResult(w, clientFmt, res)
				return
			}
		}
	}
}

// idempotencyBody reads the request body for hashing, then restores it for
// the handler. ok=false means the body could not be read within
// max_body_bytes: the body is left in an erroring state so readBody below
// surfaces the usual 413 instead of silently dropping bytes.
func (s *Server) idempotencyBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	maxBody := s.cur().cfg.Server.MaxBody
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err == nil && int64(len(body)) > maxBody {
		err = fmt.Errorf("body exceeds %d bytes", maxBody)
	}
	if err != nil {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errorReader{err}))
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

// errorReader replays a body-read failure to the handler.
type errorReader struct{ err error }

func (e errorReader) Read([]byte) (int, error) { return 0, e.err }

// credentialOf extracts the client credential exactly like authorize does,
// so the idempotency scope namespaces clients the same way auth does.
func credentialOf(r *http.Request) string {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	if key == "" {
		key = r.Header.Get("x-goog-api-key")
	}
	return key
}

// serveIdempotentResult answers a replay: the recorded status, body and
// content-type for a finished non-streaming response. A stream-shaped
// original answers 409 — recording stream bodies would blow the gateway's
// RAM budget, so the honest bound is to make the client retry with a fresh
// key.
func serveIdempotentResult(w http.ResponseWriter, clientFmt translat.Format, res *idempotency.Result) {
	if res.Stream {
		writeErr(w, clientFmt, errAPI(http.StatusConflict, "idempotency_conflict",
			"this Idempotency-Key already produced a streaming response that cannot be replayed; retry with a new Idempotency-Key"))
		return
	}
	if res.ContentType != "" {
		w.Header().Set("Content-Type", res.ContentType)
	}
	w.Header().Set("Idempotency-Replayed", "true")
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

// idemRecorder buffers a participant's response for replay while passing
// every byte through to the client unchanged. It stops buffering and marks
// the entry stream-shaped on the first event-stream flush or when the
// buffered body would exceed EntryBodyCap.
//
// A flush alone is not a stream signal: relayResponse flushes
// unconditionally even on a same-format JSON reply, so the event-stream
// content type is what distinguishes the two.
type idemRecorder struct {
	http.ResponseWriter
	status int
	ctype  string
	wrote  bool
	sse    bool // an event-stream flush was observed
	over   bool // body grew past EntryBodyCap
	buf    bytes.Buffer
}

func (rc *idemRecorder) WriteHeader(code int) {
	if rc.wrote {
		return
	}
	rc.status = code
	rc.ctype = rc.Header().Get("Content-Type")
	rc.wrote = true
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *idemRecorder) Write(p []byte) (int, error) {
	if !rc.wrote {
		rc.WriteHeader(http.StatusOK)
	}
	if !rc.sse && !rc.over {
		if rc.buf.Len()+len(p) > idempotency.EntryBodyCap {
			rc.over = true
			rc.buf.Reset()
		} else {
			rc.buf.Write(p)
		}
	}
	return rc.ResponseWriter.Write(p)
}

// Flush forwards to the underlying writer (streaming latency must not
// change) and marks the entry stream-shaped when the response is an event
// stream.
func (rc *idemRecorder) Flush() {
	if !rc.wrote {
		rc.WriteHeader(http.StatusOK)
	}
	if strings.HasPrefix(rc.ctype, "text/event-stream") {
		rc.sse = true
		rc.buf.Reset()
	}
	if f, ok := rc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// result snapshots the response for the cache; nil means nothing was
// written (the entry is dropped so waiters execute themselves).
func (rc *idemRecorder) result() *idempotency.Result {
	if !rc.wrote {
		return nil
	}
	if rc.sse || rc.over {
		return &idempotency.Result{Status: rc.status, Stream: true}
	}
	body := make([]byte, rc.buf.Len())
	copy(body, rc.buf.Bytes())
	return &idempotency.Result{Status: rc.status, ContentType: rc.ctype, Body: body}
}
