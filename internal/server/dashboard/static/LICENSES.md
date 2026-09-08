# Vendored frontend assets (inlined into the onegw binary)

Zero external assets policy: the dashboard loads no CDN resources. These
minified upstream files are embedded via go:embed and inlined into every
dashboard page response.

| File | Upstream | Version | License |
|---|---|---|---|
| `htmx.min.js` | https://github.com/bigskysoftware/htmx (npm htmx.org) | 2.0.6 | BSD 2-Clause |
| `sse.min.js` | https://github.com/bigskysoftware/htmx-extensions (npm htmx-ext-sse) | 2.2.4 | BSD 2-Clause |
| `uPlot.iife.min.js` | https://github.com/leeoniya/uPlot | 1.6.32 | MIT |
| `uPlot.min.css` | https://github.com/leeoniya/uPlot | 1.6.32 | MIT |

`admin.css` and the page JS in `templates/` are onegw-original code (MIT,
same as the repo).

To bump a version: re-download from the npm CDN pin above, update the
table, and re-run the page tests (`go test ./internal/server/`).
