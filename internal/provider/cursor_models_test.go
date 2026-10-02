package provider

// Cursor model discovery: FetchModels asks the live AiService catalog RPC
// (POST /aiserver.v1.AiService/AvailableModels, application/proto, protobuf
// reply) and falls back to the curated list when that is unreachable.
//
// The RPC's shape is load-bearing and was found the hard way (2026-10-02):
// GET answers 405 and the chat path's connect+proto answers 415 — only
// POST + application/proto answers 200 — and a `type=web` browser session
// token answers 401 while a `type=session` account token answers 200.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"onegw/internal/translat"
)

// cursorCatalogBytes builds an AvailableModels protobuf reply: one repeated
// field-2 record per model, id in field 1, display name in field 17, with a
// non-model scalar (field 11) interleaved — the shape the live RPC returns.
func cursorCatalogBytes(ids ...string) []byte {
	var msg []byte
	for i, id := range ids {
		if i%2 == 0 {
			msg = append(msg, 0x50, 0x01) // field 11, varint 1
		}
		var rec []byte
		rec = append(rec, 0x0a, byte(len(id)))
		rec = append(rec, id...)
		msg = append(msg, 0x12, byte(len(rec))) // field 2, length-delimited
		msg = append(msg, rec...)
	}
	return msg
}

// The live catalog must win over the curated list, and the request must use
// the one method/content-type Cursor answers.
func TestFetchModelsCursorUsesLiveCatalog(t *testing.T) {
	var got struct {
		method, path, ct, auth string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.ct = r.Header.Get("Content-Type")
		got.auth = r.Header.Get("Authorization")
		_, _ = w.Write(cursorCatalogBytes("default", "grok-4.7", "claude-sonnet-4-5"))
	}))
	defer srv.Close()

	d := &Def{Name: "cursor", Kind: KindCursor, BaseURL: srv.URL,
		Accounts: []Account{{Name: "me", APIKey: "session-token"}}}

	body, status, err := d.FetchModels(context.Background(), &d.Accounts[0])
	if err != nil || status != 200 {
		t.Fatalf("fetch = %d %v", status, err)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST (GET answers 405 upstream)", got.method)
	}
	if got.path != translat.CursorModelsPath {
		t.Errorf("path = %s, want %s", got.path, translat.CursorModelsPath)
	}
	if got.ct != translat.CursorModelsContentType {
		t.Errorf("Content-Type = %s, want %s (connect+proto answers 415 upstream)", got.ct, translat.CursorModelsContentType)
	}
	if got.auth != "Bearer session-token" {
		t.Errorf("Authorization = %q", got.auth)
	}
	var catalog struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "grok-4.7", "claude-sonnet-4-5"}
	if len(catalog.Models) != len(want) {
		t.Fatalf("catalog = %v, want %v", catalog.Models, want)
	}
	for i := range want {
		if catalog.Models[i] != want[i] {
			t.Fatalf("catalog = %v, want %v", catalog.Models, want)
		}
	}
	// The point of the change: the live list is NOT the curated one. A stub
	// echoing cursorCuratedModels back would pass every assertion above —
	// these three ids are upstream's, not the gateway's stock eight, and
	// `claude-sonnet-4-5` (dashes) is the spelling Cursor actually serves.
	if slices.Contains(catalog.Models, "gpt-5.6") || slices.Contains(catalog.Models, "composer-2") {
		t.Errorf("catalog must come from upstream, not the curated list: %v", catalog.Models)
	}
}

// An unreachable listing RPC must NOT cost the provider its catalog: the
// dashboard surfaces the fallback as a working row, not an error.
func TestFetchModelsCursorFallsBackToCurated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &Def{Name: "cursor", Kind: KindCursor, BaseURL: srv.URL,
		Accounts: []Account{{Name: "me", APIKey: "session-token"}}}
	body, status, err := d.FetchModels(context.Background(), &d.Accounts[0])
	if err != nil || status != 200 {
		t.Fatalf("fallback must answer 200 with no error, got %d %v", status, err)
	}
	var catalog struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != len(cursorCuratedModels) {
		t.Fatalf("fallback catalog = %v, want the curated %v", catalog.Models, cursorCuratedModels)
	}
}

// No credential is the one case that must not dial: fetchProviderModels
// walks accounts, so a keyless row would burn the whole probe budget.
func TestFetchModelsCursorNoCredentialSkipsUpstream(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	d := &Def{Name: "cursor", Kind: KindCursor, BaseURL: srv.URL,
		Accounts: []Account{{Name: "me"}}}
	if _, _, err := d.FetchModels(context.Background(), &d.Accounts[0]); err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Error("a keyless account must not reach upstream")
	}
}

// The curated list is the documented stock fallback, so it must stay in sync
// with DefaultModels (which the server materialises onto an unconfigured
// provider block).
func TestCursorCuratedMatchesDefault(t *testing.T) {
	if got := DefaultModels(KindCursor); len(got) != len(cursorCuratedModels) {
		t.Fatalf("DefaultModels(cursor) = %v, want %v", got, cursorCuratedModels)
	}
	for i := range cursorCuratedModels {
		if DefaultModels(KindCursor)[i] != cursorCuratedModels[i] {
			t.Fatalf("DefaultModels(cursor) = %v, want %v", DefaultModels(KindCursor), cursorCuratedModels)
		}
	}
	if strings.Contains(strings.Join(cursorCuratedModels, " "), "cursor/") {
		t.Error("curated ids must be bare: the server prefixes them with the provider name")
	}
}
