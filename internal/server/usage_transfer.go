// Usage transfer across onegw instances: node-attributed JSONL export and
// merge import. Turns any instance into an aggregator with zero extra
// processes — the store's rollups are sharded by source node (node_id in
// the primary key), so snapshot imports are idempotent (replace per
// key+node) and delta pushes accumulate (sum).
//
// Wire format (application/x-ndjson):
//
//	{"node":"<exporter>","kind":"snapshot"|"delta"}   first line
//	{"node":"<source>","day":...,"hour":...,...}      one line per rollup shard
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"onegw/internal/store"
)

// nodeID returns this instance's identifier: hostname plus a short random
// suffix, persisted in the data dir so restarts keep the same identity.
// Ephemeral (unpersisted) when there is no data dir.
func nodeID(dataDir string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "onegw"
	}
	host = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.':
			return r
		default:
			return '-'
		}
	}, host)
	if dataDir == "" || dataDir == "memory" {
		return host + "-" + randHex6()
	}
	path := filepath.Join(dataDir, "node_id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	id := host + "-" + randHex6()
	// Best effort: a read-only data dir still yields a usable (unstable)
	// identity for this process.
	_ = os.WriteFile(path, []byte(id+"\n"), 0o644)
	return id
}

func randHex6() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}

// handleUsageExport answers GET /admin/usage/export?from=YYYY-MM-DD&to=YYYY-MM-DD
// with a node-attributed JSONL snapshot of the stored rollups. Rows stream
// out in deterministic (day, hour, provider, model, api_key, node) order —
// never fully buffered.
func (s *Server) handleUsageExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	if s.st == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no persistent store"}`))
		return
	}
	from, to, err := exportDayRange(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%q}`, err.Error())))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	line := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		_, _ = w.Write(append(b, '\n'))
		return true
	}
	line(exportHeader{Node: s.nodeID, Kind: "snapshot"})
	// Stream row by row: memory O(1), never a full-history buffer.
	n := 0
	err = s.st.EachRollup(from, to, func(row store.RollupRow) error {
		if !line(row) {
			return errStopExport
		}
		n++
		if flusher != nil && n%256 == 0 {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && err != errStopExport {
		// Headers are already gone; the truncated stream itself signals the
		// failure to the importer (a snapshot re-import self-heals).
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

type exportHeader struct {
	Node string `json:"node"`
	Kind string `json:"kind"`
}

// errStopExport aborts a streaming export (marshal failure) without
// misreporting it as a store error.
var errStopExport = errors.New("export aborted")

// exportDayRange validates the window. Defaults: today. The fixed
// YYYY-MM-DD layout makes lexicographic order equal chronological order.
func exportDayRange(r *http.Request) (from, to string, err error) {
	const layout = "2006-01-02"
	today := time.Now().UTC().Format(layout)
	from, to = r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" {
		from = today
	}
	if to == "" {
		to = today
	}
	f, ferr := time.Parse(layout, from)
	t, terr := time.Parse(layout, to)
	if ferr != nil || terr != nil {
		return "", "", fmt.Errorf("from/to must be YYYY-MM-DD")
	}
	if f.After(t) {
		return "", "", fmt.Errorf("from must not be after to")
	}
	if t.Sub(f) > 730*24*time.Hour {
		return "", "", fmt.Errorf("range exceeds 730 days")
	}
	return from, to, nil
}

// handleUsageImport answers POST /admin/usage/import. It accepts the export
// JSONL (header line + row lines, or a single {"node":...,"rows":[...]}
// envelope) and merges rows into the local store:
//
//	kind "snapshot" (default) — replace counters per (day, hour, provider,
//	  model, api_key, node). Re-importing the same file is a no-op.
//	kind "delta" — sum counters per key+node (pushed flush windows).
//
// Body size is capped by server.max_body_bytes; rows merge in batches, so
// a malformed file can partially apply — retrying a snapshot self-heals.
func (s *Server) handleUsageImport(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	if s.st == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no persistent store"}`))
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cur().cfg.Server.MaxBody)
	dec := json.NewDecoder(body)

	var header exportHeader
	var envelope struct {
		Node string            `json:"node"`
		Kind string            `json:"kind"`
		Rows []store.RollupRow `json:"rows"`
	}
	if err := dec.Decode(&envelope); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"malformed jsonl"}`))
		return
	}
	if envelope.Node != "" {
		header.Node = envelope.Node
	}
	header.Kind = envelope.Kind

	imported, skipped := 0, 0
	var mergeErr error
	batch := make([]store.RollupRow, 0, 256)
	merge := func() {
		if mergeErr != nil || len(batch) == 0 {
			return
		}
		if err := s.st.MergeRows(batch, header.Kind == "delta"); err != nil {
			mergeErr = err
			return
		}
		imported += len(batch)
		batch = batch[:0]
	}
	add := func(row store.RollupRow) {
		if !validRollupRow(row) {
			skipped++
			return
		}
		if row.Node == "" {
			row.Node = header.Node
		}
		batch = append(batch, row)
		if len(batch) >= 1024 {
			merge()
		}
	}
	for _, row := range envelope.Rows {
		add(row)
	}
	for dec.More() {
		var row store.RollupRow
		if err := dec.Decode(&row); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"malformed jsonl after %d rows"}`, imported)))
			return
		}
		add(row)
	}
	merge()
	if mergeErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"merge failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]int{"imported": imported, "skipped": skipped})
}

// validRollupRow rejects rows that could corrupt totals: bad day/hour
// shapes or negative counters. Zero counters are fine (idle keys re-sent).
func validRollupRow(r store.RollupRow) bool {
	if _, err := time.Parse("2006-01-02", r.Day); err != nil {
		return false
	}
	if r.Hour != "" {
		if _, err := time.Parse("15", r.Hour); err != nil {
			return false
		}
	}
	neg := r.Requests < 0 || r.InputTokens < 0 || r.OutputTokens < 0 ||
		r.CacheRead < 0 || r.CacheWrite < 0 || r.Reasoning < 0 || r.SavedTokens < 0
	return !neg
}
