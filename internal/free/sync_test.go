package free

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// catalogBody is a models.dev-shaped document with two models.
const catalogBody = `{"opencode-go":{"id":"opencode-go","models":{"minimax-m3.1-flash-free":{"limit":{"context":1000000,"output":512000}},"deepseek-v4.1-flash":{"limit":{"context":1000000,"output":384000}}}}}`

// TestSyncFetchesAndWritesArtifacts: the first fetch populates the index
// and both artifacts land in the data dir.
func TestSyncFetchesAndWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(catalogBody))
	}))
	defer srv.Close()

	ix := NewIndex()
	s := NewSyncer(ix, func() Options {
		return Options{URL: srv.URL, DataDir: dir, Interval: 24 * time.Hour, Timeout: 5 * time.Second}
	})
	if err := s.FetchOnce(context.Background()); err != nil {
		t.Fatalf("FetchOnce: %v", err)
	}
	e, ok := ix.Lookup("opencode-go", "minimax-m3.1-flash-free")
	if !ok {
		t.Fatal("catalog row not indexed")
	}
	if e.Context != 1000000 || e.MaxOut != 512000 {
		t.Fatalf("row = %+v, want context 1000000 / maxOut 512000", e)
	}
	for _, name := range []string{CatalogFile, RawCatalogFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
	}
	// The raw artifact holds the body as fetched.
	raw, err := os.ReadFile(filepath.Join(dir, RawCatalogFile))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if string(raw) != catalogBody {
		t.Fatalf("raw artifact = %q, want the fetched body", raw)
	}
	// The parsed artifact carries the ETag for the next conditional GET.
	blob, err := os.ReadFile(filepath.Join(dir, CatalogFile))
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var art artifact
	if err := json.Unmarshal(blob, &art); err != nil {
		t.Fatalf("parse artifact: %v", err)
	}
	if art.ETag != `"v1"` {
		t.Fatalf("artifact etag = %q, want %q", art.ETag, `"v1"`)
	}
	if len(art.Entries) != 2 {
		t.Fatalf("artifact entries = %d, want 2", len(art.Entries))
	}
}

// TestSyncNotModifiedKeepsData: a 304 must not wipe the index.
func TestSyncNotModifiedKeepsData(t *testing.T) {
	dir := t.TempDir()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(catalogBody))
	}))
	defer srv.Close()

	ix := NewIndex()
	s := NewSyncer(ix, func() Options {
		return Options{URL: srv.URL, DataDir: dir, Interval: 24 * time.Hour, Timeout: 5 * time.Second}
	})
	if err := s.FetchOnce(context.Background()); err != nil {
		t.Fatalf("first FetchOnce: %v", err)
	}
	// The second fetch is conditional and answers 304.
	if err := s.FetchOnce(context.Background()); err != nil {
		t.Fatalf("second FetchOnce: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits.Load())
	}
	e, ok := ix.Lookup("opencode-go", "minimax-m3.1-flash-free")
	if !ok {
		t.Fatal("304 wiped the index")
	}
	if e.Context != 1000000 {
		t.Fatalf("context = %d, want 1000000", e.Context)
	}
}

// TestSyncFailureKeepsData: a failing upstream must never truncate the
// previously good catalog.
func TestSyncFailureKeepsData(t *testing.T) {
	dir := t.TempDir()
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(catalogBody))
	}))
	defer srv.Close()

	ix := NewIndex()
	s := NewSyncer(ix, func() Options {
		return Options{URL: srv.URL, DataDir: dir, Interval: 24 * time.Hour, Timeout: 5 * time.Second}
	})
	if err := s.FetchOnce(context.Background()); err != nil {
		t.Fatalf("first FetchOnce: %v", err)
	}
	fail.Store(true)
	if err := s.FetchOnce(context.Background()); err == nil {
		t.Fatal("failing fetch must report an error")
	}
	e, ok := ix.Lookup("opencode-go", "minimax-m3.1-flash-free")
	if !ok {
		t.Fatal("a failed fetch wiped the index")
	}
	if e.Context != 1000000 {
		t.Fatalf("context = %d, want 1000000", e.Context)
	}
	// The artifact on disk is untouched as well.
	blob, err := os.ReadFile(filepath.Join(dir, CatalogFile))
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var art artifact
	if err := json.Unmarshal(blob, &art); err != nil {
		t.Fatalf("parse artifact: %v", err)
	}
	if len(art.Entries) != 2 {
		t.Fatalf("artifact entries = %d, want the 2 kept rows", len(art.Entries))
	}
}

// TestSyncUnparsableBodyKeepsData: a body the parser cannot read must not
// wipe the catalog either.
func TestSyncUnparsableBodyKeepsData(t *testing.T) {
	dir := t.TempDir()
	var junk atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if junk.Load() {
			_, _ = w.Write([]byte(`<html>502</html>`))
			return
		}
		_, _ = w.Write([]byte(catalogBody))
	}))
	defer srv.Close()

	ix := NewIndex()
	s := NewSyncer(ix, func() Options {
		return Options{URL: srv.URL, DataDir: dir, Interval: 24 * time.Hour, Timeout: 5 * time.Second}
	})
	if err := s.FetchOnce(context.Background()); err != nil {
		t.Fatalf("first FetchOnce: %v", err)
	}
	junk.Store(true)
	if err := s.FetchOnce(context.Background()); err == nil {
		t.Fatal("unparsable body must report an error")
	}
	if _, ok := ix.Lookup("opencode-go", "minimax-m3.1-flash-free"); !ok {
		t.Fatal("an unparsable body wiped the index")
	}
}

// TestSyncRestartResumesFromArtifact: a restart loads the stored
// catalog and re-reads the stored ETag, so the next fetch is conditional.
func TestSyncRestartResumesFromArtifact(t *testing.T) {
	dir := t.TempDir()
	var lastIfNoneMatch atomic.Value
	lastIfNoneMatch.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastIfNoneMatch.Store(r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(catalogBody))
	}))
	defer srv.Close()

	opts := func() Options {
		return Options{URL: srv.URL, DataDir: dir, Interval: 24 * time.Hour, Timeout: 5 * time.Second}
	}
	first := NewSyncer(NewIndex(), opts)
	if err := first.FetchOnce(context.Background()); err != nil {
		t.Fatalf("first FetchOnce: %v", err)
	}

	// A fresh syncer over the same data dir: the artifact is loaded
	// without a fetch, and the ETag rides along.
	ix := NewIndex()
	second := NewSyncer(ix, opts)
	second.Refresh()
	e, ok := ix.Lookup("opencode-go", "minimax-m3.1-flash-free")
	if !ok {
		t.Fatal("restart did not load the stored catalog")
	}
	if e.Context != 1000000 {
		t.Fatalf("context = %d, want 1000000", e.Context)
	}
	if err := second.FetchOnce(context.Background()); err != nil {
		t.Fatalf("second FetchOnce: %v", err)
	}
	if got := lastIfNoneMatch.Load().(string); got != `"v1"` {
		t.Fatalf("If-None-Match = %q, want the stored %q", got, `"v1"`)
	}
}

// TestSyncWritesAtomically: the artifact is written via a temp file, so a
// reader never sees a half-written catalog and no temp file is left over.
func TestSyncWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(catalogBody))
	}))
	defer srv.Close()

	ix := NewIndex()
	s := NewSyncer(ix, func() Options {
		return Options{URL: srv.URL, DataDir: dir, Interval: 24 * time.Hour, Timeout: 5 * time.Second}
	})
	if err := s.FetchOnce(context.Background()); err != nil {
		t.Fatalf("FetchOnce: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := map[string]bool{CatalogFile: true, RawCatalogFile: true}
	if len(names) != len(want) {
		t.Fatalf("data dir holds %v, want only the two artifacts", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected file %q in the data dir", n)
		}
	}
}
