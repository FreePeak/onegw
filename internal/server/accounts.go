package server

// Terminal account state actions (#80).
//
// A credential the upstream refused for billing reasons (402 /
// insufficient_quota) is marked terminal in the pool: not a cooldown, so no
// timer clears it, because waiting does not add credits. Recovery is an
// operator action — this endpoint — or a config reload that rotates the key
// (CarryInvalidated matches on name AND credential, so a new key under the
// same account name starts active).

import (
	"log"
	"net/http"
	"time"

	"onegw/internal/config"
	"onegw/internal/provider"
	"onegw/internal/types"
)

// handleAdminAccountReset answers
// POST /admin/api/v1/providers/{name}/accounts/{acct}/reset
// and clears the account's terminal state so the pool may serve it again.
// Resetting an account that is not invalidated answers 409 (not a silent
// success), so a UI mistake cannot look like a successful repair.
func (s *Server) handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	st := s.cur()
	name, acct := r.PathValue("name"), r.PathValue("acct")
	if st == nil || st.pool == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no live pool")
		return
	}
	def, ok := st.pool.Get(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown provider "+name)
		return
	}
	if !def.RevalidateByName(acct) {
		writeJSONError(w, http.StatusConflict, "account "+acct+" is not invalidated")
		return
	}
	log.Printf("server: re-enabled %s/%s (terminal billing state cleared)", name, acct)
	s.observeLog("", "", acct, 0, "key_revalidated", types.Usage{}, 0,
		"operator cleared terminal billing state for "+name, 0, 0, 0, 0)
	writeJSON(w, map[string]any{"provider": name, "account": acct, "invalidated": false})
}

// rotationPolicy merges the global [rotation] table with a provider override
// (#84): the provider wins field-by-field, an empty field inherits, and a
// fully empty pair yields the zero policy — which the provider package
// resolves to its shipped defaults, so an untouched config behaves exactly as
// it did before this knob existed.
func rotationPolicy(global, over config.RotationCfg) provider.RotationPolicy {
	pick := func(g, o string) time.Duration {
		for _, s := range []string{o, g} {
			if s == "" {
				continue
			}
			if d, err := time.ParseDuration(s); err == nil {
				return d
			}
		}
		return 0
	}
	pol := provider.RotationPolicy{
		CoolBase: pick(global.CooldownBase, over.CooldownBase),
		CoolCap:  pick(global.CooldownCap, over.CooldownCap),
		FlapOpen: pick(global.FlapOpen, over.FlapOpen),
		BenchTTL: pick(global.ModelBenchTTL, over.ModelBenchTTL),
	}
	switch {
	case over.FlapThreshold != 0:
		pol.FlapThreshold = over.FlapThreshold
	default:
		pol.FlapThreshold = global.FlapThreshold
	}
	return pol
}

// subscriptionHeadroom reports the worst remaining percent across an
// account's upstream-reported subscription windows (#79), for the p2c
// selection score (#81). ok=false when nothing is known — an unprobed or
// failed account must not be scored as if it were exhausted.
func (s *Server) subscriptionHeadroom(providerName, acct string) (float64, bool) {
	worst := 101.0
	found := false
	for _, snap := range s.subscriptionSnapshots() {
		if snap.Provider != providerName || snap.Account != acct {
			continue
		}
		for _, w := range snap.Windows {
			remaining := float64(100 - w.Used)
			if remaining < worst {
				worst = remaining
				found = true
			}
		}
	}
	if !found {
		return 0, false
	}
	return worst, true
}
