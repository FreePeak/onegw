package update

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"
)

// Status is the update state surfaced to admins (CLI and /admin/update).
// Pid lets an updater verify WHICH process answered: after a handoff the
// new gateway reports its own pid, and only a pid change proves the new
// binary owns the port. LastApply carries a failed apply's reason.
type Status struct {
	Current     string     `json:"current"`
	Latest      string     `json:"latest,omitempty"`
	Outdated    bool       `json:"outdated"`
	LastCheck   *time.Time `json:"last_check,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastApply   string     `json:"last_apply,omitempty"`
	Pid         int        `json:"pid"`
	InContainer bool       `json:"in_container"`
	AutoApply   bool       `json:"auto_apply"`
	Interval    int64      `json:"interval_seconds"`
	Checking    bool       `json:"checking"`
	Applying    bool       `json:"applying"`
}

// Service runs the background update checker inside the gateway. It is
// reload-safe: cfgFn is re-read on every wake, so a SIGHUP that changes
// [update] settings takes effect on the next tick without a restart.
type Service struct {
	cfgFn func() Settings

	mu       sync.Mutex
	st       Status
	inflight bool
	applying bool
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewService builds the checker; cfgFn must return the live settings.
func NewService(cfgFn func() Settings) *Service {
	s := &Service{cfgFn: cfgFn, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	s.st.Current = Version()
	s.st.Pid = os.Getpid()
	s.st.InContainer = InContainer()
	return s
}

// Start launches the background loop; the first wake checks immediately so
// a fresh install learns its update state right away.
func (s *Service) Start() {
	go s.loop()
}

// Stop ends the loop and waits for the current check to finish.
func (s *Service) Stop() {
	close(s.stopCh)
	<-s.doneCh
}

func (s *Service) loop() {
	defer close(s.doneCh)
	for {
		s.wake()
		interval := s.cfgFn().Interval
		if interval <= 0 {
			interval = 3600 // disabled: re-wake hourly to notice re-enables
		}
		select {
		case <-s.stopCh:
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

// wake runs one check cycle when the current settings allow it.
func (s *Service) wake() {
	cfg := s.cfgFn()
	if cfg.Interval <= 0 {
		return // checking disabled
	}
	s.mu.Lock()
	if s.inflight {
		s.mu.Unlock()
		return
	}
	s.inflight = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight = false
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rel, err := (&Client{Repo: cfg.Repo}).Latest(ctx)
	now := time.Now()
	s.mu.Lock()
	if err != nil {
		// LastError is diagnostic state the NEXT successful check erases
		// (snapshotLocked clears it below), so a transient failure that
		// heals on the next 24h tick would leave no trace at all. Log it
		// here with a timestamp — the only durable record of when the
		// check failed and why (e.g. x509 unknown authority on an
		// intercepting network).
		log.Printf("onegw update: periodic check failed: %v", err)
		s.st.LastError = err.Error()
		s.st.LastCheck = &now
		s.mu.Unlock()
		return
	}
	s.st.Latest = rel.Tag
	s.st.LastError = ""
	s.st.LastCheck = &now
	s.st.Outdated = Compare(rel.Tag, s.st.Current) > 0
	outdated := s.st.Outdated
	s.mu.Unlock()
	if outdated {
		log.Printf("onegw update: release %s available (running %s) — `onegw update` or POST /admin/update",
			rel.Tag, s.st.Current)
	}
	// Auto-apply is a deliberate opt-in; containers never self-apply.
	if cfg.Auto && outdated && !s.st.InContainer {
		if err := s.ApplyAsync(Opt{Repo: cfg.Repo, Listen: cfg.Listen, AdminPassword: cfg.Password}); err != nil {
			log.Printf("onegw update: auto-apply: %v", err)
		}
	}
}

// ApplyAsync runs one full update (check + download + handoff) in the
// background and records the outcome in LastApply. On success the handoff
// drains THIS process, so a caller must not wait for the result here; on
// failure the current process keeps serving.
func (s *Service) ApplyAsync(opt Opt) error {
	if opt.Repo == "" {
		opt.Repo = s.cfgFn().Repo
	}
	if opt.Listen == "" || opt.AdminPassword == "" {
		cfg := s.cfgFn()
		if opt.Listen == "" {
			opt.Listen = cfg.Listen
		}
		if opt.AdminPassword == "" {
			opt.AdminPassword = cfg.Password
		}
	}
	s.mu.Lock()
	if s.applying {
		s.mu.Unlock()
		return errors.New("an update is already in progress")
	}
	s.applying = true
	s.st.Applying = true
	s.st.LastApply = ""
	s.mu.Unlock()
	go func() {
		_, err := Run(context.Background(), opt)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.applying = false
		s.st.Applying = false
		switch {
		case err == nil:
			s.st.LastApply = "applied" // this process is being drained now
		case errors.Is(err, ErrUpToDate):
			s.st.LastApply = "already up to date"
		default:
			s.st.LastApply = "failed: " + err.Error()
			log.Printf("onegw update: apply failed: %v", err)
		}
	}()
	return nil
}

// CheckNow performs an immediate check outside the tick (CLI / admin
// endpoint) and returns the resulting status.
func (s *Service) CheckNow(ctx context.Context) Status {
	cfg := s.cfgFn()
	rel, err := (&Client{Repo: cfg.Repo}).Latest(ctx)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.st.LastError = err.Error()
		s.st.LastCheck = &now
	} else {
		s.st.Latest = rel.Tag
		s.st.LastError = ""
		s.st.LastCheck = &now
		s.st.Outdated = Compare(rel.Tag, s.st.Current) > 0
	}
	return s.snapshotLocked()
}

// Snapshot copies the current status (safe for concurrent readers).
func (s *Service) Snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) snapshotLocked() Status {
	out := s.st
	cfg := s.cfgFn()
	out.AutoApply = cfg.Auto
	out.Interval = cfg.Interval
	out.Checking = s.inflight
	out.Applying = s.applying
	return out
}
