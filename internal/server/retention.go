package server

// Usage-rollup retention. The [usage].retention_days knob (default 90)
// and store.Prune existed since M3, but nothing ever scheduled the prune —
// the rollup table grew unbounded. This loop deletes rows older than the
// retention window: once a minute after boot, then daily. The window is
// re-read from the live config snapshot every pass, so a SIGHUP reload of
// retention_days takes effect without a restart.

import "time"

func (s *Server) startRetention() {
	if s.st == nil { // memory mode: nothing persisted, nothing to prune
		return
	}
	s.retainStop = make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute) // first pass shortly after boot
		defer t.Stop()
		for {
			select {
			case <-s.retainStop:
				return
			case <-t.C:
				s.pruneOnce()
				t.Reset(24 * time.Hour) // steady-state daily cadence
			}
		}
	}()
}

// pruneOnce deletes rollups older than the configured retention window.
// Exposed for tests; returns the rows deleted.
func (s *Server) pruneOnce() (int64, error) {
	if s.st == nil {
		return 0, nil
	}
	st := s.cur()
	if st == nil || st.cfg.Usage.RetentionDays <= 0 {
		return 0, nil
	}
	return s.st.Prune(st.cfg.Usage.RetentionDays)
}
