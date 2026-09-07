package server

import (
	"context"

	"onegw/internal/config"
	"onegw/internal/saver"
	"onegw/internal/translat"
)

// saverConfigFrom copies the TOML saver config into the runtime saver
// snapshot (the types are deliberately separate: config is file-shaped,
// saver.Config is the hot-path snapshot).
func saverConfigFrom(c *config.SaverCfg) saver.Config {
	out := saver.Config{Enabled: c.Enabled}
	for _, in := range c.Inject {
		out.Inject = append(out.Inject, saver.InjectCfg{Mode: in.Mode, Models: in.Models, Text: in.Text})
	}
	out.External = saver.ExternalCfg{
		Enabled:   c.External.Enabled,
		URL:       c.External.URL,
		TimeoutMS: c.External.TimeoutMS,
		MinBytes:  c.External.MinBytes,
		FailOpen:  c.External.FailOpen,
	}
	return out
}

// applyOutputSavers runs the output-side token savers over the raw
// client-format body: first the external compress hook, then terse-output
// injection. The [saver] enabled flag is the master token-saving switch —
// when it is off, neither output hook may touch the request. Injection
// runs last so the directive never gets shipped to the compress service
// (the client's own prompt stays the compression input; the directive is
// additive gateway state). An error can only come from external compress
// with fail_open=false; callers answer 502.
func (s *Server) applyOutputSavers(ctx context.Context, st *state, f translat.Format, body []byte, model string) ([]byte, error) {
	if !st.cfg.Saver.Enabled {
		return body, nil
	}
	var err error
	if body, err = st.saver.CompressExternal(ctx, body); err != nil {
		return nil, err
	}
	if len(st.cfg.Saver.Inject) > 0 {
		body = st.saver.InjectRaw(f, body, model)
	}
	return body, nil
}
