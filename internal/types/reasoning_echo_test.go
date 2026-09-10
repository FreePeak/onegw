package types

import "testing"

// Live 2026-09-10 (commandcode/deepseek/deepseek-v4-flash, seqs 2455/2621/2812):
// 400 "The `reasoning_content` in the thinking mode must be passed back to the
// API." wrapped by the reseller's AI SDK as type AI_APICallError. The refusal
// is a deterministic body-contract verdict against THIS target — the combo
// must fall through instead of handing the client a terminal 400.
func TestReasoningEchoRequiredMatchesLivePayload(t *testing.T) {
	const live = "The `reasoning_content` in the thinking mode must be passed back to the API."
	e := &APIError{Status: 400, Type: "AI_APICallError", Message: live}
	if !e.ReasoningEchoRequired() {
		t.Fatalf("live reasoning-echo 400 must classify: %q", live)
	}
	// Word-order robustness for future paraphrases of the same demand.
	if !(&APIError{Status: 400, Message: "In thinking mode, reasoning_content must be passed back to the API."}).ReasoningEchoRequired() {
		t.Fatal("paraphrased echo demand must classify")
	}
}

// Guard against over-reach: the GLM reasoning_effort 400 family and generic
// invalid_request 400s keep their terminal contract; a non-400 never matches.
func TestReasoningEchoRequiredLeavesOther400sAlone(t *testing.T) {
	for _, e := range []*APIError{
		{Status: 400, Type: "invalid_request_error", Message: "reasoning_effort none is not supported; use low, high or max"},
		{Status: 400, Type: "invalid_request_error", Message: "Messages with role 'tool' must follow 'assistant'"},
		{Status: 400, Message: "The reasoning_content field must not be empty."},
		{Status: 500, Message: "The `reasoning_content` in the thinking mode must be passed back to the API."},
		{Status: 429, Message: "The `reasoning_content` in the thinking mode must be passed back to the API."},
	} {
		if e.ReasoningEchoRequired() {
			t.Fatalf("must NOT classify as reasoning-echo: %d %q", e.Status, e.Message)
		}
	}
}
