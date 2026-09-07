package usage

import (
	"bytes"
	"io"
	"regexp"
)

// snifferRegexes extract final usage numbers from upstream payloads. The
// LAST match of each pattern wins (final counts appear last in streams).
var (
	reInput      = regexp.MustCompile(`"(?:input_tokens|prompt_tokens|promptTokenCount)"\s*:\s*(\d+)`)
	reOutput     = regexp.MustCompile(`"(?:output_tokens|completion_tokens|candidatesTokenCount)"\s*:\s*(\d+)`)
	reCacheRead  = regexp.MustCompile(`"(?:cache_read_input_tokens|cached_tokens|cachedContentTokenCount)"\s*:\s*(\d+)`)
	reCacheWrite = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)
	reReasoning  = regexp.MustCompile(`"(?:reasoning_tokens|thoughtsTokenCount)"\s*:\s*(\d+)`)
)

const sniffWindow = 64 << 10 // 64 KiB rolling tail

// trigger substrings that justify running regexes over the window.
var triggers = []string{"usage", "tokens", "TokenCount"}

// Sniffer wraps an upstream response body, scans a bounded rolling window,
// and passes bytes through untouched. It never buffers more than
// sniffWindow+chunk bytes. Usage() reports the best-effort final counts
// after EOF.
type Sniffer struct {
	r        io.Reader
	tail     []byte
	in       int64
	out      int64
	cr       int64
	cw       int64
	rs       int64
	seen     bool
	allZeros bool
}

// NewSniffer wraps r. limit caps how many bytes are scanned (0 = unlimited);
// scanning stops after limit bytes to bound CPU on huge non-stream bodies.
func NewSniffer(r io.Reader, limit int64) *Sniffer {
	return &Sniffer{r: r}
}

// Read implements io.Reader, sniffing while copying.
func (s *Sniffer) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.observe(p[:n])
	}
	return n, err
}

// Close closes the underlying reader when it supports it.
func (s *Sniffer) Close() error {
	if c, ok := s.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (s *Sniffer) observe(b []byte) {
	fast := false
	for _, t := range triggers {
		if bytes.Contains(b, []byte(t)) {
			fast = true
			break
		}
	}
	if fast {
		s.tail = append(s.tail, b...)
		if len(s.tail) > sniffWindow+len(b) {
			s.tail = s.tail[len(s.tail)-sniffWindow:]
		}
		s.extract()
	} else {
		// Keep a short tail anyway so multi-chunk matches assemble.
		const keep = 256
		s.tail = append(s.tail, b...)
		if len(s.tail) > keep {
			s.tail = s.tail[len(s.tail)-keep:]
		}
	}
}

// extract runs the regexes over the tail, keeping maxima (later final counts
// are larger or equal; take max to be resilient against partial windows).
func (s *Sniffer) extract() {
	if m := lastMatch(reInput, s.tail); m >= 0 && m > s.in {
		s.in = m
		s.seen = true
	}
	if m := lastMatch(reOutput, s.tail); m >= 0 && m > s.out {
		s.out = m
		s.seen = true
	}
	if m := lastMatch(reCacheRead, s.tail); m > s.cr {
		s.cr = m
	}
	if m := lastMatch(reCacheWrite, s.tail); m > s.cw {
		s.cw = m
	}
	if m := lastMatch(reReasoning, s.tail); m > s.rs {
		s.rs = m
	}
}

func lastMatch(re *regexp.Regexp, b []byte) int64 {
	ms := re.FindAllSubmatch(b, -1)
	if len(ms) == 0 {
		return -1
	}
	m := ms[len(ms)-1][1]
	var v int64
	for _, c := range m {
		v = v*10 + int64(c-'0')
	}
	return v
}

// Usage returns sniffed counts and whether any usage marker was seen.
func (s *Sniffer) Usage() (in, out, cacheRead, cacheWrite, reasoning int64, seen bool) {
	return s.in, s.out, s.cr, s.cw, s.rs, s.seen
}
