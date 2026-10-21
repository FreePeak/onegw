package free

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The catalog artifacts live under the gateway data dir.
const (
	// CatalogFile holds the parsed/indexed catalog (with the ETag used
	// for the next conditional GET).
	CatalogFile = "free-catalog.json"
	// RawCatalogFile holds the last raw body, for offline inspection.
	RawCatalogFile = "free-catalog-raw.json"
)

// The sync cadence (ported from 9router's modelCatalog/sync.js).
const (
	// DefaultStartupDelay keeps the first fetch off the boot path: the
	// gateway must start even when the catalog host is slow or down.
	DefaultStartupDelay = 60 * time.Second
	// DefaultInterval is the steady-state refresh cadence.
	DefaultInterval = 24 * time.Hour
	// DefaultRetryDelay is the wait after a failed fetch.
	DefaultRetryDelay = 30 * time.Minute
	// DefaultTimeout bounds one fetch (connect + body).
	DefaultTimeout = 60 * time.Second
)

// Options configure one catalog sync cycle.
type Options struct {
	// URL is the catalog endpoint. Empty disables the fetch.
	URL string
	// Interval is the steady-state refresh cadence.
	Interval time.Duration
	// DataDir is where the two artifacts are written.
	DataDir string
	// Timeout bounds one fetch.
	Timeout time.Duration
	// StartupDelay is the wait before the first fetch.
	StartupDelay time.Duration
	// RetryDelay is the wait after a failed fetch.
	RetryDelay time.Duration
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.StartupDelay <= 0 {
		o.StartupDelay = DefaultStartupDelay
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = DefaultRetryDelay
	}
	// Interval is NOT defaulted: 0 means the sync is off (the operator
	// set catalog_sync = ""), which the loop honours by idling.
	return o
}

// artifact is the on-disk shape of CatalogFile.
type artifact struct {
	ETag      string  `json:"etag"`
	FetchedAt string  `json:"fetched_at"`
	Entries   []Entry `json:"entries"`
}

// Syncer keeps the catalog index fresh.
//
// The loop never blocks the gateway: the first fetch waits out the startup
// delay, a failed fetch only costs a retry, and previously good data is
// never truncated by a failure. Two artifacts are written atomically
// (tmp + rename) under the data dir, so a crash mid-write leaves the last
// good catalog in place; the stored ETag is re-read from the artifact
// after a restart, so a restart does not re-download the whole catalog.
type Syncer struct {
	idx  *Index
	opts func() Options
	cli  *http.Client

	mu    sync.Mutex
	etag  string
	mtime time.Time

	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	ran    bool
	cancel context.CancelFunc
}

// NewSyncer builds a syncer over idx. The options are read through the
// getter on every cycle, so a config reload changes the URL or the
// cadence without a restart.
func NewSyncer(idx *Index, opts func() Options) *Syncer {
	if idx == nil {
		idx = NewIndex()
	}
	return &Syncer{
		idx:  idx,
		opts: opts,
		cli:  &http.Client{Timeout: DefaultTimeout},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Index returns the index the syncer writes into.
func (s *Syncer) Index() *Index { return s.idx }

// Start loads the stored artifact (if any) and runs the refresh loop in
// the background. It returns immediately.
func (s *Syncer) Start() {
	s.Refresh()
	s.mu.Lock()
	s.ran = true
	s.mu.Unlock()
	go s.run()
}

// Stop ends the refresh loop and waits for it to finish. An in-flight
// fetch is cancelled, so a shutdown never waits out the fetch timeout.
func (s *Syncer) Stop() {
	s.once.Do(func() { close(s.stop) })
	s.mu.Lock()
	ran := s.ran
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !ran {
		return
	}
	<-s.done
}

// run is the refresh loop: startup delay, then fetch, then wait out the
// interval (or the retry delay after a failure).
func (s *Syncer) run() {
	defer close(s.done)
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer cancel()
	opts := s.opts().withDefaults()
	timer := time.NewTimer(opts.StartupDelay)
	defer timer.Stop()
	select {
	case <-s.stop:
		return
	case <-timer.C:
	}
	for {
		opts = s.opts().withDefaults()
		if opts.Interval <= 0 {
			// The sync is off (catalog_sync = ""). Idle and re-check, so
			// a config reload that turns it back on takes effect.
			timer.Reset(time.Minute)
			select {
			case <-s.stop:
				return
			case <-timer.C:
			}
			continue
		}
		err := s.FetchOnce(ctx)
		opts = s.opts().withDefaults()
		wait := opts.Interval
		if err != nil {
			wait = opts.RetryDelay
		}
		timer.Reset(wait)
		select {
		case <-s.stop:
			return
		case <-timer.C:
		}
	}
}

// Refresh re-reads the stored artifact when it changed on disk, so a
// catalog written by another process (or by hand) is picked up without a
// fetch. The in-memory copy is invalidated by the artifact's mtime.
func (s *Syncer) Refresh() {
	opts := s.opts().withDefaults()
	if opts.DataDir == "" {
		return
	}
	path := filepath.Join(opts.DataDir, CatalogFile)
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	s.mu.Lock()
	fresh := !st.ModTime().Equal(s.mtime)
	s.mu.Unlock()
	if !fresh {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var art artifact
	if err := json.Unmarshal(body, &art); err != nil {
		return
	}
	rows := make([]Entry, 0, len(art.Entries))
	for _, e := range art.Entries {
		if e.Model == "" {
			continue
		}
		if e.Source == "" {
			e.Source = SourceCatalog
		}
		rows = append(rows, e)
	}
	if len(rows) == 0 {
		return
	}
	s.idx.Replace(rows)
	s.mu.Lock()
	s.etag = art.ETag
	s.mtime = st.ModTime()
	s.mu.Unlock()
}

// FetchOnce performs one conditional GET and, on a fresh body, writes the
// artifacts and swaps the index. A 304 keeps the current data; a failed
// fetch keeps the current data too — nothing is ever truncated by a
// failure.
func (s *Syncer) FetchOnce(ctx context.Context) error {
	opts := s.opts().withDefaults()
	if opts.URL == "" {
		return nil // disabled
	}
	if opts.DataDir == "" {
		return errors.New("free catalog: no data dir")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if etag := s.currentETag(); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	client := s.cli
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		// Not modified: the stored catalog is still current.
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("free catalog: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxCatalogBytes+1))
	if err != nil {
		return err
	}
	if len(body) > MaxCatalogBytes {
		return fmt.Errorf("free catalog: body over %d bytes", MaxCatalogBytes)
	}
	rows := ParseModelList(body)
	if len(rows) == 0 {
		// An unparsable body must never wipe a good catalog.
		return errors.New("free catalog: no rows parsed")
	}
	etag := resp.Header.Get("ETag")
	art := artifact{
		ETag:      etag,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Entries:   rows,
	}
	blob, err := json.Marshal(art)
	if err != nil {
		return err
	}
	// Both artifacts land atomically; the index only moves once they are
	// on disk, so a crash leaves the previous catalog intact.
	if err := writeFileAtomic(filepath.Join(opts.DataDir, RawCatalogFile), body); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(opts.DataDir, CatalogFile), blob); err != nil {
		return err
	}
	s.idx.Replace(rows)
	s.mu.Lock()
	s.etag = etag
	if st, err := os.Stat(filepath.Join(opts.DataDir, CatalogFile)); err == nil {
		s.mtime = st.ModTime()
	}
	s.mu.Unlock()
	return nil
}

// currentETag returns the stored ETag for the next conditional GET.
func (s *Syncer) currentETag() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.etag
}

// writeFileAtomic writes data to path via a temp file in the same dir
// plus a rename, so a reader never sees a half-written artifact.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
