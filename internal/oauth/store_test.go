package oauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestExternalWriteMerge covers two processes sharing the token file: a
// CLI Put while the gateway holds its own (newer) refresh must not roll
// back the gateway's rotated token, and a CLI write with a newer stamp
// must be adopted.
func TestExternalWriteMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth-tokens.json")

	// "Gateway" store: writes a refreshed token at T2.
	gw := NewTokenStoreFile(path)
	t2 := time.Now()
	gw.now = func() time.Time { return t2 }
	if err := gw.Put("xai/main", Token{
		AccessToken:  "at-gw-rotated",
		RefreshToken: "rt-gw-rotated",
		UpdatedAt:    t2,
	}); err != nil {
		t.Fatal(err)
	}

	// "CLI" store (separate process view, opened before the gateway's
	// write): writes an older token at T1 AFTER the gateway wrote.
	cli := NewTokenStoreFile(path)
	t1 := time.Now().Add(-time.Hour)
	cli.now = func() time.Time { return t1 }
	// cli's first sync adopts the gateway token; then its older Put must
	// NOT clobber the newer gateway entry...
	if err := cli.Put("xai/main", Token{
		AccessToken:  "at-cli-stale",
		RefreshToken: "rt-cli-stale",
		UpdatedAt:    t1,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := gw.Get("xai/main")
	if got.AccessToken != "at-gw-rotated" {
		t.Fatalf("older CLI write clobbered gateway rotation: %+v", got)
	}

	// ...but a NEWER external write must be adopted.
	t3 := t2.Add(time.Minute)
	cli.now = func() time.Time { return t3 }
	if err := cli.Put("xai/main", Token{
		AccessToken:  "at-cli-newer",
		RefreshToken: "rt-cli-newer",
		UpdatedAt:    t3,
	}); err != nil {
		t.Fatal(err)
	}
	gw.nextCheck = time.Time{} // past the hot-path throttle window
	got, _ = gw.Get("xai/main")
	if got.AccessToken != "at-cli-newer" {
		t.Fatalf("newer external write not adopted: %+v", got)
	}
}

// TestGetDoesNotRescanEveryCall pins the hot-path contract: after the
// first Get, the backing file is re-checked at most once per
// hotResyncInterval even under hammering.
func TestGetDoesNotRescanEveryCall(t *testing.T) {
	dir := t.TempDir()
	st := NewTokenStore(dir)
	if err := st.Put("xai/main", Token{AccessToken: "at-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("xai/main"); !ok {
		t.Fatal("token missing")
	}
	// Rewrite the file externally; an immediate Get may serve the cached
	// value (within the throttle window) — but within one hotResyncInterval
	// the new value must become visible.
	ext := map[string]Token{"xai/main": {AccessToken: "at-external", UpdatedAt: time.Now()}}
	b, _ := json.Marshal(tokenFile{Tokens: ext})
	if err := os.WriteFile(st.path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Force clock past the throttle: the next Get must observe it.
	st.nextCheck = time.Time{}
	got, ok := st.Get("xai/main")
	if !ok || got.AccessToken != "at-external" {
		t.Fatalf("external change not picked up after throttle window: %+v ok=%v", got, ok)
	}

	// The throttle window must be armed after the resync.
	if !st.now().Before(st.nextCheck) {
		t.Fatal("throttle window not armed after resync")
	}
}

// TestMemoryStoreHasNoFile pins the memory-mode contract (tests, data_dir
// = "memory").
func TestMemoryStoreHasNoFile(t *testing.T) {
	st := NewTokenStore("memory")
	if err := st.Put("xai/main", Token{AccessToken: "at-1"}); err != nil {
		t.Fatal(err)
	}
	if st.path != "" {
		t.Fatalf("memory store has a path: %q", st.path)
	}
	if _, ok := st.Get("xai/main"); !ok {
		t.Fatal("memory store lost token")
	}
}

// TestConcurrentGetPut exercises the mutex under -race.
func TestConcurrentGetPut(t *testing.T) {
	st := NewTokenStore("memory")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = st.Put("xai/main", Token{AccessToken: "at"})
		}()
		go func() {
			defer wg.Done()
			_, _ = st.Get("xai/main")
		}()
	}
	wg.Wait()
}
