package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/config"
	"onegw/internal/store"
	"onegw/internal/usage"
)

func newTransferServer(t *testing.T, adminPW string, provs ...providerSpec) (*Server, *config.Config) {
	t.Helper()
	cfg := makeCfg(t, "gw-key", adminPW, false, provs...)
	cfg.Server.DataDir = t.TempDir()
	cfg.Usage.FlushInterval = "1h" // tests flush explicitly via usage.Stop()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, cfg
}

// exportJSONL GETs the export endpoint (X-Admin-Password header, matching
// master's header-only adminOK) and returns the raw body.
func exportJSONL(t *testing.T, h http.Handler, pw, from, to string) string {
	t.Helper()
	url := "/admin/usage/export?"
	if from != "" {
		url += "from=" + from + "&"
	}
	if to != "" {
		url += "to=" + to
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	if pw != "" {
		r.Header.Set("X-Admin-Password", pw)
	}
	w := do(t, h, r)
	if w.Code != 200 {
		t.Fatalf("export: code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("export content type = %q", ct)
	}
	return w.Body.String()
}

// parseJSONL splits a transfer payload into header and rows.
func parseJSONL(t *testing.T, body string) (hdr exportHeader, rows []store.RollupRow) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) == 0 {
		t.Fatal("empty export")
	}
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header line: %v (%q)", err, lines[0])
	}
	for _, l := range lines[1:] {
		var r store.RollupRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("row line: %v (%q)", err, l)
		}
		rows = append(rows, r)
	}
	return hdr, rows
}

func importJSONL(t *testing.T, h http.Handler, pw, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/admin/usage/import", strings.NewReader(body))
	r.Header.Set("X-Admin-Password", pw)
	r.Header.Set("Content-Type", "application/x-ndjson")
	w := do(t, h, r)
	return w.Code, w.Body.String()
}

func seedBucket(t *testing.T, srv *Server, node, provider, model, day, hour string, req int64, first, last time.Time) {
	t.Helper()
	srv.st.SetNodeID(node)
	if err := srv.st.FlushBuckets([]usage.Bucket{{
		Key:      usage.Key{Day: day, Hour: hour, Provider: provider, Model: model, APIKey: "gw-key"},
		Requests: req, InputTokens: 10 * req, OutputTokens: 5 * req,
		FirstSeen: first, LastSeen: last,
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageExportDeterministicFilteredAndGated(t *testing.T) {
	srv, _ := newTransferServer(t, "pw")
	h := srv.Handler()
	day := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	seedBucket(t, srv, "n1", "p2", "m1", day, "09", 1, time.Time{}, time.Time{})
	seedBucket(t, srv, "n1", "p1", "m2", day, "09", 1, time.Time{}, time.Time{})
	seedBucket(t, srv, "n1", "p1", "m1", yesterday, "08", 3, time.Time{}, time.Time{})

	body := exportJSONL(t, h, "pw", yesterday, day)
	hdr, rows := parseJSONL(t, body)
	if hdr.Node == "" || hdr.Kind != "snapshot" {
		t.Fatalf("header wrong: %+v", hdr)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	for i, want := range [][2]string{{"p1", "m1"}, {"p1", "m2"}, {"p2", "m1"}} {
		if rows[i].Provider != want[0] || rows[i].Model != want[1] {
			t.Fatalf("row %d = %s/%s, want %s/%s", i, rows[i].Provider, rows[i].Model, want[0], want[1])
		}
	}
	if rows[0].Requests != 3 {
		t.Fatalf("row 0 requests = %d", rows[0].Requests)
	}
	// Filtered window drops today's rows.
	_, rows = parseJSONL(t, exportJSONL(t, h, "pw", yesterday, yesterday))
	if len(rows) != 1 || rows[0].Requests != 3 {
		t.Fatalf("yesterday filter wrong: %+v", rows)
	}
	// Deterministic: same window → byte-identical body.
	if again := exportJSONL(t, h, "pw", yesterday, day); again != body {
		t.Fatalf("export not deterministic:\n%q\nvs\n%q", body, again)
	}
	// Bad window and wrong/missing password rejected (header-only auth).
	badFrom := httptest.NewRequest(http.MethodGet, "/admin/usage/export?from=2026-13-01", nil)
	badFrom.Header.Set("X-Admin-Password", "pw")
	if w := do(t, h, badFrom); w.Code != 400 {
		t.Fatalf("bad from should 400, got %d", w.Code)
	}
	wrong := httptest.NewRequest(http.MethodGet, "/admin/usage/export", nil)
	wrong.Header.Set("X-Admin-Password", "wrong")
	if w := do(t, h, wrong); w.Code != 401 {
		t.Fatalf("wrong password should 401, got %d", w.Code)
	}
	if w := do(t, h, httptest.NewRequest(http.MethodGet, "/admin/usage/export", nil)); w.Code != 401 {
		t.Fatalf("no password should 401, got %d", w.Code)
	}
}

// Master's fail-closed defaulting must survive the merge: a config with an
// empty admin password still gates /admin/*, and Defaults fills "admin"
// rather than leaving the gate open.
func TestAdminPasswordDefaultStillGates(t *testing.T) {
	srv, cfg := newTransferServer(t, "")
	if cfg.Server.AdminPassword == "" {
		t.Fatal("Defaults must not leave the admin password empty")
	}
	h := srv.Handler()
	if w := do(t, h, httptest.NewRequest(http.MethodGet, "/admin/usage/export", nil)); w.Code != 401 {
		t.Fatalf("empty-config export without password should 401, got %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/usage/export", nil)
	r.Header.Set("X-Admin-Password", cfg.Server.AdminPassword)
	if w := do(t, h, r); w.Code != 200 {
		t.Fatalf("default admin password should pass, got %d", w.Code)
	}
}

// Import merges as snapshot: totals = local + remote; re-import is a no-op.
func TestUsageImportSnapshotIdempotent(t *testing.T) {
	srv, _ := newTransferServer(t, "pw")
	h := srv.Handler()
	day, hour := time.Now().UTC().Format("2006-01-02"), time.Now().UTC().Format("15")
	seedBucket(t, srv, "local", "p", "m", day, hour, 2, time.Time{}, time.Time{})

	first := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	last := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	payload := fmt.Sprintf(`{"node":"node-a","kind":"snapshot"}
{"node":"node-a","day":%q,"hour":%q,"provider":"p","model":"m","api_key":"gw-key","requests":5,"input":100,"output":50,"firstSeen":%q,"lastSeen":%q}
{"node":"node-a","day":%q,"hour":%q,"provider":"p","model":"other","api_key":"gw-key","requests":1,"input":10,"output":1}
`, day, hour, first.Format(time.RFC3339), last.Format(time.RFC3339), day, hour)

	code, resp := importJSONL(t, h, "pw", payload)
	if code != 200 || !strings.Contains(resp, `"imported":2`) {
		t.Fatalf("import: code=%d resp=%s", code, resp)
	}
	// Re-import must not double-count.
	code, resp = importJSONL(t, h, "pw", payload)
	if code != 200 || !strings.Contains(resp, `"imported":2`) {
		t.Fatalf("re-import: code=%d resp=%s", code, resp)
	}
	_, rows := parseJSONL(t, exportJSONL(t, h, "pw", day, day))
	var gotReq, otherReq int64
	for _, r := range rows {
		if r.Node != "node-a" {
			continue // the local shard keeps its own node
		}
		switch r.Model {
		case "m":
			gotReq = r.Requests
		case "other":
			otherReq = r.Requests
		}
	}
	if gotReq != 5 || otherReq != 1 {
		t.Fatalf("snapshot re-import changed totals: %+v", rows)
	}
}

// Delta kind sums consecutive windows and keeps min(first)/max(last);
// empty incoming timestamps never clobber stored bounds.
func TestUsageImportDeltaSumsAndBounds(t *testing.T) {
	srv, _ := newTransferServer(t, "pw")
	h := srv.Handler()
	day, hour := time.Now().UTC().Format("2006-01-02"), time.Now().UTC().Format("15")
	first := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	payload := fmt.Sprintf(`{"node":"node-a","kind":"delta"}
{"node":"node-a","day":%q,"hour":%q,"provider":"p","model":"m","api_key":"k","requests":3,"firstSeen":%q,"lastSeen":%q}
`, day, hour, first.Format(time.RFC3339), mid.Format(time.RFC3339))
	narrower := fmt.Sprintf(`{"node":"node-a","kind":"delta"}
{"node":"node-a","day":%q,"hour":%q,"provider":"p","model":"m","api_key":"k","requests":4,"firstSeen":%q,"lastSeen":%q}
{"node":"node-a","day":%q,"hour":%q,"provider":"p","model":"m","api_key":"k","requests":1}
`, day, hour, mid.Format(time.RFC3339), mid.Format(time.RFC3339), day, hour)

	for _, body := range []string{payload, narrower} {
		if code, resp := importJSONL(t, h, "pw", body); code != 200 {
			t.Fatalf("delta import: code=%d resp=%s", code, resp)
		}
	}
	_, rows := parseJSONL(t, exportJSONL(t, h, "pw", day, day))
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %+v", rows)
	}
	if rows[0].Requests != 8 {
		t.Fatalf("delta windows must sum: %+v", rows[0])
	}
	if !rows[0].FirstSeen.Equal(first) || !rows[0].LastSeen.Equal(mid) {
		t.Fatalf("bounds must stay min/max: %v..%v", rows[0].FirstSeen, rows[0].LastSeen)
	}
}

// Invalid rows are skipped, malformed JSON is a 400 — a hostile or broken
// source cannot corrupt totals.
func TestUsageImportRejectsCorruptingRows(t *testing.T) {
	srv, _ := newTransferServer(t, "pw")
	h := srv.Handler()
	day, hour := time.Now().UTC().Format("2006-01-02"), time.Now().UTC().Format("15")
	body := fmt.Sprintf(`{"node":"node-a"}
{"node":"node-a","day":%q,"hour":%q,"provider":"p","model":"m","api_key":"k","requests":-5}
{"node":"node-a","day":"2026-99-01","hour":%q,"provider":"p","model":"m2","api_key":"k","requests":9}
{"node":"node-a","day":%q,"hour":%q,"provider":"p","model":"ok","api_key":"k","requests":2}
`, day, hour, hour, day, hour)
	code, resp := importJSONL(t, h, "pw", body)
	if code != 200 || !strings.Contains(resp, `"imported":1`) || !strings.Contains(resp, `"skipped":2`) {
		t.Fatalf("corrupt rows must be skipped: code=%d resp=%s", code, resp)
	}
	_, rows := parseJSONL(t, exportJSONL(t, h, "pw", day, day))
	for _, r := range rows {
		if r.Model == "m" || r.Model == "m2" {
			t.Fatalf("rejected row leaked into store: %+v", r)
		}
	}
	if code, _ := importJSONL(t, h, "pw", `{"node":"a"}
not json`); code != 400 {
		t.Fatalf("malformed line should 400, got %d", code)
	}
}

// Round-trip across two instances with REAL traffic: requests through A,
// flush, export from A, import into B — B's aggregate view must equal A's.
func TestUsageTwoInstanceRoundTripE2E(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	srvA, _ := newTransferServer(t, "pw-a", providerSpec{name: "p1", up: up.URL, model: "m1"})
	srvB, _ := newTransferServer(t, "pw-b", providerSpec{name: "p1", up: up.URL, model: "m1"})
	hA, hB := srvA.Handler(), srvB.Handler()

	// Real usage through A (and B, so B is not empty and node sharding is
	// exercised on overlapping keys).
	req := func(h http.Handler, pw string) {
		t.Helper()
		r := chatReq(t, "p1/m1")
		r.Header.Set("Authorization", "Bearer gw-key")
		if w := do(t, h, r); w.Code != 200 {
			t.Fatalf("gateway request: code=%d body=%s", w.Code, w.Body.String())
		}
		_ = pw
	}
	req(hA, "pw-a")
	req(hB, "pw-b")
	srvA.cur().usage.Stop() // flush A's window into its store
	srvB.cur().usage.Stop() // flush B's window into its store

	// Export A → import into B.
	bodyA := exportJSONL(t, hA, "pw-a", "", "")
	code, resp := importJSONL(t, hB, "pw-b", bodyA)
	if code != 200 || !strings.Contains(resp, `"imported":1`) {
		t.Fatalf("import into B: code=%d resp=%s", code, resp)
	}

	// A's rows survive the trip into B byte-for-byte.
	_, rowsA := parseJSONL(t, bodyA)
	_, rowsB := parseJSONL(t, exportJSONL(t, hB, "pw-b", "", ""))
	var fromA, localB []store.RollupRow
	for _, r := range rowsB {
		if r.Node == rowsA[0].Node {
			fromA = append(fromA, r)
		} else {
			localB = append(localB, r)
		}
	}
	if len(fromA) != len(rowsA) || fmt.Sprint(fromA) != fmt.Sprint(rowsA) {
		t.Fatalf("round-trip rows differ:\nA: %+v\nB(from A): %+v", rowsA, fromA)
	}
	if len(localB) != 1 || localB[0].Requests != 1 {
		t.Fatalf("B's own row wrong: %+v", localB)
	}
	// B's aggregate view (dashboard semantics): A + B requests.
	saw := 0
	for _, r := range rowsB {
		saw += int(r.Requests)
	}
	if saw != 2 {
		t.Fatalf("B total requests = %d, want 2 (A=1 + B local=1)", saw)
	}
}

// Push: A forwards every flushed window to B's import endpoint; B's store
// ends up with A's usage attributed to A's node.
func TestUsagePushToAggregator(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	srvB, _ := newTransferServer(t, "pw-b", providerSpec{name: "p1", up: up.URL, model: "m1"})

	// Expose B's handler on a real listener so A's pusher can reach it.
	httpB := &http.Server{Handler: srvB.Handler()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go httpB.Serve(ln)
	defer httpB.Close()

	cfgA := makeCfg(t, "gw-key", "pw-a", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	cfgA.Server.DataDir = t.TempDir()
	cfgA.Usage.FlushInterval = "25ms"
	cfgA.Usage.ExportURL = "http://" + ln.Addr().String() + "/admin/usage/import"
	cfgA.Usage.ExportPassword = "pw-b"
	srvA, err := New(cfgA)
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	defer srvA.Close()

	r := chatReq(t, "p1/m1")
	r.Header.Set("Authorization", "Bearer gw-key")
	if w := do(t, srvA.Handler(), r); w.Code != 200 {
		t.Fatalf("gateway request: code=%d", w.Code)
	}

	// Poll B's store until A's pushed window lands (flush 25ms + push).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, rerr := srvB.st.Rollups("2000-01-01", "2999-01-01")
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, row := range rows {
			if row.Node == srvA.nodeID && row.Requests == 1 {
				return // arrived, attributed to A's node
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pushed window never reached the aggregator")
}
