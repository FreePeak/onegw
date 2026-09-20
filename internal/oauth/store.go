package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TokenStore persists OAuth tokens keyed by "provider/account".
//
// A JSON file under the data dir (not SQLite): tokens are O(#accounts)
// (tens at most) and are written only on login and refresh — minutes to
// hours apart — so a whole-file atomic rewrite is strictly simpler than a
// SQLite table and adds no schema or driver heap. It also stays
// human-inspectable while debugging expired accounts. This matches the
// store package's charter: SQLite is for usage rollups only.
//
// Writes are atomic (temp file + rename in the same directory) with 0600
// perms — the file holds bearer credentials. An empty data dir ("memory")
// keeps everything in RAM, which is what tests use.
//
// The parsed token map is kept resident: Get (the request-time bearer
// resolver) is a mutex-guarded map read, syscall-free. The file is read
// once at startup and re-read only when an external writer (the
// onegw-oauth CLI in another process) changes it — detected by mtime+size,
// re-checked at most once per second, never per request. Reads and writes
// merge per account: the newer UpdatedAt wins per key, so the gateway's
// auto-refresher and a concurrent CLI refresh cannot clobber each other's
// rotated refresh token.
type TokenStore struct {
	path string // "" = memory only

	mu        sync.Mutex
	toks      map[string]Token
	loaded    bool             // file read since process start
	modTime   time.Time        // last seen file mtime (external-write detection)
	size      int64            // last seen file size (mtime granularity guard)
	nextCheck time.Time        // next allowed file re-check on the hot path
	now       func() time.Time // injectable clock (tests)
}

// hotResyncInterval bounds how often Get re-stats the file for external
// writes: at most one stat per second, so the per-request bearer read is
// a pure map lookup.
const hotResyncInterval = time.Second

// NewTokenStore opens the token store under dataDir. An empty or "memory"
// dataDir keeps tokens in RAM only.
func NewTokenStore(dataDir string) *TokenStore {
	if dataDir == "" || dataDir == "memory" {
		return &TokenStore{toks: map[string]Token{}, now: time.Now}
	}
	return &TokenStore{
		path: filepath.Join(dataDir, "oauth-tokens.json"),
		toks: map[string]Token{},
		now:  time.Now,
	}
}

// NewTokenStoreFile is NewTokenStore with an explicit file path (tests).
func NewTokenStoreFile(path string) *TokenStore {
	return &TokenStore{path: path, toks: map[string]Token{}, now: time.Now}
}

type tokenFile struct {
	Tokens map[string]Token `json:"tokens"`
}

// syncLocked reconciles the resident map with the file: loads it once,
// then re-reads only when the file changed underneath us (external
// writer). On a re-read, the per-account merge keeps whichever copy has
// the newer UpdatedAt, so a stale disk snapshot never rolls back a live
// refresh. Callers hold mu.
func (s *TokenStore) syncLocked() error {
	if s.path == "" {
		return nil
	}
	fi, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	if s.loaded && fi.ModTime().Equal(s.modTime) && fi.Size() == s.size {
		return nil // unchanged since our last read/write
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f tokenFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("oauth token store %s: %w", s.path, err)
	}
	if !s.loaded {
		// Initial load: disk is the truth.
		if f.Tokens != nil {
			s.toks = f.Tokens
		}
	} else {
		mergeNewer(s.toks, f.Tokens)
	}
	s.loaded = true
	s.modTime = fi.ModTime()
	s.size = fi.Size()
	return nil
}

// mergeNewer copies every entry of src that is newer (by UpdatedAt) than
// the resident one into dst.
func mergeNewer(dst, src map[string]Token) {
	for k, t := range src {
		cur, ok := dst[k]
		if !ok || !t.UpdatedAt.Before(cur.UpdatedAt) {
			dst[k] = t
		}
	}
}

// persistLocked atomically rewrites the backing file. Immediately before
// writing it re-syncs with disk (merge-by-account, newer UpdatedAt wins),
// so a stale writer in another process can never roll back a fresher
// token that landed since its last read. Callers hold mu.
func (s *TokenStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := s.syncLocked(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tokenFile{Tokens: s.toks}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.modTime = fi.ModTime()
		s.size = fi.Size()
	}
	return nil
}

// Get returns the token stored under key. Hot path: a mutex-guarded map
// read; the file is re-checked at most once per hotResyncInterval.
func (s *TokenStore) Get(key string) (Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" && s.now().After(s.nextCheck) {
		_ = s.syncLocked()
		s.nextCheck = s.now().Add(hotResyncInterval)
	}
	t, ok := s.toks[key]
	return t, ok
}

// Put stores (or replaces) the token under key and persists. The write
// merges with any concurrent external change: per account, the newer
// UpdatedAt survives.
func (s *TokenStore) Put(key string, t Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.syncLocked(); err != nil {
		return err
	}
	t.UpdatedAt = s.now()
	s.toks[key] = t
	return s.persistLocked()
}

// Delete drops the token under key and persists.
func (s *TokenStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.syncLocked(); err != nil {
		return err
	}
	delete(s.toks, key)
	return s.persistLocked()
}

// Keys lists stored account keys, sorted.
func (s *TokenStore) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.syncLocked()
	keys := make([]string, 0, len(s.toks))
	for k := range s.toks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
