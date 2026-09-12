package server

// Subscription-quota admin surface (issue #79): wiring between the config
// (providers.subscription_quota), the subquota poll tracker, and the
// dashboard/API. The tracker itself lives in internal/subquota; this file
// derives its targets from config, parks exhausted accounts, and serves
// the JSON surface.

import (
	"fmt"
	"net/http"
	"time"

	"onegw/internal/config"
	"onegw/internal/subquota"
)

// quotaPageView is the quota dashboard page's whole view: the local
// reset-window table plus the upstream-reported subscription section.
type quotaPageView struct {
	Windows []quotaRowView
	Subs    []subRowView
}

// subTargets derives one probe target per account of every provider that
// opts into subscription quota tracking. Accounts without a credential
// have nothing to probe with and are skipped.
func subTargets(cfg *config.Config) []subquota.Target {
	var out []subquota.Target
	for _, p := range cfg.Providers {
		if !subquota.ValidDialect(p.SubscriptionQuota) {
			continue
		}
		accounts := p.Accounts
		if len(accounts) == 0 && p.APIKey != "" {
			// Single api_key providers build one "default" account (same
			// convention as Def building).
			accounts = []config.Acct{{Name: "default", APIKey: p.APIKey}}
		}
		for _, a := range accounts {
			if a.APIKey == "" {
				continue
			}
			out = append(out, subquota.Target{
				Provider: p.Name,
				AcctName: a.Name,
				AcctKey:  a.APIKey,
				Dialect:  p.SubscriptionQuota,
				URL:      p.SubscriptionURL,
			})
		}
	}
	return out
}

// liveSubKey resolves the CURRENT bearer credential for (provider, account)
// at probe time: OAuth-managed accounts rotate tokens in the background
// (TokenProvider), so the config-time key may already be stale.
func (s *Server) liveSubKey(provider, acctName string) string {
	st := s.cur()
	if st == nil || st.pool == nil {
		return ""
	}
	def, ok := st.pool.Get(provider)
	if !ok {
		return ""
	}
	for i := range def.Accounts {
		a := &def.Accounts[i]
		if a.Name == acctName && a.HasOAuthToken() {
			if tok := a.OAuthToken.Token(); tok != "" {
				return tok
			}
		}
	}
	return ""
}

// parkExhaustedSubscription is the tracker's onExhausted hook: the vendor
// reports a window fully consumed, so park THIS account (others keep
// serving) until the capped reset instant. The park duration is already
// bounded by the tracker to one poll cycle, so a recovered subscription
// self-heals without a manual un-park.
func (s *Server) parkExhaustedSubscription(tgt subquota.Target, until time.Time) {
	st := s.cur()
	if st == nil || st.pool == nil {
		return
	}
	def, ok := st.pool.Get(tgt.Provider)
	if !ok {
		return
	}
	d := time.Until(until)
	if d <= 0 {
		return
	}
	for i := range def.Accounts {
		// Match by account name only: the slot's own stored key always
		// matches itself inside pool.cool, and OAuth slots rotate keys.
		if a := &def.Accounts[i]; a.Name == tgt.AcctName {
			def.Cool(a, d)
			return
		}
	}
}

// subscriptionSnapshots returns the live per-account snapshots (empty for
// servers with no subscription_quota providers).
func (s *Server) subscriptionSnapshots() []subquota.Snapshot {
	if st := s.cur(); st != nil && st.subq != nil {
		return st.subq.All()
	}
	return nil
}

// handleAPISubscription answers GET /admin/api/v1/subscription with the
// upstream-reported subscription quota per (provider, account).
func (s *Server) handleAPISubscription(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	accounts := s.subscriptionSnapshots()
	if accounts == nil {
		accounts = []subquota.Snapshot{}
	}
	writeJSON(w, map[string]any{"accounts": accounts})
}

// ---------------------------------------------------------------------------
// Dashboard view rows (quota page, subscription section)
// ---------------------------------------------------------------------------

type subWinView struct {
	Name      string
	Pct       int
	BarClass  string
	Resets    string
	Exhausted bool
}

type subRowView struct {
	Provider, Account, Plan, Dialect string
	Windows                          []subWinView
	Err                              string
	Exhausted                        bool
	Fetched                          string
}

// subViews renders one row per account: its plan, windows (as a compact
// name/percent/reset cell list), or the probe error (fail-open: stale or
// missing data is shown, never hidden).
func (s *Server) subViews() []subRowView {
	out := []subRowView{}
	now := time.Now()
	for _, snap := range s.subscriptionSnapshots() {
		acct := snap.Account
		if acct == "" {
			acct = "default"
		}
		row := subRowView{
			Provider: snap.Provider,
			Account:  acct,
			Plan:     snap.Plan,
			Dialect:  snap.Dialect,
			Err:      snap.Err,
			Fetched:  snap.FetchedAt.Format("15:04:05"),
		}
		for _, w := range snap.Windows {
			wv := subWinView{Name: w.Name, Pct: w.Used}
			switch {
			case w.Used >= 100:
				wv.BarClass = "err"
				wv.Exhausted = true
				row.Exhausted = true
			case w.Used >= 80:
				wv.BarClass = "warn"
			}
			if w.Resets != nil && w.Resets.After(now) {
				wv.Resets = untilString(*w.Resets, now)
			}
			row.Windows = append(row.Windows, wv)
		}
		out = append(out, row)
	}
	return out
}

// untilString renders a remaining duration the same way the local quota
// table does (days > hours > Go duration).
func untilString(t, now time.Time) string {
	d := t.Sub(now).Round(time.Second)
	switch {
	case d > 48*time.Hour:
		return fmt.Sprintf("%.1f d", d.Hours()/24)
	case d > 2*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	default:
		return d.String()
	}
}
