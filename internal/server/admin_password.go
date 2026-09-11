package server

// PUT /admin/config/password — change the admin password from the dashboard
// (Settings → Admin password) or via the API with X-Admin-Password.
//
// Contract:
//   - The caller must re-prove the CURRENT password (a stolen session cookie
//     alone cannot lock the owner out of their own gateway).
//   - An empty new_password generates a fresh random credential (the UI's
//     "generate" path); the generated value is echoed back once so the
//     operator can copy it before signing in again.
//   - The new password is spliced into the TOML (admin_password key) and
//     applied through patchConfigFile — the same validate + atomic write +
//     Load+Reload path as SIGHUP, so it takes effect immediately.
//   - Mirrored into <data_dir>/admin_password so a container recreated from
//     a config whose key is empty picks the value back up from the volume.
//   - Every session dies with the change: tokens are bound to the sha256 of
//     the password they were issued for, so this browser's cookie is
//     expired and all other sessions are dropped — everyone signs in again
//     with the new password.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"onegw/internal/config"
)

type adminPasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"` // empty = generate one
}

func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req adminPasswordReq
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	cur := s.cur().cfg.Server.AdminPassword
	if subtle.ConstantTimeCompare([]byte(req.CurrentPassword), []byte(cur)) != 1 {
		adminError(w, http.StatusUnauthorized, "current password does not match")
		return
	}
	generated := false
	next := req.NewPassword
	if next == "" {
		if next, err = config.GenerateAdminPassword(); err != nil {
			adminError(w, http.StatusInternalServerError, "generate password: "+err.Error())
			return
		}
		generated = true
	} else if len(next) < 8 {
		adminError(w, http.StatusBadRequest, "new password must be at least 8 characters (or leave it empty to generate one)")
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	// Persist in the config file when that is possible, and always mirror
	// into <data_dir>/admin_password. Either one alone survives a restart:
	// an explicit config key outranks the file, and the file is what a
	// config-less boot (main.go's defaults branch) or a read-only config
	// mount (docker `-v cfg:/etc/onegw:ro`) has to fall back on.
	persistedTOML := false
	if path := s.configPath(); path != "" && fileExists(path) {
		if _, err := s.patchConfigFile(func(lines []string) ([]string, error) {
			return spliceAdminPassword(lines, next)
		}); err == nil {
			persistedTOML = true
		} else {
			var bad badConfigEdit
			if errors.As(err, &bad) {
				editFailed(w, err) // the operator's content was rejected: nothing changed
				return
			}
			// Unwritable config (read-only mount, permissions): keep going —
			// the mirror below carries the change across restarts.
			log.Printf("admin: config file not updated (%v); falling back to the %s file", err, config.AdminPasswordFile)
		}
	}
	if !persistedTOML {
		// Swap in memory through the same Reload path, so the new password is
		// live for this process either way.
		st := s.cur()
		fresh := *st.cfg
		fresh.Server.AdminPassword = next
		s.Reload(&fresh)
	}
	// The mirror always tracks the effective password, so a container
	// recreated from an image config whose key is empty comes back with the
	// operator's credential from the data volume.
	if err := config.WriteAdminPasswordFile(s.dataDir, next); err != nil {
		log.Printf("admin: password changed but the %s mirror failed: %v", config.AdminPasswordFile, err)
	}
	persistedWhere := "marker"
	if persistedTOML {
		persistedWhere = "config"
	}
	s.sessions.clear() // every issued token died with the old password; free the table
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	log.Printf("admin: admin_password changed (generated=%v, persisted=%s); all admin sessions invalidated", generated, persistedWhere)
	resp := map[string]any{"changed": true, "generated": generated, "persisted": persistedWhere}
	if generated {
		// Echo the credential once so the operator can copy it before
		// signing back in; a password the operator chose is not reflected.
		resp["password"] = next
	}
	writeJSON(w, resp)
}

// spliceAdminPassword rewrites the admin_password key of the [server] table,
// opening a [server] table at the top of the file when the config has none
// (keys in TOML's default table are not the server settings, so an insert
// there would be dead config). Every other line — comments, other tables,
// nested [[providers.accounts]] — is carried over byte-for-byte; the
// caller's patchConfigFile round-trip validates the result before the write.
func spliceAdminPassword(lines []string, pw string) ([]string, error) {
	if strings.TrimSpace(pw) == "" {
		return nil, errors.New("refusing to write an empty admin_password")
	}
	rendered := tsv("admin_password", pw)

	serverAt, insertAt := -1, 0
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "[server]" {
			serverAt, insertAt = i, i+1
			break
		}
	}
	if serverAt < 0 {
		out := make([]string, 0, len(lines)+3)
		out = append(out, "[server]", rendered, "")
		return append(out, lines...), nil
	}
	// Inside [server]: rewrite an existing key, otherwise insert after the
	// header. The table ends at the next header line.
	for i := serverAt + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "[") {
			break
		}
		if strings.HasPrefix(t, "admin_password") && strings.Contains(t, "=") {
			out := cloneLines(lines)
			out[i] = rendered
			return out, nil
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, rendered)
	return append(out, lines[insertAt:]...), nil
}

// fileExists reports whether path names an existing file (a config path the
// process was started with may simply never have been written).
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
