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

`admin.css` is COMPILED from `../admin.src.css` by the Tailwind v4
standalone CLI (`scripts/dashboard-css.sh`, no Node/npm) — the committed
output is what the binary embeds. Tailwind itself is a build-time-only
tool (MIT) and is not vendored. The page JS in `templates/` is
onegw-original code (MIT, same as the repo).

The vendored fonts are latin subsets. Fira Code comes from Google Fonts
under the SIL Open Font License 1.1 (text in `fonts/OFL.txt`); "Rebels"
ships with the M.O.N.K.Y OS dashboard template this CSS revamp follows
(no license file included — decorative display face; swap the file to
rebrand):

| File | Family | License |
|---|---|---|
| `fonts/FiraCode-latin.woff2` | Fira Code (Nikita Prokopov) | OFL 1.1 |
| `fonts/Rebels-Fett.woff2` | "Rebels" display face (M.O.N.K.Y OS template) | see note in fonts/OFL.txt |
| `fonts/OFL.txt` | license texts + provenance notes | OFL 1.1 |

Fonts are served unauthenticated from `GET /admin/assets/fonts/{name}`
(static bytes; the login page loads them before any session exists).

To bump a version: re-download from the npm CDN pin above, update the
table, and re-run the page tests (`go test ./internal/server/`).
