// Package server exposes the gateway HTTP surfaces: OpenAI, Anthropic, and
// Gemini compatibility endpoints plus admin/dashboard.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"onegw/internal/config"
	"onegw/internal/provider"
	"onegw/internal/quota"
	"onegw/internal/ratelimit"
	"onegw/internal/router"
	"onegw/internal/saver"
	"onegw/internal/store"
	"onegw/internal/translat"
	"onegw/internal/types"
	"onegw/internal/usage"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// state bundles everything a hot reload swaps as one atomic snapshot. A
// request loads it once and runs against it; in-flight requests finish on
// the old snapshot.
type state struct {
	cfg    *config.Config
	pool   *provider.Pool
	router *router.Router
	saver  *saver.Saver
	usage  *usage.Tracker
	quota  *quota.Tracker
	budget *ByteBudget
}

// Server wires the gateway together.
type Server struct {
	st     *store.Store
	nodeID string
	start  time.Time

	state    atomic.Pointer[state]
	inflight atomic.Int64 // requests currently live in the gateway pipeline
	// rl holds per-key rate-limit windows. It lives on the Server, not the
	// reloadable state, so SIGHUP does not reset in-progress windows.
	rl *ratelimit.Limiter
	m  *gatewayMetrics
}

// cur returns the active state snapshot (non-nil once New has run).
func (s *Server) cur() *state { return s.state.Load() }

// New builds the server from config. The store and its data dir are fixed
// at process start — moving data_dir needs a restart; everything else is
// swappable via apply/Reload.
func New(cfg *config.Config) (*Server, error) {
	var st *store.Store
	dataDir := cfg.Server.DataDir
	if dataDir != "" && dataDir != "memory" {
		var err error
		st, err = store.Open(dataDir + "/usage.db")
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
	}
	s := &Server{st: st, nodeID: nodeID(dataDir), start: time.Now(), rl: ratelimit.New(), m: newGatewayMetrics()}
	if st != nil {
		st.SetNodeID(s.nodeID)
	}
	if err := s.apply(cfg, true); err != nil {
		return nil, err
	}
	return s, nil
}

// apply (re)builds the mutable parts of the server from cfg and swaps them
// in as one atomic snapshot: provider pool, router tables, saver, auth
// keys, admin password, buffered-memory budget, and the usage flush
// interval.

// loopbackListen reports whether addr binds only loopback interfaces.
// An empty host (":port"), "0.0.0.0", or "::" is NOT loopback.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hasKey reports whether any usable client auth key is configured.
func hasKey(keys []config.AuthKey) bool {
	for _, k := range keys {
		if strings.TrimSpace(k.Key) != "" {
			return true
		}
	}
	return false
}

// apply (re)builds the mutable parts of the server from cfg and swaps them
// in as one atomic snapshot.
func (s *Server) apply(cfg *config.Config, initial bool) error {
	// Fail closed: a non-loopback listener with no auth keys is an open
	// proxy over every upstream account quota. Returning before the atomic
	// swap keeps the previous config live on reload; on startup it refuses
	// to start.
	if !loopbackListen(cfg.Server.Listen) && !hasKey(cfg.Auth.KeyList) {
		return fmt.Errorf("refusing to serve %q with no auth keys — set [auth] keys or bind a loopback address", cfg.Server.Listen)
	}
	pool := provider.NewPool()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		kind := provider.Kind(p.Kind)
		def := &provider.Def{
			Name:             p.Name,
			Kind:             kind,
			BaseURL:          p.BaseURL,
			MaxConc:          p.MaxConc,
			ExtraHeaders:     p.ExtraHeader,
			Models:           p.Models,
			AlwaysThinking:   p.AlwaysThinking,
			Passthrough:      p.Passthrough,
			SearchMaxResults: p.MaxResults,
			SearchTimeout:    provider.ParseSearchTimeout(p.Timeout),
		}
		if len(p.Accounts) > 0 {
			for _, a := range p.Accounts {
				def.Accounts = append(def.Accounts, provider.Account{
					Name: a.Name, APIKey: a.APIKey, BaseURL: a.BaseURL, Weight: a.Weight,
				})
			}
		} else {
			def.Accounts = []provider.Account{{Name: "default", APIKey: p.APIKey, BaseURL: p.BaseURL}}
		}
		pool.Set(def)
		// Materialize the kind's default catalog onto the config copy so
		// routing AND every surface that reads cfg.Providers (models list,
		// dashboard) agree.
		if len(p.Models) == 0 {
			p.Models = provider.DefaultModels(kind)
		}
	}
	rt := router.New(pool)
	for _, p := range cfg.Providers {
		rt.SetModels(p.Models)
	}
	var combos []*router.Combo
	for _, c := range cfg.Combos {
		targets := make([]router.Target, 0, len(c.Targets))
		for _, t := range c.Targets {
			prov, model, _ := strings.Cut(t, "/")
			targets = append(targets, router.Target{Provider: prov, Model: model})
		}
		combos = append(combos, &router.Combo{Name: c.Name, Targets: targets})
	}
	rt.SetCombos(combos)
	rt.SetAliases(cfg.Aliases)

	var sink usage.Sink
	if s.st != nil {
		sink = s.st
	}
	pusher := usage.NewPusher(cfg.Usage.ExportURL, cfg.Usage.ExportPassword, s.nodeID)
	usageTracker := usage.New(sink, cfg.FlushEvery(), pusher)

	// Quota windows (issue #7): per-provider limits from config; counters
	// seed from the store and inherit live state across hot reloads.
	limits := make(map[string]quota.Limits, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.QuotaWindow == "" {
			continue
		}
		l := quota.Limits{Window: p.QuotaWindow, LimitTokens: p.QuotaLimitTokens, LimitRequests: p.QuotaLimitRequests}
		if p.QuotaResetAnchor != "" {
			if a, err := time.Parse(time.RFC3339, p.QuotaResetAnchor); err == nil {
				l.Anchor = a
			}
		}
		limits[p.Name] = l
	}
	var quotaTracker *quota.Tracker
	if len(limits) > 0 {
		quotaTracker = quota.New(limits, s.st, cfg.FlushEvery())
		quotaTracker.Inherit(oldStateQuota(s.state.Load()))
	}

	old := s.state.Load()
	s.state.Store(&state{
		cfg:    cfg,
		pool:   pool,
		router: rt,
		saver:  saver.New(saver.Config{Enabled: cfg.Saver.Enabled}),
		usage:  usageTracker,
		quota:  quotaTracker,
		budget: NewByteBudget(cfg.Server.BufferCap),
	})
	if !initial && old != nil {
		old.usage.Stop() // flushes remaining data to the store, then ends the loop
		if old.quota != nil {
			old.quota.Stop()
		}
	}
	return nil
}

// oldStateQuota returns the previous snapshot's quota tracker, or nil.
func oldStateQuota(old *state) *quota.Tracker {
	if old == nil {
		return nil
	}
	return old.quota
}

// Reload hot-swaps configuration (SIGHUP). Bad config is rejected by the
// caller (config.Load) so this always applies a valid one.
func (s *Server) Reload(cfg *config.Config) {
	if err := s.apply(cfg, false); err != nil {
		log.Printf("onegw reload rejected: %v", err)
	}
}

// Close releases resources.
func (s *Server) Close() {
	if st := s.cur(); st != nil {
		st.usage.Stop()
		if st.quota != nil {
			st.quota.Stop()
		}
	}
	if s.st != nil {
		s.st.Close()
	}
}

// Handler builds the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleOpenAI)
	mux.HandleFunc("POST /v1/completions", s.handleOpenAI)
	mux.HandleFunc("POST /v1/messages", s.handleAnthropic)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /admin/usage/export", s.handleUsageExport)
	mux.HandleFunc("POST /admin/usage/import", s.handleUsageImport)
	mux.HandleFunc("POST /anthropic/v1/messages", s.handleAnthropic)
	mux.HandleFunc("POST /v1beta/models/", s.handleGemini)
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) { s.handlePassthrough(w, r, surfEmbeddings) })
	mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) { s.handlePassthrough(w, r, surfTranscriptions) })
	mux.HandleFunc("POST /v1/audio/speech", func(w http.ResponseWriter, r *http.Request) { s.handlePassthrough(w, r, surfSpeech) })
	mux.HandleFunc("GET /admin/health", s.handleHealth)
	mux.HandleFunc("GET /admin/usage", s.handleAdminUsage)
	mux.HandleFunc("GET /admin/quota", s.handleAdminQuota)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /", s.handleDashboard)
	return s.withRecovery(mux)
}

// ---------------------------------------------------------------------------
// Surfaces
// ---------------------------------------------------------------------------

func (s *Server) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, translat.FmtOpenAI)
}

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, translat.FmtAnthropic)
}

func (s *Server) handleGemini(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	model, method, ok := strings.Cut(rest, ":")
	if !ok || model == "" || r.Method != http.MethodPost {
		writeErr(w, translat.FmtGemini, errAPI(400, "invalid_request", "expected POST /v1beta/models/{model}:generateContent"))
		return
	}
	isStream := method == "streamGenerateContent"
	if method != "generateContent" && !isStream {
		writeErr(w, translat.FmtGemini, errAPI(404, "not_found", "unknown method "+method))
		return
	}
	if isStream && r.URL.Query().Get("alt") != "sse" {
		q := r.URL.Query()
		q.Set("alt", "sse")
		r.URL.RawQuery = q.Encode()
	}
	s.proxyGemini(w, r, model, isStream)
}

// proxyGemini mirrors proxy() for the Gemini surface.
func (s *Server) proxyGemini(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	ak, ok := s.authorize(w, r)
	if !ok {
		return
	}
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	release, ok := s.acquireForBody(r)
	if !ok {
		s.m.saturated()
		s.rejectSaturated(w, translat.FmtGemini)
		return
	}
	defer release()

	body, err := s.readBody(r)
	if err != nil {
		s.m.tooLarge()
		writeErr(w, translat.FmtGemini, errAPI(413, "body_too_large", err.Error()))
		return
	}
	if !s.enforceRateLimits(w, translat.FmtGemini, ak) {
		return
	}
	st := s.cur()
	var savedTokens int64
	if st.cfg.Saver.Enabled {
		body, savedTokens = st.saver.ApplyRaw(translat.FmtGemini, body)
	}
	res, rerr := st.router.Resolve(model)
	if rerr != nil {
		s.m.noRoute(rerr.Status)
		writeErr(w, translat.FmtGemini, rerr)
		return
	}
	if !s.enforceAllowlist(w, translat.FmtGemini, ak, model, res) {
		return
	}
	execErr := st.router.Execute(r.Context(), res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		return s.attempt(ctx, def, acct, m, translat.FmtGemini, body, stream, w, savedTokens, r.Header.Get(provider.OpenCodeSessionHeader), ak)
	}, func(v any) {})
	if execErr != nil && w.Header().Get("Content-Type") == "" {
		writeErr(w, translat.FmtGemini, execErr)
	}
}

// proxy is the main pipeline for OpenAI/Anthropic surfaces.
//
// Memory contract: buffered work (body read, saver, unified decode, any
// non-streaming translation) runs under the global byte budget; streaming
// passthrough runs outside it. Saturation returns 503 + Retry-After in the
// client's wire format.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, clientFmt translat.Format) {
	ak, ok := s.authorize(w, r)
	if !ok {
		return
	}
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	release, ok := s.acquireForBody(r)
	if !ok {
		s.m.saturated()
		s.rejectSaturated(w, clientFmt)
		return
	}
	defer release()

	body, err := s.readBody(r)
	if err != nil {
		s.m.tooLarge()
		writeErr(w, clientFmt, errAPI(413, "body_too_large", err.Error()))
		return
	}
	model := peekModel(body)
	stream := peekStream(body)
	if !s.enforceRateLimits(w, clientFmt, ak) {
		return
	}

	st := s.cur()
	var savedTokens int64
	if st.cfg.Saver.Enabled {
		body, savedTokens = st.saver.ApplyRaw(clientFmt, body)
	}

	res, rerr := st.router.Resolve(model)
	if rerr != nil {
		s.m.noRoute(rerr.Status)
		writeErr(w, clientFmt, rerr)
		return
	}
	if !s.enforceAllowlist(w, clientFmt, ak, model, res) {
		return
	}
	execErr := st.router.Execute(r.Context(), res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		return s.attempt(ctx, def, acct, m, clientFmt, body, stream, w, savedTokens, r.Header.Get(provider.OpenCodeSessionHeader), ak)
	}, func(v any) {})
	if execErr != nil && w.Header().Get("Content-Type") == "" {
		writeErr(w, clientFmt, execErr)
	}
}

// acquireForBody reserves budget for the request's declared content length
// (plus margin for decoded maps and response handling). Streaming requests
// also reserve: their request bodies are read fully here. Returns a release
// func; ok=false means the gateway is saturated.
func (s *Server) acquireForBody(r *http.Request) (func(), bool) {
	n := r.ContentLength
	if n < 0 {
		n = 0
	}
	// Margin covers the request pipeline's transient amplification: decoded
	// map (~2x), re-marshaled body (~1x), upstream write copy (~1x).
	if n > 0 {
		n += 3*n + 64<<10
	}
	st := s.cur()
	if err := st.budget.Acquire(r.Context(), n); err != nil {
		st.budget.Saturated()
		return nil, false
	}
	return func() { st.budget.Release(n) }, true
}

// rejectSaturated answers 503 with Retry-After in the client's format.
func (s *Server) rejectSaturated(w http.ResponseWriter, f translat.Format) {
	w.Header().Set("Retry-After", "2")
	writeErr(w, f, errAPI(503, "gateway_saturated", "onegw at buffered-memory capacity; retry shortly"))
}

// attempt performs one upstream call and returns the response to the
// client. Streaming replies are piped/translated event-by-event; a
// non-streaming cross-format reply takes the documented buffered path
// (parse whole response, translate, answer JSON). Usage is recorded.
func (s *Server) attempt(ctx context.Context, def *provider.Def, acct *provider.Account, model string,
	clientFmt translat.Format, body []byte, stream bool, w http.ResponseWriter, savedTokens int64, clientSession string, ak *config.AuthKey) (any, *types.APIError) {
	// mdl is the metrics label only: raw client model strings must not
	// create unbounded series (routing already used the original string).
	mdl := s.boundedModel(model)
	upstreamFmt := def.UpstreamFormat(model)
	// Quota enforcement (issue #7): an exhausted provider cools its whole
	// account pool until the window ends and answers 503 (retryable, so
	// combos fall through to the next target). Checked before the upstream
	// call so a direct hit never reaches the provider. Retry-After rides
	// on the error (written only if this error actually reaches the
	// client), never on w — a fallen-through attempt must not leak it.
	if q := s.cur().quota; q != nil {
		if st, ok := q.Status(def.Name, time.Now()); ok && st.Exhausted {
			cool := time.Until(st.WindowEnd)
			if cool < 0 {
				cool = 0
			}
			for i := range def.Accounts {
				def.Cool(&def.Accounts[i], cool)
			}
			return nil, &types.APIError{Status: 503, Type: "provider_quota_exhausted", Code: "quota_exceeded",
				RetryAfter: strconv.FormatInt(int64(cool.Seconds())+1, 10),
				Message: fmt.Sprintf("provider %s quota exhausted (%s window); resets %s",
					def.Name, st.Window, st.WindowEnd.UTC().Format(time.RFC3339))}
		}
	}
	upBody, err := prepareUpstreamBody(upstreamFmt, clientFmt, body, model, def)
	if err != nil {
		s.m.invalidBody(def.Name, mdl)
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: err.Error()}
	}
	res, apiErr := def.Do(ctx, acct, model, clientSession, upBody, stream)
	if apiErr != nil {
		s.m.upstreamErr(def.Name, mdl, apiErr.Status)
		return nil, apiErr
	}
	defer res.Resp.Body.Close()

	var rec types.Usage
	if !stream && upstreamFmt != clientFmt {
		// Buffered cross-format path (PRD "parse whole request, parse whole
		// response"). The reply bytes are accounted against the same global
		// byte semaphore as the request, capped at max_body_bytes; all
		// failures here are deterministic and non-retryable — a different
		// key returns the same oversized body, and retrying translate/encode
		// failures would only burn paid upstream calls.
		maxResp := s.cur().cfg.Server.MaxBody
		reserve := res.Resp.ContentLength
		if reserve < 0 || reserve > maxResp {
			reserve = maxResp
		}
		if err := s.cur().budget.Acquire(ctx, reserve); err != nil {
			s.cur().budget.Saturated()
			return nil, errAPI(503, "gateway_saturated", "onegw at buffered-memory capacity; retry shortly")
		}
		defer s.cur().budget.Release(reserve)
		raw, rerr := io.ReadAll(io.LimitReader(res.Resp.Body, maxResp+1))
		if rerr != nil {
			s.m.upstreamErr(def.Name, mdl, 502)
			return nil, errAPI(502, "upstream_read_failed", rerr.Error())
		}
		if int64(len(raw)) > maxResp {
			return nil, errAPI(413, "upstream_response_too_large", "response exceeds max_body_bytes")
		}
		cr, derr := translat.DecodeResponse(upstreamFmt, raw)
		if derr != nil {
			s.m.upstreamErr(def.Name, mdl, 501)
			return nil, errAPI(501, "response_translate_failed", derr.Error())
		}
		out, eerr := translat.EncodeResponse(clientFmt, cr)
		if eerr != nil {
			s.m.upstreamErr(def.Name, mdl, 501)
			return nil, errAPI(501, "response_encode_failed", eerr.Error())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		rec = cr.Usage
	} else {
		h := w.Header()
		if stream {
			h.Set("Content-Type", "text/event-stream; charset=utf-8")
			h.Set("Cache-Control", "no-cache")
			h.Set("Connection", "keep-alive")
			h.Set("X-Accel-Buffering", "no")
		} else {
			h.Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		flush()

		if upstreamFmt == clientFmt {
			sn := usage.NewSniffer(res.Resp.Body, 0)
			_, _ = io.Copy(w, sn)
			flush()
			in, out, cr, cw, rs, seen := sn.Usage()
			if seen {
				rec = types.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw, ReasoningTokens: rs}
			}
		} else {
			u, terr := translat.TranslateStream(res.Resp.Body, w, flush, upstreamFmt, clientFmt, model)
			if terr != nil {
				s.m.upstreamErr(def.Name, mdl, 502)
				return nil, &types.APIError{Status: 502, Type: "stream_translate_failed", Message: terr.Error()}
			}
			rec = u
		}
	}
	rec.UpstreamFormat = string(upstreamFmt)
	if rec.InputTokens == 0 && rec.OutputTokens == 0 {
		rec.Estimated = true
		rec.InputTokens = int64(len(body)) / 4
	}
	label := ""
	if ak != nil {
		label = ak.Label()
	}
	s.cur().usage.Observe(usage.Key{Provider: def.Name, Model: model, APIKey: label}, rec, savedTokens)
	s.observeTPM(ak, rec.InputTokens+rec.OutputTokens)
	if q := s.cur().quota; q != nil {
		q.Observe(def.Name, rec.InputTokens+rec.OutputTokens+rec.ReasoningTokens, 1, time.Now())
	}
	s.m.success(def.Name, mdl, rec, savedTokens)
	return nil, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// authorize authenticates the request and returns the matched key policy.
// ok=false means the 401 response was written. ak==nil with ok==true is
// the open gateway (no keys configured).
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*config.AuthKey, bool) {
	keys := s.cur().cfg.Auth.KeyList
	if len(keys) == 0 {
		return nil, true
	}
	auth := r.Header.Get("Authorization")
	key := strings.TrimPrefix(auth, "Bearer ")
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	for i := range keys {
		if keys[i].Key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(keys[i].Key)) == 1 {
			return &keys[i], true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	return nil, false
}

func (s *Server) readBody(r *http.Request) ([]byte, error) {
	maxBody := s.cur().cfg.Server.MaxBody
	r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBody {
		return nil, fmt.Errorf("body exceeds %d bytes", maxBody)
	}
	return b, nil
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				if w.Header().Get("Content-Type") == "" {
					writeErr(w, translat.FmtOpenAI, errAPI(500, "internal", fmt.Sprintf("panic: %v", rec)))
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var models []model
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, model{ID: id, Object: "model", OwnedBy: "onegw"})
	}
	cfg := s.cur().cfg
	for _, p := range cfg.Providers {
		if p.Kind == "searxng" {
			// Virtual search surface: any "<name>/<x>" model string
			// routes to it; advertise the canonical id so agent CLIs
			// can discover it via /v1/models.
			add(p.Name + "/query")
		}
		for _, m := range p.Models {
			add(p.Name + "/" + m)
		}
	}
	for _, c := range cfg.Combos {
		add(c.Name)
	}
	// Aliases surface as first-class model ids so clients can discover them.
	for a := range cfg.Aliases {
		add(a)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":        "ok",
		"uptime_s":      int(time.Since(s.start).Seconds()),
		"inflight":      s.inflight.Load(),
		"heap_alloc_mb": m.HeapAlloc >> 20,
		"heap_sys_mb":   m.HeapSys >> 20,
		"sys_mb":        m.Sys >> 20,
		"num_gc":        m.NumGC,
	})
}

func (s *Server) adminOK(r *http.Request) bool {
	pw := s.cur().cfg.Server.AdminPassword
	if pw == "" {
		return true
	}
	hpw := r.Header.Get("X-Admin-Password")
	return subtle.ConstantTimeCompare([]byte(hpw), []byte(pw)) == 1
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// source=store reads persisted rollups over ?days=N (default 1);
	// default reads the live since-last-flush window. Totals are computed
	// from the same rows as the table so the header and table can never
	// disagree (tracker totals are process-lifetime, not a time window).
	if r.URL.Query().Get("source") == "store" && s.st != nil {
		days := 1
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 366 {
				days = n
			}
		}
		from := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
		to := time.Now().UTC().Format("2006-01-02")
		rows, err := s.st.QueryRange(from, to)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"store query failed"}`))
			return
		}
		if rows == nil {
			rows = []store.UsageRow{}
		}
		var totReq, totIn, totOut, totSaved int64
		for _, r := range rows {
			totReq += r.Requests
			totIn += r.InputTok
			totOut += r.OutputTok
			totSaved += r.SavedTok
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rows":   rows,
			"totals": map[string]any{"requests": totReq, "input": totIn, "output": totOut, "saved": totSaved},
		})
		return
	}
}

// prepareUpstreamBody returns the body to send upstream. Same format →
// verbatim (with the model field rewritten to the routed upstream model);
// different format → full translate via the unified model. def (may be nil
// in tests) carries always-thinking adaptation for the routed model.
func prepareUpstreamBody(upstream, client translat.Format, body []byte, upstreamModel string, def *provider.Def) ([]byte, error) {
	if upstream == client {
		var err error
		body, err = rewriteModel(body, upstreamModel)
		if err != nil {
			return nil, err
		}
		body = adaptAlwaysThinking(body, upstreamModel, def)
		return normalizeRoles(body)
	}
	switch client {
	case translat.FmtOpenAI:
		u, err := translat.DecodeOpenAIRequest(body)
		if err != nil {
			return nil, err
		}
		u.Model = upstreamModel
		return encodeFor(upstream, u)
	case translat.FmtAnthropic:
		u, err := translat.DecodeAnthropicRequest(body)
		if err != nil {
			return nil, err
		}
		u.Model = upstreamModel
		return encodeFor(upstream, u)
	case translat.FmtGemini:
		u, err := translat.DecodeGeminiRequest(body)
		if err != nil {
			return nil, err
		}
		u.Model = upstreamModel // model arrives in the URL path on this surface
		return encodeFor(upstream, u)
	default:
		return rewriteModel(body, upstreamModel)
	}
}

// rewriteModel surgically replaces the top-level "model" string in a raw
// JSON body, preserving every other byte of structure (json.Number decode).
func rewriteModel(body []byte, model string) ([]byte, error) {
	if model == "" {
		return body, nil
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body, nil // not an object; forward verbatim
	}
	if cur, _ := root["model"].(string); cur == model || cur == "" {
		return body, nil
	}
	root["model"] = model
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// normalizeRoles maps OpenAI "developer" role messages to "system" for
// upstreams that predate the role (B.AI and friends reject "developer").
// Some clients also refuse `store: false`; it is dropped when present.
func normalizeRoles(body []byte) ([]byte, error) {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body, nil
	}
	changed := false
	if msgs, ok := root["messages"].([]any); ok {
		for _, mv := range msgs {
			m, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			if m["role"] == "developer" {
				m["role"] = "system"
				changed = true
			}
		}
	}
	if _, ok := root["store"]; ok {
		delete(root, "store")
		changed = true
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// coerceEffort maps reasoning_effort values an always-thinking upstream
// rejects onto the closest accepted one. GLM (error 1210) accepts only
// low|high|max: "none"/"minimal"/"medium" become "low"; other values pass
// through unchanged.
func coerceEffort(effort string) string {
	switch effort {
	case "none", "minimal", "medium":
		return "low"
	default:
		return effort
	}
}

// adaptAlwaysThinking rewrites disable-thinking knobs out of a raw
// same-format body when the routed model belongs to an always-thinking
// provider (def != nil and model matches AlwaysThinking globs). OpenAI
// dialect: reasoning_effort none|minimal|medium → low (GLM accepts only
// low|high|max); thinking{type:disabled} and enable_thinking:false are
// dropped so the upstream default (thinking on) applies. Knobs are never
// added — only explicit disable requests are rewritten. Returns body
// unchanged when not applicable.
func adaptAlwaysThinking(body []byte, model string, def *provider.Def) []byte {
	if def == nil || !def.AlwaysThinkingModel(model) {
		return body
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body // not an object; forward verbatim
	}
	changed := false
	if v, ok := root["reasoning_effort"]; ok {
		if s, ok := v.(string); ok {
			if c := coerceEffort(s); c != s {
				root["reasoning_effort"] = c
				changed = true
			}
		}
	}
	for _, key := range []string{"thinking", "enable_thinking"} {
		if v, ok := root[key]; ok {
			switch tv := v.(type) {
			case map[string]any:
				if t, _ := tv["type"].(string); t == "disabled" {
					delete(root, key)
					changed = true
				}
			case bool:
				if !tv {
					delete(root, key)
					changed = true
				}
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func encodeFor(f translat.Format, u *types.ChatRequest) ([]byte, error) {
	switch f {
	case translat.FmtOpenAI:
		return translat.EncodeOpenAIRequest(u)
	case translat.FmtAnthropic:
		return translat.EncodeAnthropicRequest(u)
	case translat.FmtGemini:
		return translat.EncodeGeminiRequest(u)
	case translat.FmtResponses:
		return translat.EncodeResponsesRequest(u)
	default:
		return nil, fmt.Errorf("unknown format %s", f)
	}
}

// peekModel extracts the model string without full parsing.
func peekModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		return probe.Model
	}
	return ""
}

func peekStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

func writeErr(w http.ResponseWriter, f translat.Format, e *types.APIError) {
	w.Header().Set("Content-Type", "application/json")
	if e.RetryAfter != "" {
		w.Header().Set("Retry-After", e.RetryAfter)
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(translat.EncodeError(f, e))
}

func errAPI(status int, typ, msg string) *types.APIError {
	return &types.APIError{Status: status, Type: typ, Message: msg}
}
