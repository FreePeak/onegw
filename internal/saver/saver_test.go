package saver

import (
	"strings"
	"testing"
)

func diffFixture() string {
	lines := []string{"diff --git a/main.go b/main.go", "index 1111111..2222222 100644"}
	for i := 0; i < 40; i++ {
		lines = append(lines, " context line "+strings.Repeat("x", 10))
	}
	lines = append(lines, "+func added() {}", "-func removed() {}", "@@ -1,2 +1,3 @@")
	for i := 0; i < 40; i++ {
		lines = append(lines, " more context "+strings.Repeat("y", 10))
	}
	return strings.Join(lines, "\n")
}

func TestCompressDiff(t *testing.T) {
	s := New(Config{Enabled: true})
	in := diffFixture()
	out := s.Compress(in)
	if len(out) >= len(in) {
		t.Fatalf("diff not compressed: in=%d out=%d", len(in), len(out))
	}
	if !strings.Contains(out, "+func added() {}") || !strings.Contains(out, "@@") {
		t.Fatalf("meaningful lines lost:\n%s", out)
	}
	if !strings.Contains(out, "ctx lines]") {
		t.Fatalf("context runs not collapsed:\n%s", out)
	}
}

func TestCompressLongLines(t *testing.T) {
	s := New(Config{Enabled: true})
	long := "payload: " + strings.Repeat("a", 5000)
	out := s.Compress(long)
	if len(out) >= len(long) {
		t.Fatalf("long line not truncated")
	}
	if !strings.Contains(out, "[+") {
		t.Fatalf("elision marker missing:\n%s", out)
	}
}

func TestCompressDedup(t *testing.T) {
	s := New(Config{Enabled: true})
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		sb.WriteString("ERROR something failed\n")
	}
	sb.WriteString("done\n")
	out := s.Compress(sb.String())
	if strings.Count(out, "ERROR") != 1 || !strings.Contains(out, "×") {
		t.Fatalf("dedup failed:\n%s", out)
	}
}

func TestCompressSmallPassThrough(t *testing.T) {
	s := New(Config{Enabled: true})
	small := "short output"
	if got := s.Compress(small); got != small {
		t.Fatalf("small text mutated: %q", got)
	}
}

func TestDisabledSaver(t *testing.T) {
	s := New(Config{Enabled: false})
	big := strings.Repeat("line\n", 1000)
	if got := s.Compress(big); got != big {
		t.Fatalf("disabled saver mutated text")
	}
}

func TestNeverGrows(t *testing.T) {
	s := New(Config{Enabled: true})
	inputs := []string{
		"already tight\nno waste\nhere",
		strings.Repeat("unique line 1\nunique line 2\n", 30),
		"123456789\n",
	}
	for _, in := range inputs {
		if got := s.Compress(in); len(got) > len(in) {
			t.Fatalf("output grew: %d > %d", len(got), len(in))
		}
	}
}

func TestToolResultSnippet(t *testing.T) {
	// Simulated Claude tool_result with git status output.
	status := "On branch main\nYour branch is up to date with 'origin/main'.\n\n" +
		strings.Repeat("\tmodified:   pkg/a.go\n", 25) +
		strings.Repeat("\tmodified:   pkg/b.go\n", 25) +
		"\nno changes added to commit"
	s := New(Config{Enabled: true})
	out := s.Compress(status)
	if strings.Count(out, "modified:") != 2 {
		t.Fatalf("expected dedup of modified lines:\n%s", out)
	}
	if len(out) >= len(status) {
		t.Fatal("no savings")
	}
}
