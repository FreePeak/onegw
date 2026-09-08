// Command import9r imports provider connections from a 9router SQLite
// database (default ~/.9router/db/data.sqlite) into onegw TOML config.
//
//	onegw-import9r -db ~/.9router/db/data.sqlite -out imported.toml
//
// API-key connections import as accounts under an openai-compatible provider
// (9router nodeName/prefix becomes the onegw provider name, lowercased);
// models are discovered from the upstream /models endpoint when reachable.
// Built-in 9router providers without a per-connection base URL resolve from a
// known map, else are skipped with a warning. Bearer-token connections whose
// upstream is in bearerTokenProviders import as single-account providers of
// the given kind (9router OAuth access tokens are long-lived; refresh is a
// onegw v2 feature — issue #2). Output includes onegw gateway [auth] keys
// from 9router's apiKeys table.
package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type connRow struct {
	Provider string
	Name     string
	Priority int
	Data     string
}

type connData struct {
	APIKey               string `json:"apiKey"`
	AccessToken          string `json:"accessToken"`
	TestStatus           string `json:"testStatus"`
	ProviderSpecificData struct {
		Prefix  string `json:"prefix"`
		APIType string `json:"apiType"`
		BaseURL string `json:"baseUrl"`
		Node    string `json:"nodeName"`
	} `json:"providerSpecificData"`
}

// bearerTokenProviders maps 9router connections whose upstream accepts the
// stored token/key as a plain bearer to the onegw kind, base_url, and models
// endpoint to use. commandcode connections are apikey (not OAuth) but their
// upstream also takes the raw key as a bearer, so they import through the
// same table via the apikey path below. cursor/grok-cli remain skipped:
// cursor's protobuf protocol is a onegw skeleton (issue #12) and grok-cli
// needs an OAuth refresh flow (issue #2).
var bearerTokenProviders = map[string]struct{ kind, baseURL, modelsURL string }{
	"xai":         {"openai", "https://api.x.ai", "https://api.x.ai/v1/models"},
	"kilocode":    {"openai", "https://api.kilo.ai/api/openrouter", "https://api.kilo.ai/api/gateway/models"},
	"commandcode": {"commandcode", "https://api.commandcode.ai/alpha/generate", ""},
}

type account struct {
	name    string
	key     string
	ok      bool      // testStatus active
	expires time.Time // bearer-token expiry parsed from JWT, zero if unknown
}

type group struct {
	name      string
	kind      string
	baseURL   string
	modelsURL string
	nodeName  string
	accts     []account
}

// builtinBaseURL resolves 9router built-in providers that store no
// per-connection base URL. Unknown ones are skipped with a warning rather
// than emitting a config pointing at the wrong host.
var builtinBaseURL = map[string]string{
	"glm": "https://open.bigmodel.cn/api/paas/v4",
}

// builtinKind maps 9router built-in providers whose upstream is not an
// OpenAI-compatible chat-completions endpoint (empty modelsURL = discovery
// unsupported for that kind).
var builtinKind = map[string]struct{ kind, baseURL, modelsURL string }{
	"commandcode": {"commandcode", "https://api.commandcode.ai/alpha/generate", ""},
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	dbPath := flag.String("db", home+"/.9router/db/data.sqlite", "path to 9router data.sqlite")
	out := flag.String("out", "", "output TOML path (default stdout)")
	flag.Parse()
	os.Exit(runImport(*dbPath, *out, false))
}

// runImport reads the 9router DB and writes the onegw TOML. Split from main
// so tests can drive it; returns a process exit code. Verbose echoes the
// generated config to stdout.
func runImport(dbPath, out string, verbose bool) int {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(3000)")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()

	groups := map[string]*group{}
	rows, err := db.Query(`SELECT provider, name, priority, data FROM providerConnections
		WHERE authType IN ('apikey', 'oauth') AND isActive = 1 ORDER BY provider, priority`)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for rows.Next() {
		var r connRow
		if err := rows.Scan(&r.Provider, &r.Name, &r.Priority, &r.Data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		var d connData
		if err := json.Unmarshal([]byte(r.Data), &d); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		psd := d.ProviderSpecificData
		base := psd.BaseURL
		node := psd.Node
		if base == "" && d.APIKey == "" && d.AccessToken != "" {
			// Bearer-token (OAuth) connection: import as a single-account
			// provider when the upstream is known to accept the token as-is.
			bt, known := bearerTokenProviders[r.Provider]
			if !known {
				fmt.Fprintf(os.Stderr, "skip %s (%s): OAuth token flow not supported by onegw v1 (issue #2)\n", r.Provider, r.Name)
				continue
			}
			groups[r.Provider] = &group{
				name:      r.Provider,
				kind:      bt.kind,
				baseURL:   bt.baseURL,
				modelsURL: bt.modelsURL,
				accts:     []account{{name: orDefault(r.Name, "default"), key: d.AccessToken, ok: d.TestStatus == "active", expires: jwtExp(d.AccessToken)}},
			}
			continue
		}
		kind, modelsURL := "openai", ""
		if bk, ok := builtinKind[r.Provider]; ok {
			kind, modelsURL = bk.kind, bk.modelsURL
		}
		if base == "" {
			if known, ok := builtinBaseURL[r.Provider]; ok {
				base = known
				node = r.Provider
			} else if bk, ok := builtinKind[r.Provider]; ok {
				base = bk.baseURL
				node = r.Provider
			} else {
				fmt.Fprintf(os.Stderr, "skip %s (%s): no base_url known to onegw; configure manually\n", r.Provider, r.Name)
				continue
			}
		}
		key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(node, ".", "-"), " ", "-"))
		g := groups[key]
		if g == nil {
			g = &group{name: key, kind: kind, baseURL: base, modelsURL: modelsURL, nodeName: node}
			groups[key] = g
		}
		g.accts = append(g.accts, account{name: orDefault(r.Name, "default"), key: d.APIKey, ok: d.TestStatus == "active"})
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var names []string
	for k := range groups {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Generated by onegw-import9r from 9router data.sqlite\n")
	b.WriteString("# " + time.Now().UTC().Format(time.RFC3339) + "\n\n")

	// Gateway API keys: clients authenticate to onegw with these.
	var keyList []string
	krows, kerr := db.Query(`SELECT key FROM apiKeys WHERE isActive = 1`)
	if kerr == nil {
		for krows.Next() {
			var k string
			if krows.Scan(&k) == nil && k != "" {
				keyList = append(keyList, k)
			}
		}
		krows.Close()
	}
	if len(keyList) > 0 {
		b.WriteString("[auth]\nkeys = [" + quoteJoin(keyList) + "]\n\n")
	}

	b.WriteString("# --- providers ---\n\n")
	for _, name := range names {
		g := groups[name]
		fmt.Fprintf(&b, "[[providers]]\nname = %q\nkind = %q\n", g.name, g.kind)
		if g.baseURL != "" {
			fmt.Fprintf(&b, "base_url = %q\n", strings.TrimRight(g.baseURL, "/"))
		}
		models := discoverModels(g)
		if len(models) > 0 {
			fmt.Fprintf(&b, "models = [%s]\n", quoteJoin(models))
		}
		b.WriteString("\n")
		for _, a := range g.accts {
			note := ""
			if !a.ok {
				note = " # 9router marked unavailable (429 backoff)"
			}
			if !a.expires.IsZero() && time.Until(a.expires) < 48*time.Hour {
				note += fmt.Sprintf(" # bearer token expires %s — rotate in 9router, then re-import", a.expires.UTC().Format(time.RFC3339))
			}
			fmt.Fprintf(&b, "[[providers.accounts]]\nname = %q\napi_key = %q\n%s\n", a.name, a.key, note)
		}
		b.WriteString("\n")
	}

	if out == "" || verbose {
		os.Stdout.WriteString(b.String())
	}
	if out == "" {
		return 0
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "wrote", out)
	return 0
}

// discoverModels lists upstream models via GET /models using the first
// account; returns nil on any failure (pass-through routing still works via
// provider/model strings).
func discoverModels(g *group) []string {
	// modelsURL == "" on a non-openai kind means the upstream has no
	// discoverable /models endpoint (e.g. commandcode): skip discovery;
	// pass-through routing still works via provider/model strings.
	if g.baseURL == "" || len(g.accts) == 0 {
		return nil
	}
	if g.modelsURL == "" && g.kind != "openai" {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	modelsURL := g.modelsURL
	if modelsURL == "" {
		modelsURL = strings.TrimRight(g.baseURL, "/") + "/models"
	}
	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+g.accts[0].key)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: %s models fetch failed (%v); no static model list\n", g.name, err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &parsed) != nil || len(parsed.Data) == 0 {
		return nil
	}
	var ids []string
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func quoteJoin(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// jwtExp extracts the exp claim (seconds since epoch) from a JWT payload.
// Returns the zero time for opaque or undecodable tokens so callers can
// distinguish "expiry unknown" from a real deadline.
func jwtExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(claims.Exp), 0)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "import9r:", err)
		os.Exit(1)
	}
}
