// Package dashboard serves the admin UI: server-rendered html/template
// pages with vendored inline htmx + uPlot (zero external assets), live
// updates over a bounded SSE fan-out. Pages are read-mostly views over
// live config and usage; config itself stays file-based (#41/#45).
package dashboard

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"strconv"
	"strings"
	"sync"
)

//go:embed templates/*.html static/*.css static/*.js static/LICENSES.md
var files embed.FS

// assets holds the minified vendor files; read once at package init and
// inlined into pages from memory (read-only bytes, no runtime cost beyond
// serving).
var assets map[string][]byte

// NavItem is one sidebar entry.
type NavItem struct{ ID, Href, Label string }

// Shell is the data every page template sees at the top level. V carries
// the page-specific view (content/scripts/hdr blocks execute with V as dot).
type Shell struct {
	Title  string
	Active string
	Live   bool // page subscribes to SSE (loads the sse extension)
	Nav    []NavItem
	V      any
}

// LoginView is the login page's view data (login.html is standalone and
// reads the dot directly).
type LoginView struct{ Error string }

// funcMap adds template helpers.
var funcMap = template.FuncMap{
	// compact renders a token count on the K/M/B ladder (≥1e9 → B),
	// trailing ".0" trimmed — mirrors the old dashboard's formatter.
	"compact":   compact,
	"asset_js":  func(name string) template.JS { return template.JS(assets[name]) },
	"asset_css": func(name string) template.CSS { return template.CSS(assets[name]) },
}

// Compact is the exported form of compact for handler-side formatting.
func Compact(n int64) string { return compact(n) }

func init() {
	assets = map[string][]byte{}
	for _, name := range []string{"htmx.min.js", "sse.min.js", "uPlot.iife.min.js", "uPlot.min.css", "admin.css"} {
		b, err := fs.ReadFile(files, "static/"+name)
		if err != nil {
			panic("dashboard: missing vendored asset " + name + ": " + err.Error())
		}
		assets[name] = b
	}
}

// compact renders n on the K/M/B ladder (≥1e9 → B, ≥1e6 → M, ≥1e3 → K),
// one decimal, trailing ".0" trimmed.
func compact(n int64) string {
	a := n
	if a < 0 {
		a = -a
	}
	var f string
	switch {
	case a >= 1e9:
		f = strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "B"
	case a >= 1e6:
		f = strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case a >= 1e3:
		f = strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "K"
	default:
		return strconv.FormatInt(n, 10)
	}
	return strings.TrimSuffix(f, ".0"+f[len(f)-1:])
}

// parsed caches the combined template set per page name so steady-state
// renders do no parsing.
var parsed sync.Map // name -> *template.Template

// Render executes the base shell with the named page's block overrides
// into a string the handler writes out.
func Render(page string, data Shell) (string, error) {
	var t *template.Template
	if v, ok := parsed.Load(page); ok {
		t = v.(*template.Template)
	} else {
		base, err := fs.ReadFile(files, "templates/base.html")
		if err != nil {
			return "", err
		}
		src, err := fs.ReadFile(files, "templates/"+page+".html")
		if err != nil {
			return "", err
		}
		t, err = template.New(page).Funcs(funcMap).Parse(string(base))
		if err != nil {
			return "", fmt.Errorf("dashboard: base: %w", err)
		}
		if _, err = t.Parse(string(src)); err != nil {
			return "", fmt.Errorf("dashboard: %s: %w", page, err)
		}
		parsed.Store(page, t)
	}
	var out strings.Builder
	if err := t.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Asset returns a vendored static asset's bytes (name like "htmx.min.js").
func Asset(name string) ([]byte, bool) {
	b, ok := assets[name]
	return b, ok
}
