package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"onegw/internal/config"
	"onegw/internal/update"
)

// updateHandler serves /admin/update. It lives in the wiring layer (not
// internal/server) so the update feature stays a leaf package; main mounts
// it on a root-level mux that falls through to the server's handler.
//
//	GET  /admin/update → current status JSON (pid, current, latest…)
//	POST /admin/update → immediate check (default) or {"apply":true} handoff
//
// The GET doubles as the handoff probe: update.waitServing polls it and
// accepts only a 200 whose body names the spawned pid, which is evidence
// exactly the new process can produce during a SO_REUSEPORT overlap.
func updateHandler(svc *update.Service, liveCfg func() *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := liveCfg()
		got := r.Header.Get("X-Admin-Password")
		if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Server.AdminPassword)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(svc.Snapshot())
		case http.MethodPost:
			var body struct {
				Apply bool `json:"apply"`
				Force bool `json:"force"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body) // empty body = check only
			if !body.Apply {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(svc.CheckNow(r.Context()))
				return
			}
			if update.InContainer() {
				http.Error(w, `{"error":"container: update the image on the host (docker pull; docker compose up -d)"}`,
					http.StatusConflict)
				return
			}
			if err := svc.ApplyAsync(update.Opt{
				Force:         body.Force,
				Listen:        cfg.Server.Listen,
				AdminPassword: cfg.Server.AdminPassword,
			}); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"started":true}`))
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
