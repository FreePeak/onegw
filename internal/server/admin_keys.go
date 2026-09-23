package server

// The credential inventory behind the dashboard's keys page: how every secret
// the gateway holds is flattened into rows, and how a per-row edit reaches the
// TOML without disturbing anything else in the block.
//
// Two families, because they point in opposite directions:
//
//   - client keys — what a CLI puts in `Authorization: Bearer …` to reach
//     onegw. Written by spliceAuthKeys, the path PATCH /admin/config/keys has
//     always used.
//   - provider credentials — what onegw sends upstream, one row per provider
//     account. Written by spliceProvider (the provider editor's own path) with
//     the untouched accounts' keys carried over from the server's config
//     snapshot, so editing one key cannot clobber its neighbours or drop a
//     field the keys page knows nothing about.
//
// A provider row is named the way the editor names it — the account's `name`,
// or the synthetic `key-N` / `default` that the legacy `keys` array and
// `api_key` scalar map to — so the page addresses exactly the line a save
// would write, and the first edit to a legacy provider normalizes it into
// [[providers.accounts]] the same way the editor does.

import (
	"fmt"
	"sort"
	"strings"

	"onegw/internal/config"
)

// clientKeyView is one gateway auth key.
type clientKeyView struct {
	Key    string   `json:"key"`
	Name   string   `json:"name,omitempty"`
	RPM    int      `json:"rpm,omitempty"`
	TPM    int      `json:"tpm,omitempty"`
	Models []string `json:"models,omitempty"`
}

// providerKeyView is one upstream credential row.
type providerKeyView struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Kind     string `json:"kind"`
	Key      string `json:"key,omitempty"`
	Masked   string `json:"masked,omitempty"`
	OAuth    string `json:"oauth,omitempty"` // subscription service profile
	Disabled bool   `json:"disabled,omitempty"`
	EnvVar   string `json:"env_var,omitempty"` // key came from the environment, not the file
}

// keyRowOp writes one provider account's key.
type keyRowOp struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Key      string `json:"key"`
}

// keyRowRef names one provider account's credential to delete.
type keyRowRef struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
}

// clientKeyViews lists the gateway's own auth keys in the clear.
func clientKeyViews(st *state) []clientKeyView {
	out := make([]clientKeyView, 0, len(st.cfg.Auth.KeyList))
	for i := range st.cfg.Auth.KeyList {
		k := &st.cfg.Auth.KeyList[i]
		out = append(out, clientKeyView{
			Key: k.Key, Name: k.Name, RPM: k.RPM, TPM: k.TPM, Models: k.Models,
		})
	}
	return out
}

// providerKeyViews flattens every provider's credential surface to one row per
// account, including the accounts that authenticate by OAuth sign-in (no key,
// so their row offers "Sign in" rather than a copy button).
func providerKeyViews(st *state) []providerKeyView {
	var out []providerKeyView
	for _, p := range st.cfg.Providers {
		svc := map[string]string{}
		for _, a := range st.cfg.OAuthAccounts() {
			if a.Provider == p.Name {
				svc[a.Account] = a.Service
			}
		}
		push := func(name, key, env string) {
			out = append(out, providerKeyView{
				Provider: p.Name, Account: name, Kind: p.Kind, Key: key,
				OAuth: svc[name], Disabled: p.Disabled, EnvVar: env,
			})
		}
		switch {
		case len(p.Accounts) > 0:
			for _, a := range p.Accounts {
				name := a.Name
				if name == "" {
					name = "default"
				}
				push(name, a.APIKey, "")
			}
		case len(p.Keys) > 0:
			for i, k := range p.Keys {
				env := ""
				if k == "" {
					env = fmt.Sprintf("%s_KEY%d", config.ProviderEnvPrefix(p.Name), i+1)
				}
				push(fmt.Sprintf("key-%d", i+1), k, env)
			}
		default:
			env := ""
			if p.APIKey == "" {
				env = config.ProviderKeyEnv(p.Name)
			}
			push("default", p.APIKey, env)
		}
	}
	return out
}

// validateKeyRowOps rejects a request the splicers could not honour, before
// anything touches the file.
func validateKeyRowOps(sets []keyRowOp, dels []keyRowRef) error {
	for _, op := range sets {
		if op.Provider == "" || op.Account == "" {
			return fmt.Errorf("provider key ops need both provider and account")
		}
		if op.Key == "" {
			return fmt.Errorf("provider %s account %s: set needs a key (use clear to remove one)", op.Provider, op.Account)
		}
		if strings.ContainsAny(op.Key, "\r\n\"\\") || strings.TrimSpace(op.Key) != op.Key {
			return fmt.Errorf("that key contains a character TOML cannot hold")
		}
	}
	for _, ref := range dels {
		if ref.Provider == "" || ref.Account == "" {
			return fmt.Errorf("provider key ops need both provider and account")
		}
	}
	return nil
}

// applyKeyRowOps rewrites every provider named in the batch, once each, and
// returns the edited lines plus the provider names touched.
func applyKeyRowOps(lines []string, providers []config.ProviderCfg, accounts []config.OAuthAccount,
	sets []keyRowOp, dels []keyRowRef) ([]string, []string, error) {

	byProvider := map[string][]keyRowOp{}
	delsBy := map[string][]keyRowRef{}
	for _, op := range sets {
		byProvider[op.Provider] = append(byProvider[op.Provider], op)
	}
	for _, ref := range dels {
		delsBy[ref.Provider] = append(delsBy[ref.Provider], ref)
	}
	var touched []string
	for _, name := range unionSets(byProvider, delsBy) {
		edited, err := applyKeyOps(lines, providers, accounts, name, byProvider[name], delsBy[name])
		if err != nil {
			return nil, nil, fmt.Errorf("provider %s: %w", name, err)
		}
		lines = edited
		touched = append(touched, name)
	}
	return lines, touched, nil
}

// applyKeyOps rebuilds one provider's editor request from the LIVE config
// (plaintext keys included — this is the server's own read of its own file),
// applies the key deltas, and splices the block.
func applyKeyOps(lines []string, providers []config.ProviderCfg, accounts []config.OAuthAccount,
	name string, sets []keyRowOp, dels []keyRowRef) ([]string, error) {

	var pc *config.ProviderCfg
	for i := range providers {
		if providers[i].Name == name {
			pc = &providers[i]
			break
		}
	}
	if pc == nil {
		return nil, fmt.Errorf("not configured")
	}
	req := providerEditReqFromConfig(*pc, accounts)

	setBy := make(map[string]string, len(sets))
	for _, op := range sets {
		setBy[op.Account] = op.Key
	}
	dropBy := make(map[string]bool, len(dels))
	for _, ref := range dels {
		dropBy[ref.Account] = true
	}

	roster := make([]acctEdit, 0, len(req.Accounts)+len(setBy))
	seen := make(map[string]bool, len(req.Accounts)+len(setBy))
	for _, a := range req.Accounts {
		if dropBy[a.Name] {
			if a.OAuth != "" {
				// A subscription row holds no key: clearing it means signing
				// out, which is the OAuth endpoints' job. Keep the row.
				seen[a.Name] = true
				roster = append(roster, a)
			}
			continue
		}
		if k, ok := setBy[a.Name]; ok {
			a.APIKey = k
		}
		seen[a.Name] = true
		roster = append(roster, a)
	}
	for _, acct := range sortedKeys(setBy) {
		if seen[acct] {
			continue
		}
		roster = append(roster, acctEdit{Name: acct, APIKey: setBy[acct]})
		seen[acct] = true
	}
	if len(roster) == 0 {
		return nil, fmt.Errorf("refusing to leave the provider with no account")
	}
	req.Accounts = roster
	out, _, err := spliceProvider(lines, req)
	return out, err
}

// providerEditReqFromConfig converts a live provider into the editor's request
// shape, keeping plaintext keys (unlike providerEditViews, which prefills a
// browser form and therefore reports has_key only). Accounts stays nil exactly
// when the provider has no per-account credential surface to rewrite, so
// spliceProvider leaves a keyless provider's legacy lines untouched.
// boolPtr boxes b for optional JSON bools where nil means "untouched"
// (the provider edit's proxy opt-in: omitted by forms that don't manage
// it, explicit true/false from the Proxies page multi-select).
func boolPtr(b bool) *bool { return &b }

func providerEditReqFromConfig(p config.ProviderCfg, accounts []config.OAuthAccount) providerEditReq {
	svc := map[string]string{}
	for _, a := range accounts {
		if a.Provider == p.Name {
			svc[a.Account] = a.Service
		}
	}
	req := providerEditReq{
		Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL,
		Models: p.Models, ResponsesModels: p.ResponsesModels,
		SubscriptionQuota: p.SubscriptionQuota, MaxConc: p.MaxConc,
		Sticky: p.Sticky, QuotaWindow: p.QuotaWindow,
		QuotaLimitTokens: p.QuotaLimitTokens, QuotaLimitReqs: p.QuotaLimitRequests,
		Proxy: boolPtr(p.Proxy),
	}
	switch {
	case len(p.Accounts) > 0:
		for _, a := range p.Accounts {
			name := a.Name
			if name == "" {
				name = "default"
			}
			req.Accounts = append(req.Accounts, acctEdit{
				Name: name, APIKey: a.APIKey, BaseURL: a.BaseURL, Weight: a.Weight, RPM: a.RPM,
				OAuth: svc[name],
			})
		}
	case len(p.Keys) > 0:
		for i, k := range p.Keys {
			name := fmt.Sprintf("key-%d", i+1)
			req.Accounts = append(req.Accounts, acctEdit{Name: name, APIKey: k, OAuth: svc[name]})
		}
	case p.APIKey != "" || svc["default"] != "":
		req.Accounts = append(req.Accounts, acctEdit{Name: "default", APIKey: p.APIKey, OAuth: svc["default"]})
	}
	return req
}

// unionSets lists the sorted union of two maps' keys, so each touched provider
// is rebuilt exactly once.
func unionSets[T, U any](a map[string][]T, b map[string][]U) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// sortedKeys returns m's keys in order, for deterministic roster writes.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
