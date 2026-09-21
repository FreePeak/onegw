package server

import (
	"strings"
	"testing"

	"onegw/internal/translat"
)

// TestJunkReasoningWiring pins the relay wiring and the fixture the
// streaming tests serve.
//
// The verdict rule itself is pinned at its own layer
// (translat.TestJunkGuardPrefetchFailsOverJunkReasoning); what this test
// guards is that the SERVER's fixture still reaches that verdict after any
// change to the threshold, and that the hold window the relay passes to the
// guard is the failover-capable one.
func TestJunkReasoningWiring(t *testing.T) {
	if junkHoldBytes <= 0 || corruptHoldBytes <= 0 {
		t.Fatalf("hold windows must be positive: junk=%d corrupt=%d", junkHoldBytes, corruptHoldBytes)
	}
	// The stub's reasoning fragment, repeated to the length a real run
	// reaches before the upstream's noise is visible.
	if !translat.JunkReasoningForTest(strings.Repeat(junkReasoningStreamFrag, 3)) {
		t.Fatal("the fixture the streaming tests serve no longer reaches the junk verdict")
	}
	// And the clean fixture those same tests rely on must stay clean.
	if translat.JunkReasoningForTest(strings.Repeat("Reading app.go to find the render path, then I will check the tests. ", 20)) {
		t.Fatal("healthy reasoning must never reach the junk verdict")
	}
}
