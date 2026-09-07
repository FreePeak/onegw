// Package server exposes the gateway HTTP surfaces: OpenAI, Anthropic, and
// Gemini compatibility endpoints plus admin/dashboard.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"onegw/internal/config"
	"onegw/internal/provider"
	"onegw/internal/router"
	"onegw/internal/saver"
	"onegw/internal/store"
	"onegw/internal/translat"
	"onegw/internal/types"
	"onegw/internal/usage"
	"strconv"
	"strings"
	"time"
)

// Server wires the gateway together.
type Server struct {
	cfg    *config.Config
	pool   *provider.Pool
	router *router.Router
	saver  *saver.Saver
	usage  *usage.Tracker
	st     *store.Store
	budget *ByteBudget
	start  time.Time
}

// New builds the server from config.
func New(cfg *config.Config) (*Server, error) {
	pool := provider.NewPool()
	for _, p := range cfg.Providers {
		kind := provider.Kind(p.Kind)
		def := &provider.Def{
			Name:         p.Name,
			Kind:         kind,
			BaseURL:      p.BaseURL,
			MaxConc:      p.MaxConc,
			ExtraHeaders: p.ExtraHeader,
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
	}
	var st *store.Store
	dataDir := cfg.Server.DataDir
	if dataDir != "" && dataDir != "memory" {
		var err error
		st, err = store.Open(dataDir + "/usage.db")
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
	}
	s := &Server{
		cfg:    cfg,
		pool:   pool,
		router: router.New(pool),
		saver:  saver.New(saver.Config{Enabled: cfg.Saver.Enabled}),
		st:     st,
		budget: NewByteBudget(cfg.Server.BufferCap),
		start:  time.Now(),
	}
	var sink usage.Sink
	if st != nil {
		sink = st
	}
	s.usage = usage.New(sink, cfg.FlushEvery())
	for _, p := range cfg.Providers {
		s.router.SetModels(p.Models)
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
	s.router.SetCombos(combos)
	return s, nil
}

// Close releases resources.
func (s *Server) Close() {
	s.usage.Stop()
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
	mux.HandleFunc("POST /anthropic/v1/messages", s.handleAnthropic)
	mux.HandleFunc("POST /v1beta/models/", s.handleGemini)
	mux.HandleFunc("GET /admin/health", s.handleHealth)
	mux.HandleFunc("GET /admin/usage", s.handleAdminUsage)
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

// proxy is the main pipeline for OpenAI/Anthropic surfaces.
//
// Memory contract: buffered work (body read, saver, unified decode, any
// non-streaming translation) runs under the global byte budget; streaming
// passthrough runs outside it. Saturation returns 503 + Retry-After in the
// client's wire format.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, clientFmt translat.Format) {
	if !s.authorize(w, r) {
		return
	}
	release, ok := s.acquireForBody(r)
	if !ok {
		s.rejectSaturated(w, clientFmt)
		return
	}
	defer release()

	body, err := s.readBody(r)
	if err != nil {
		writeErr(w, clientFmt, errAPI(413, "body_too_large", err.Error()))
		return
	}
	model := peekModel(body)
	stream := peekStream(body)

	if s.cfg.Saver.Enabled {
		body, _ = s.saver.ApplyRaw(clientFmt, body)
	}

	res, rerr := s.router.Resolve(model)
	if rerr != nil {
		writeErr(w, clientFmt, rerr)
		return
	}

	execErr := s.router.Execute(r.Context(), res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		return s.attempt(ctx, def, acct, m, clientFmt, body, stream, w)
	}, func(v any) {})
	if execErr != nil && w.Header().Get("Content-Type") == "" {
		writeErr(w, clientFmt, execErr)
	}
}

// proxyGemini mirrors proxy() for the Gemini surface.
func (s *Server) proxyGemini(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	if !s.authorize(w, r) {
		return
	}
	release, ok := s.acquireForBody(r)
	if !ok {
		s.rejectSaturated(w, translat.FmtGemini)
		return
	}
	defer release()

	body, err := s.readBody(r)
	if err != nil {
		writeErr(w, translat.FmtGemini, errAPI(413, "body_too_large", err.Error()))
		return
	}
	if s.cfg.Saver.Enabled {
		body, _ = s.saver.ApplyRaw(translat.FmtGemini, body)
	}
	res, rerr := s.router.Resolve(model)
	if rerr != nil {
		writeErr(w, translat.FmtGemini, rerr)
		return
	}
	execErr := s.router.Execute(r.Context(), res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		return s.attempt(ctx, def, acct, m, translat.FmtGemini, body, stream, w)
	}, func(v any) {})
	if execErr != nil && w.Header().Get("Content-Type") == "" {
		writeErr(w, translat.FmtGemini, execErr)
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
	// Margin covers saver/translation heap amplification (maps + strings).
	if n > 0 {
		n += n/2 + 16<<10
	}
	if err := s.budget.Acquire(r.Context(), n); err != nil {
		s.budget.Saturated()
		return nil, false
	}
	return func() { s.budget.Release(n) }, true
}

// rejectSaturated answers 503 with Retry-After in the client's format.
func (s *Server) rejectSaturated(w http.ResponseWriter, f translat.Format) {
	w.Header().Set("Retry-After", "2")
	writeErr(w, f, errAPI(503, "gateway_saturated", "onegw at buffered-memory capacity; retry shortly"))
}

// attempt performs one upstream call and streams the response back,
// translating or passing through as needed. Usage is recorded.
func (s *Server) attempt(ctx context.Context, def *provider.Def, acct *provider.Account, model string,
	clientFmt translat.Format, body []byte, stream bool, w http.ResponseWriter) (any, *types.APIError) {

	upstreamFmt := def.Kind.Format()
	upBody, err := prepareUpstreamBody(upstreamFmt, clientFmt, body)
	if err != nil {
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: err.Error()}
	}
	res, apiErr := def.Do(ctx, acct, model, upBody, stream)
	if apiErr != nil {
		return nil, apiErr
	}
	defer res.Resp.Body.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	var rec types.Usage
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
			return nil, &types.APIError{Status: 502, Type: "stream_translate_failed", Message: terr.Error()}
		}
		rec = u
	}
	rec.UpstreamFormat = string(upstreamFmt)
	if rec.InputTokens == 0 && rec.OutputTokens == 0 {
		rec.Estimated = true
		rec.InputTokens = int64(len(body)) / 4
	}
	s.usage.Observe(usage.Key{Provider: def.Name, Model: model}, rec, 0)
	return nil, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if len(s.cfg.Auth.Keys) == 0 {
		return true
	}
	auth := r.Header.Get("Authorization")
	key := strings.TrimPrefix(auth, "Bearer ")
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	for _, k := range s.cfg.Auth.Keys {
		if k != "" && key == k {
			return true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	return false
}

func (s *Server) readBody(r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, s.cfg.Server.MaxBody)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > s.cfg.Server.MaxBody {
		return nil, fmt.Errorf("body exceeds %d bytes", s.cfg.Server.MaxBody)
	}
	return b, nil
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if w.Header().Get("Content-Type") == "" {
					writeErr(w, translat.FmtOpenAI, errAPI(500, "internal", fmt.Sprintf("panic: %v", rec)))
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
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
	for _, p := range s.cfg.Providers {
		for _, m := range p.Models {
			add(p.Name + "/" + m)
		}
	}
	for _, c := range s.cfg.Combos {
		add(c.Name)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}

func (s *Server) adminOK(r *http.Request) bool {
	pw := s.cfg.Server.AdminPassword
	if pw == "" {
		return true
	}
	return r.URL.Query().Get("password") == pw || r.Header.Get("X-Admin-Password") == pw
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
	// source=store reads persisted rollups over ?days=N (default 1);
	// default reads the live since-last-flush window.
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
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows})
		return
	}
	snap := s.usage.Snapshot()
	if snap == nil {
		snap = []usage.Bucket{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"buckets": snap})
}

// prepareUpstreamBody returns the body to send upstream. Same format →
// verbatim. Different format → full translate via the unified model.
func prepareUpstreamBody(upstream, client translat.Format, body []byte) ([]byte, error) {
	if upstream == client {
		return body, nil
	}
	switch client {
	case translat.FmtOpenAI:
		u, err := translat.DecodeOpenAIRequest(body)
		if err != nil {
			return nil, err
		}
		return encodeFor(upstream, u)
	case translat.FmtAnthropic:
		u, err := translat.DecodeAnthropicRequest(body)
		if err != nil {
			return nil, err
		}
		return encodeFor(upstream, u)
	case translat.FmtGemini:
		u, err := translat.DecodeGeminiRequest(body)
		if err != nil {
			return nil, err
		}
		return encodeFor(upstream, u)
	default:
		return body, nil
	}
}

func encodeFor(f translat.Format, u *types.ChatRequest) ([]byte, error) {
	switch f {
	case translat.FmtOpenAI:
		return translat.EncodeOpenAIRequest(u)
	case translat.FmtAnthropic:
		return translat.EncodeAnthropicRequest(u)
	case translat.FmtGemini:
		return translat.EncodeGeminiRequest(u)
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
	w.WriteHeader(e.Status)
	_, _ = w.Write(translat.EncodeError(f, e))
}

func errAPI(status int, typ, msg string) *types.APIError {
	return &types.APIError{Status: status, Type: typ, Message: msg}
}
