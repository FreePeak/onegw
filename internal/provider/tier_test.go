package provider

import "testing"

// Tier lookup (issue #54): exact model id wins over globs; globs follow
// path.Match ("*" does not cross "/"); a miss returns ok=false.
func TestDefTierLookup(t *testing.T) {
	def := &Def{
		Tiers: []ModelTier{
			{Model: "glm-5.3", Power: 110, Reasoning: true},
			{Model: "glm-5.3-flash", Power: 45},
			{Model: "claude-*", Power: 95, Vision: true, Context: 200_000},
		},
	}
	tests := []struct {
		model   string
		wantPow int
		wantVis bool
		wantOK  bool
	}{
		{"glm-5.3", 110, false, true}, // exact match (first table hit)
		{"glm-5.3-flash", 45, false, true},
		{"claude-sonnet-4-5", 95, true, true}, // glob
		{"claude/x/other", 0, false, false},   // "*" must not cross "/"
		{"unknown-model", 0, false, false},
		{"", 0, false, false},
	}
	for _, tc := range tests {
		got, ok := def.Tier(tc.model)
		if ok != tc.wantOK {
			t.Errorf("Tier(%q) ok = %v, want %v", tc.model, ok, tc.wantOK)
			continue
		}
		if ok && (got.Power != tc.wantPow || got.Vision != tc.wantVis) {
			t.Errorf("Tier(%q) = %+v, want power %d vision %v", tc.model, got, tc.wantPow, tc.wantVis)
		}
	}
}

// Exact match takes priority over an earlier glob that also fits.
func TestDefTierExactBeatsGlob(t *testing.T) {
	def := &Def{
		Tiers: []ModelTier{
			{Model: "claude-*", Power: 80},
			{Model: "claude-haiku-4-5", Power: 30},
		},
	}
	got, ok := def.Tier("claude-haiku-4-5")
	if !ok || got.Power != 30 {
		t.Fatalf("exact match must win: ok=%v %+v", ok, got)
	}
}

// A provider without tiers returns ok=false for everything — task
// routing then uses neutral defaults for its targets.
func TestDefTierEmpty(t *testing.T) {
	def := &Def{Name: "p"}
	if _, ok := def.Tier("anything"); ok {
		t.Fatal("empty Tiers must not match")
	}
}
