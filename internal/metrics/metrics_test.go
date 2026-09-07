package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestRenderFormatAndOrdering(t *testing.T) {
	reg := NewRegistry()
	reqs := reg.Counter("onegw_requests_total", "Requests by provider and model.", "provider", "model", "code")
	reqs.Inc("p1", "m2", "200")
	reqs.Inc("p1", "m1", "500")
	reqs.Inc("p1", "m1", "500")
	inflight := reg.Gauge("onegw_inflight", "Requests currently live.")
	inflight.Set(7)
	reg.Gauge("onegw_zero_series", "Registered but never written.")

	got := reg.Render()
	want := `# HELP onegw_inflight Requests currently live.
# TYPE onegw_inflight gauge
onegw_inflight 7
# HELP onegw_requests_total Requests by provider and model.
# TYPE onegw_requests_total counter
onegw_requests_total{code="500",model="m1",provider="p1"} 2
onegw_requests_total{code="200",model="m2",provider="p1"} 1
# HELP onegw_zero_series Registered but never written.
# TYPE onegw_zero_series gauge
`
	if got != want {
		t.Fatalf("render mismatch:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestLabelValueEscaping(t *testing.T) {
	reg := NewRegistry()
	f := reg.Counter("m_weird", "h", "v")
	// Label value containing backslash, double quote, newline, tab.
	f.Inc("a\\b\"c\nd\te")
	// The escaping sibling that must NOT collide with the above.
	f.Inc(`a\b"c`)

	got := reg.Render()
	// Rendered form: \\ for backslash, \" for quote, \n for newline; the tab
	// passes through verbatim per the text exposition spec.
	want1 := `m_weird{v="a\\b\"c\nd` + "\t" + `e"} 1` + "\n"
	want2 := `m_weird{v="a\\b\"c"} 1` + "\n"
	if !strings.Contains(got, want1) {
		t.Fatalf("missing escaped line %q in:\n%s", want1, got)
	}
	if !strings.Contains(got, want2) {
		t.Fatalf("missing escaped line %q in:\n%s", want2, got)
	}
	if strings.Count(got, "m_weird{") != 2 {
		t.Fatalf("escapes must be bijective; series collided:\n%s", got)
	}
}

func TestHelpEscaping(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("m_help", `back\slash
newline and "quotes"`, "k")

	want := "# HELP m_help back\\\\slash\\nnewline and \"quotes\"\n"
	if got := reg.Render(); !strings.Contains(got, want) {
		t.Fatalf("help not escaped as expected:\n%s", got)
	}
}

func TestSeriesValueOps(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter("c", "h", "k")
	c.Add(5, "a")
	c.Inc("a")
	if got := c.Value("a"); got != 6 {
		t.Fatalf("counter Add/Inc: got %d, want 6", got)
	}
	g := reg.Gauge("g", "h", "k")
	g.Set(3, "x")
	g.Add(-5, "x")
	if got := g.Value("x"); got != -2 {
		t.Fatalf("gauge Set/Add: got %d, want -2", got)
	}
	// Same label values must return the same series (no duplicates).
	g.Set(9, "x")
	if got := g.Value("x"); got != 9 {
		t.Fatalf("series identity broken: got %d, want 9", got)
	}
}

func TestArityMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("wrong label arity must panic")
		}
	}()
	reg := NewRegistry()
	reg.Counter("c", "h", "a", "b").Inc("only-one")
}

func TestDuplicateFamilyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate family registration must panic")
		}
	}()
	reg := NewRegistry()
	reg.Counter("dupe", "h")
	reg.Gauge("dupe", "h")
}

func TestConcurrentAccess(t *testing.T) {
	reg := NewRegistry()
	f := reg.Counter("conc", "h", "w")
	const workers, iters = 8, 1000
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for range iters {
				f.Inc("worker")
			}
			_ = reg.Render() // scrape concurrently with writes
		}(w)
	}
	wg.Wait()
	if got := f.Value("worker"); got != workers*iters {
		t.Fatalf("lost increments: got %d, want %d", got, workers*iters)
	}
}
