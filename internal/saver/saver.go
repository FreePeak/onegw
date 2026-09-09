// Package saver implements RTK-style tool_result compression: sniff the
// first bytes of a tool result, pick a matching filter, compress lossy-but-
// safe. Filters never grow the payload and never fail the request — on any
// doubt the original text is returned.
package saver

import (
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Config controls filter aggressiveness and the output-side savers.
type Config struct {
	Enabled       bool        `toml:"enabled"`
	MaxLine       int         `toml:"max_line"`        // collapse long lines beyond this (0=400)
	DedupLines    bool        `toml:"dedup_lines"`     // collapse repeated identical lines
	MaxHead       int         `toml:"max_head"`        // keep N head lines for truncating filters (0=120)
	MaxTail       int         `toml:"max_tail"`        // keep N tail lines (0=20)
	MinSavingsPct int         `toml:"min_savings_pct"` // keep original if saved less (0=5)
	Inject        []InjectCfg `toml:"inject"`          // output-side system-prompt injections
	External      ExternalCfg `toml:"external"`        // external compress hook (Headroom protocol)
}

func (c *Config) fill() Config {
	v := *c
	if !v.DedupLines {
		v.DedupLines = true
	}
	if v.MaxLine == 0 {
		v.MaxLine = 400
	}
	if v.MaxHead == 0 {
		v.MaxHead = 120
	}
	if v.MaxTail == 0 {
		v.MaxTail = 20
	}
	if v.MinSavingsPct == 0 {
		v.MinSavingsPct = 5
	}
	if v.External.TimeoutMS == 0 {
		v.External.TimeoutMS = 2000
	}
	if v.External.MinBytes == 0 {
		v.External.MinBytes = 32768
	}
	return v
}

// Saver compresses tool_result text. The config is an atomic snapshot so a
// hot reload can flip Enabled (or any knob) without locking the hot path.
// convMu/convs carry per-conversation stickiness state for the raw-path
// gate (conv.go, issue #35).
type Saver struct {
	cfg     atomic.Pointer[Config]
	extOnce sync.Once // logs the first external-compress failure, once

	convMu sync.Mutex
	convs  map[uint64]*convEntry
}

// New builds a saver from config.
func New(cfg Config) *Saver {
	s := &Saver{}
	filled := cfg.fill()
	s.cfg.Store(&filled)
	return s
}

// SetEnabled flips the saver on/off without rebuilding filter config.
func (s *Saver) SetEnabled(on bool) {
	c := *s.cfg.Load()
	c.Enabled = on
	s.cfg.Store(&c)
}

// settings returns the active config snapshot.
func (s *Saver) settings() Config { return *s.cfg.Load() }

// Compress applies the best-matching filter to tool-result text. Empty text
// passes through unchanged.
func (s *Saver) Compress(text string) string {
	cfg := s.settings()
	out := s.compressFiltered(text, cfg)
	// Never grow, never over-truncate: savings gate.
	if out == "" || len(out) >= len(text) {
		return text
	}
	saved := 100 * (len(text) - len(out)) / len(text)
	if saved < cfg.MinSavingsPct {
		return text
	}
	return out
}

// compressFiltered runs the sniff-matched filter without the savings
// floor; callers apply their own never-grow gate.
func (s *Saver) compressFiltered(text string, cfg Config) string {
	if !cfg.Enabled || len(text) < 256 {
		return text
	}
	switch sniff(text) {
	case "git-diff":
		return filterDiff(text, cfg)
	case "git-status":
		return filterDedupCollapse(text, cfg, false)
	case "grep":
		return filterGrep(text, cfg)
	case "find", "ls", "tree":
		return filterTree(text, cfg)
	case "log":
		return filterDedupCollapse(text, cfg, true)
	default:
		return filterGeneric(text, cfg)
	}
}

// sniff identifies content type from leading bytes (RTK peek heuristic).
func sniff(text string) string {
	head := text
	if len(head) > 1024 {
		head = head[:1024]
	}
	lower := strings.ToLower(head)
	switch {
	case strings.Contains(lower, "diff --git") || (strings.Contains(lower, "+++") && strings.Contains(lower, "---")):
		return "git-diff"
	case strings.Contains(lower, "on branch ") || strings.Contains(lower, "nothing to commit") ||
		strings.Contains(lower, "changes not staged") || strings.Contains(lower, "untracked files"):
		return "git-status"
	case strings.Contains(lower, "error while loading shared libraries"):
		return "generic"
	case strings.Count(head, "\n") > 3 && (looksLikeLog(head) || strings.Contains(lower, "error") || strings.Contains(lower, "warn")):
		return "log"
	case strings.Contains(lower, "total ") && strings.Contains(lower, "-rw") || strings.Contains(lower, "drwx"):
		return "ls"
	case strings.HasPrefix(strings.TrimSpace(head), "./") || strings.Contains(lower, "\n./"):
		return "tree"
	case strings.Contains(lower, "matches") || strings.Contains(lower, ":") && looksLikeGrep(head):
		return "grep"
	default:
		return "generic"
	}
}

func looksLikeLog(head string) bool {
	// timestamps like 2026-09-07T12:00:00 or [12:00:00]
	return strings.Count(head, "T") > 1 && (strings.Contains(head, "Z ") || strings.Contains(head, "Z\n") || strings.Contains(head, "] "))
}

func looksLikeGrep(head string) bool {
	lines := strings.Split(head, "\n")
	if len(lines) < 2 {
		return false
	}
	colon := 0
	for _, l := range lines {
		if strings.Contains(l, ":") || strings.Contains(l, "-") {
			colon++
		}
	}
	return colon*2 > len(lines)
}

// collapseLongLines truncates individual monster lines (minified JS, dumps).
func collapseLongLines(text string, max int) string {
	if max <= 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) / 2)
	for line := range strings.SplitSeq(text, "\n") {
		if len(line) > max {
			cut := max
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut-- // never split a multi-byte rune: CJK, box-drawing, emoji
			}
			b.WriteString(line[:cut])
			b.WriteString("…[+")
			b.WriteString(itoa(len(line) - cut))
			b.WriteString("b]")
		} else {
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// truncate keeps head+tail lines and reports elision.
func truncate(text string, head, tail int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= head+tail+1 {
		return text
	}
	kept := make([]string, 0, head+tail+1)
	kept = append(kept, lines[:head]...)
	kept = append(kept, "… ["+itoa(len(lines)-head-tail)+" lines elided]")
	if tail > 0 {
		kept = append(kept, lines[len(lines)-tail:]...)
	}
	return strings.Join(kept, "\n")
}

// dedup collapses runs of identical lines into one + count.
func dedup(text string) string {
	var b strings.Builder
	var prev string
	count := 0
	flush := func() {
		switch {
		case count == 0:
		case count == 1:
			b.WriteString(prev)
			b.WriteByte('\n')
		default:
			b.WriteString(prev)
			b.WriteString(" ×")
			b.WriteString(itoa(count))
			b.WriteByte('\n')
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		if line == prev {
			count++
			continue
		}
		flush()
		prev = line
		count = 1
	}
	flush()
	return strings.TrimSuffix(b.String(), "\n")
}

// stripWhitespace squeezes trailing spaces and blank-line runs.
func stripWhitespace(text string) string {
	var b strings.Builder
	blank := 0
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimRight(line, " \t\r")
		if t == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		b.WriteString(t)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// filterDiff strips diff noise: keeps hunk headers and +/-/@@ lines, drops
// index/binary boilerplate, collapses context runs.
func filterDiff(text string, cfg Config) string {
	var b strings.Builder
	ctxRun := 0
	flushCtx := func() {
		if ctxRun > 3 {
			b.WriteString("… [")
			b.WriteString(itoa(ctxRun))
			b.WriteString(" ctx lines]\n")
		}
		ctxRun = 0
	}
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"),
			strings.HasPrefix(line, "@@"),
			strings.HasPrefix(line, "+++"),
			strings.HasPrefix(line, "---"),
			strings.HasPrefix(line, "+"),
			strings.HasPrefix(line, "-"):
			flushCtx()
			b.WriteString(line)
			b.WriteByte('\n')
		default:
			ctxRun++
		}
	}
	flushCtx()
	out := strings.TrimSuffix(b.String(), "\n")
	out = stripWhitespace(out)
	out = collapseLongLines(out, cfg.MaxLine)
	return out
}

// filterDedupCollapse for status/log content.
func filterDedupCollapse(text string, cfg Config, isLog bool) string {
	out := stripWhitespace(text)
	if cfg.DedupLines {
		out = dedup(out)
	}
	out = collapseLongLines(out, cfg.MaxLine)
	if isLog {
		out = truncate(out, cfg.MaxHead, cfg.MaxTail)
	}
	return out
}

// filterGrep keeps filename:line prefixes, squeezes matches.
func filterGrep(text string, cfg Config) string {
	out := stripWhitespace(text)
	out = dedup(out)
	out = collapseLongLines(out, cfg.MaxLine)
	return truncate(out, cfg.MaxHead, cfg.MaxTail)
}

// filterTree collapses directory listing runs.
func filterTree(text string, cfg Config) string {
	out := stripWhitespace(text)
	out = dedup(out)
	return truncate(out, cfg.MaxHead*2, cfg.MaxTail)
}

// filterGeneric is the safe default for unrecognized content.
func filterGeneric(text string, cfg Config) string {
	out := stripWhitespace(text)
	out = collapseLongLines(out, cfg.MaxLine)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
