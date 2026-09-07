package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"onegw/internal/config"
	"onegw/internal/router"
	"onegw/internal/translat"
)

// Per-key policy enforcement (issue #3): model allowlist, rpm, tpm.
//
// rpm is enforced at request time: one slot of the key's sliding 60s
// window is consumed before any upstream work; denial answers 429 with
// Retry-After.
//
// tpm cannot be known before the upstream answers (input tokens are only
// reported back, and output tokens do not exist yet), so it is tracked
// retroactively: each completed request records its observed input+output
// tokens into the key's sliding 60s window, and the NEXT request is
// pre-checked against that window. A request already in flight is never
// cancelled or rejected after the fact — a burst can overshoot the token
// budget by the concurrent in-flight volume once per window.

// enforceRateLimits checks rpm (consuming a slot when allowed) and the tpm
// window. Returns false after writing the 429 response.
func (s *Server) enforceRateLimits(w http.ResponseWriter, clientFmt translat.Format, ak *config.AuthKey) bool {
	if ak == nil {
		return true
	}
	label := ak.Label()
	if ak.RPM > 0 {
		if ok, retry := s.rl.AllowRPM(label, ak.RPM); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeErr(w, clientFmt, errAPI(429, "rate_limited",
				fmt.Sprintf("key %q exceeds %d requests per minute", label, ak.RPM)))
			return false
		}
	}
	if ak.TPM > 0 {
		if blocked, retry := s.rl.TPMBlocked(label, ak.TPM); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeErr(w, clientFmt, errAPI(429, "rate_limited",
				fmt.Sprintf("key %q exceeds %d tokens per minute", label, ak.TPM)))
			return false
		}
	}
	return true
}

// observeTPM records a completed request's tokens into the key's tpm
// window. Called from the usage hook; no-op for unlimited keys.
func (s *Server) observeTPM(ak *config.AuthKey, tokens int64) {
	if ak == nil || ak.TPM <= 0 || tokens <= 0 {
		return
	}
	s.rl.ObserveTPM(ak.Label(), ak.TPM, tokens)
}

// modelAllowed reports whether ak may use the requested model. An empty
// allowlist (or the open gateway) allows everything. An entry matches the
// client's model string, or any resolved target as "provider/model" or
// bare model — so combo names, direct routes, and bare models all work.
func modelAllowed(ak *config.AuthKey, model string, res *router.Resolution) bool {
	if ak == nil || len(ak.Models) == 0 {
		return true
	}
	for _, e := range ak.Models {
		if strings.EqualFold(e, model) {
			return true
		}
		if res == nil {
			continue
		}
		for _, t := range res.Targets {
			if strings.EqualFold(e, t.Provider+"/"+t.Model) || strings.EqualFold(e, t.Model) {
				return true
			}
		}
	}
	return false
}

// enforceAllowlist checks the model allowlist after routing. Returns false
// after writing the 403 response.
func (s *Server) enforceAllowlist(w http.ResponseWriter, clientFmt translat.Format, ak *config.AuthKey, model string, res *router.Resolution) bool {
	if modelAllowed(ak, model, res) {
		return true
	}
	writeErr(w, clientFmt, errAPI(403, "model_not_allowed",
		fmt.Sprintf("key %q may not use model %q", ak.Label(), model)))
	return false
}
