package server

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/translat"
)

// markerPaths decodes body and reports every path whose object carries a
// cache_control field, sorted for order-independent comparison.
func markerPaths(t *testing.T, body []byte) []string {
	t.Helper()
	var root any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var paths []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch tv := v.(type) {
		case map[string]any:
			if _, has := tv["cache_control"]; has {
				paths = append(paths, path)
			}
			for k, child := range tv {
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range tv {
				walk(child, path+"["+itoa(i)+"]")
			}
		}
	}
	walk(root, "")
	sort.Strings(paths)
	return paths
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// claudeFixture builds an Anthropic-format body with client markers in
// the wrong places (first system block, a mid-list tool, a thinking
// block) and a defer_loading tool after the last cache-eligible one.
func claudeFixture() []byte {
	return []byte(`{
	 "model": "claude-x", "max_tokens": 8, "temperature": 0.123456789,
	 "system": [
	   {"type": "text", "text": "sys one", "cache_control": {"type": "ephemeral"}},
	   {"type": "text", "text": "sys two"}
	 ],
	 "tools": [
	   {"name": "t1", "input_schema": {"type": "object"}},
	   {"name": "t3", "cache_control": {"type": "ephemeral"}},
	   {"name": "t2", "defer_loading": true}
	 ],
	 "messages": [
	   {"role": "user", "content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}]},
	   {"role": "assistant", "content": [
	     {"type": "text", "text": "answer"},
	     {"type": "thinking", "thinking": "hmm", "cache_control": {"type": "ephemeral"}},
	     {"type": "text", "text": "final"}
	   ]},
	   {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "x", "content": "out"}]}
	 ]
	}`)
}

func claudeDef(profile string) *provider.Def {
	return &provider.Def{Name: "p", CacheProfile: profile}
}

// TestAnchorClaudeStripsAndReanchors pins the full claude-anchor contract:
// every client marker is stripped (they point at pre-normalization
// offsets), then exactly three ephemeral markers are placed at the
// canonical positions — last system block, last cache-eligible tool
// (defer_loading skipped), last assistant turn's last non-thinking block.
func TestAnchorClaudeStripsAndReanchors(t *testing.T) {
	out := anchorCacheProfile(claudeFixture(), "claude-x", claudeDef("claude-anchor"), translat.FmtAnthropic, "")
	paths := markerPaths(t, out)
	want := []string{".messages[1].content[2]", ".system[1]", ".tools[1]"}
	if len(paths) != len(want) {
		t.Fatalf("want markers at %v, got %v", want, paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("want markers at %v, got %v", want, paths)
		}
	}
	// defer_loading tools and thinking blocks must never carry a marker.
	joined := strings.Join(paths, ",")
	if strings.Contains(joined, ".tools[2]") || strings.Contains(joined, "thinking") {
		t.Fatalf("defer_loading tool or thinking block received a marker: %v", paths)
	}
	// Numeric fidelity across the rewrite.
	if !bytes.Contains(out, []byte("0.123456789")) {
		t.Fatal("temperature literal not preserved")
	}
}

// TestAnchorClaudeTurnOneFallback: with no assistant message yet, the
// final message takes the anchor and a string system is wrapped into the
// block form so it can carry the marker.
func TestAnchorClaudeTurnOneFallback(t *testing.T) {
	body := []byte(`{
	 "model": "claude-x", "max_tokens": 8, "system": "be nice",
	 "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`)
	out := anchorCacheProfile(body, "claude-x", claudeDef("claude-anchor"), translat.FmtAnthropic, "")
	paths := markerPaths(t, out)
	if len(paths) != 2 || paths[0] != ".messages[0].content[0]" || paths[1] != ".system[0]" {
		t.Fatalf("want final-message + system[0] markers, got %v", paths)
	}
}

// TestAnchorProfilesDefaultUntouched: none/nil-def/unknown profiles and
// profile/format mismatches never touch the body — GLM/DeepSeek/b-ai
// upstreams ignore cache fields and must not receive invented bytes.
func TestAnchorProfilesDefaultUntouched(t *testing.T) {
	fixture := claudeFixture()
	cases := []struct {
		name     string
		profile  string
		def      *provider.Def
		upstream translat.Format
		key      string
	}{
		{"no profile", "", claudeDef(""), translat.FmtAnthropic, ""},
		{"none profile", "none", claudeDef("none"), translat.FmtAnthropic, ""},
		{"nil def", "claude-anchor", nil, translat.FmtAnthropic, ""},
		{"unknown profile", "bogus", claudeDef("bogus"), translat.FmtAnthropic, ""},
		{"claude-anchor on openai upstream", "claude-anchor", claudeDef("claude-anchor"), translat.FmtOpenAI, ""},
		{"sticky-key on anthropic upstream", "sticky-key", claudeDef("sticky-key"), translat.FmtAnthropic, "sess"},
		{"dashscope on anthropic upstream", "dashscope-marker", claudeDef("dashscope-marker"), translat.FmtAnthropic, ""},
		{"sticky-key without session", "sticky-key", claudeDef("sticky-key"), translat.FmtOpenAI, ""},
	}
	for _, tc := range cases {
		out := anchorCacheProfile(fixture, "m", tc.def, tc.upstream, tc.key)
		if !bytes.Equal(out, fixture) {
			t.Fatalf("%s: body must stay byte-identical", tc.name)
		}
	}
}

// dashscopeFixture builds an OpenAI-format body whose cache_control
// markers land on the system part, three user parts, and (when n >= 5)
// the first tool definition.
func dashscopeFixture(n int) []byte {
	part := func(text string, marked bool) string {
		m := ""
		if marked {
			m = `,"cache_control": {"type": "ephemeral"}`
		}
		return `{"type": "text", "text": "` + text + `"` + m + `}`
	}
	msgs := `{"role": "system", "content": [` + part("s", n > 0) + `]}` +
		`,{"role": "user", "content": [` + part("a", n > 1) + `]}` +
		`,{"role": "user", "content": [` + part("b", n > 2) + `]}` +
		`,{"role": "user", "content": [` + part("c", n > 3) + `]}`
	tools := ""
	if n > 4 {
		tools = `,"tools": [{"type": "function", "cache_control": {"type": "ephemeral"}}, {"type": "function"}]`
	}
	return []byte(`{"model": "qwen", "messages": [` + msgs + `]` + tools + `}`)
}

// TestAnchorDashscopeCapsAtFourMarkers: at or below the documented
// 4-marker ceiling the body stays byte-identical; beyond it the oldest
// markers (message parts, walked before tools) are stripped until 4
// remain, keeping the newest conversation markers and the tool anchor.
func TestAnchorDashscopeCapsAtFourMarkers(t *testing.T) {
	def := claudeDef("dashscope-marker")
	// 4 markers: preserved verbatim, no re-encode.
	in4 := dashscopeFixture(4)
	if out := anchorCacheProfile(in4, "qwen", def, translat.FmtOpenAI, ""); !bytes.Equal(out, in4) {
		t.Fatal("4 markers must be preserved byte-identically")
	}
	// 5 markers: the oldest (system part) is stripped.
	in5 := dashscopeFixture(5)
	out := anchorCacheProfile(in5, "qwen", def, translat.FmtOpenAI, "")
	paths := markerPaths(t, out)
	if len(paths) != 4 {
		t.Fatalf("want 4 surviving markers, got %v", paths)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, ".messages[0]") {
			t.Fatalf("oldest marker (system part) must be stripped, got %v", paths)
		}
	}
	// The tool marker survives the cap.
	if !strings.Contains(strings.Join(paths, ","), ".tools[0]") {
		t.Fatalf("tool marker must survive the cap, got %v", paths)
	}
}

// TestAnchorStickyKey pins prompt_cache_key injection: injected when the
// session identity is non-empty, overwritten when stale, byte-identical
// when already correct or when the sessionKey is empty.
func TestAnchorStickyKey(t *testing.T) {
	def := claudeDef("sticky-key")
	body := []byte(`{"model": "m", "temperature": 0.5, "messages": [{"role": "user", "content": "hi"}]}`)
	out := anchorCacheProfile(body, "m", def, translat.FmtOpenAI, "sess-1")
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if root["prompt_cache_key"] != "sess-1" {
		t.Fatalf("prompt_cache_key = %v, want sess-1", root["prompt_cache_key"])
	}
	if !bytes.Contains(out, []byte("0.5")) {
		t.Fatal("temperature literal not preserved")
	}
	// Overwrite a stale key.
	stale := []byte(`{"model": "m", "prompt_cache_key": "old", "messages": []}`)
	out = anchorCacheProfile(stale, "m", def, translat.FmtOpenAI, "sess-1")
	if !bytes.Contains(out, []byte(`"prompt_cache_key":"sess-1"`)) {
		t.Fatalf("stale key must be overwritten: %s", out)
	}
	// Empty sessionKey skips injection entirely.
	if out := anchorCacheProfile(stale, "m", def, translat.FmtOpenAI, ""); !bytes.Equal(out, stale) {
		t.Fatal("empty sessionKey must leave the body untouched")
	}
}
