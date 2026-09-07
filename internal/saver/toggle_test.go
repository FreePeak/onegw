package saver

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/translat"
)

func longToolResult() string {
	status := "On branch main\nYour branch is up to date with 'origin/main'.\n\n" +
		strings.Repeat("\tmodified:   pkg/a.go\n", 40) +
		strings.Repeat("\tmodified:   pkg/b.go\n", 40) +
		"\nno changes added to commit"
	return `{"messages":[{"role":"tool","content":` + quoteJSON(status) + `}]}`
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSetEnabledFlipsBehaviorLive(t *testing.T) {
	s := New(Config{Enabled: false})
	body := []byte(longToolResult())

	out, saved := s.ApplyRaw(translat.FmtOpenAI, body)
	if saved != 0 || string(out) != string(body) {
		t.Fatalf("disabled saver must pass through verbatim (saved=%d)", saved)
	}

	s.SetEnabled(true)
	out, saved = s.ApplyRaw(translat.FmtOpenAI, body)
	if saved <= 0 || len(out) >= len(body) {
		t.Fatalf("enabled saver should compress (saved=%d, in=%d, out=%d)", saved, len(body), len(out))
	}

	s.SetEnabled(false)
	out, saved = s.ApplyRaw(translat.FmtOpenAI, body)
	if saved != 0 {
		t.Fatalf("re-disabled saver must pass through again (saved=%d)", saved)
	}
}
