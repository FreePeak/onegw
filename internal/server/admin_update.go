package server

// Dashboard update endpoints (#61): the Settings page's Version card talks
// to these JSON routes behind the same admin gate as every other admin
// surface (X-Admin-Password header or session cookie). /admin/update in
// cmd/onegw stays the header-auth CLI + handoff-probe surface; both share
// the same internal/update service, so the apply path — download, verify,
// swap, zero-drop handoff — is the code the CLI and auto-apply already
// exercise. In a container the apply is refused with the host-side
// pull/recreate guidance (the image owns the filesystem).

import (
	"encoding/json"
	"io"
	"net/http"

	"onegw/internal/update"
)

// SetUpdater wires the gateway's update service into the dashboard. Called
// once by main; without it the endpoints answer 409 "no updater
// configured".
func (s *Server) SetUpdater(svc *update.Service) {
	if svc != nil {
		s.upd.Store(&svc)
	}
}

func (s *Server) updater() *update.Service {
	if p := s.upd.Load(); p != nil {
		return *p
	}
	return nil
}

// handleUpdateStatus serves GET /admin/api/v1/update: the live update
// status snapshot (current/latest tag, last check/apply outcome, pid,
// container flag). Read-only; no network on this path — it reports what
// the background checker last learned, and the card renders it on load.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	svc := s.updater()
	if svc == nil {
		adminError(w, http.StatusConflict, "no updater configured")
		return
	}
	writeJSON(w, svc.Snapshot())
}

// handleUpdateApply serves POST /admin/api/v1/update. An empty body or
// {"apply":false} runs an immediate release check (GitHub call) and
// returns the fresh status. {"apply":true} starts the zero-drop handoff
// and answers 202 — the browser then polls the GET: a changed pid (or a
// fresh 401 from the new process's empty session table) means the new
// binary owns the port; a failed apply keeps the old gateway serving and
// surfaces the reason in last_apply.
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	svc := s.updater()
	if svc == nil {
		adminError(w, http.StatusConflict, "no updater configured")
		return
	}
	var body struct {
		Apply bool `json:"apply"`
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) // empty body = check only
	if !body.Apply {
		writeJSON(w, svc.CheckNow(r.Context()))
		return
	}
	st := svc.Snapshot()
	if st.InContainer {
		tag := st.Latest
		if tag == "" {
			tag = "latest"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":    "container: update the image on the host",
			"guidance": update.ContainerGuidance(&update.Release{Tag: tag}),
		})
		return
	}
	cfg := s.cur().cfg
	if err := svc.ApplyAsync(update.Opt{
		Force:         body.Force,
		Listen:        cfg.Server.Listen,
		AdminPassword: cfg.Server.AdminPassword,
	}); err != nil {
		adminError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"started":true}`))
}
