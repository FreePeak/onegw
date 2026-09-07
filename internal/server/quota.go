package server

// Quota admin surface (issue #7): per-provider reset-window countdown and
// limit status. Kept separate from handleAdminUsage to leave the usage
// endpoint untouched.

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleAdminQuota answers GET /admin/quota with the live per-provider
// quota window status.
func (s *Server) handleAdminQuota(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	var providers []any
	if q := s.cur().quota; q != nil {
		for _, st := range q.All(time.Now()) {
			providers = append(providers, st)
		}
	}
	if providers == nil {
		providers = []any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"providers": providers})
}
