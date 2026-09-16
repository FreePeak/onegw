package types

import "testing"

// Terminal billing classification (#80): the pool marks a credential out of
// rotation on these, so the set must be narrow — every false positive takes a
// working key out of service until an operator notices.
func TestPaymentRequiredClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *APIError
		want bool
	}{
		{"402 status", &APIError{Status: 402}, true},
		{"openai insufficient_quota code", &APIError{Status: 429, Code: "insufficient_quota"}, true},
		// Live 2026-09-12 experiential-labs: the canonical OpenAI-family
		// signal rides type while code carries the vendor's own token.
		// Reading code alone left the billing wall as a 10s rate-limit
		// cooldown, so the pool re-hit upstream on every request.
		{"insufficient_quota type + vendor card_required code", &APIError{Status: 429, Code: "card_required", Type: "insufficient_quota", Message: "Complete the $1 card verification to spend platform credits"}, true},
		{"balance wording", &APIError{Status: 400, Message: "Insufficient balance: top up your account"}, true},
		{"credits wording", &APIError{Status: 403, Message: "insufficient credits for this model"}, true},
		// Must NOT be terminal: the gated-403 deposit family keeps its #48
		// adaptive-ladder contract (the vendor clears on its own), and a
		// plain rate limit is a wait, not a billing state.
		{"gated 403 deposit family", &APIError{Status: 403, Code: "insufficient_user_quota", Message: "Deposit required"}, false},
		{"plain 429", &APIError{Status: 429, Code: "rate_limit_exceeded"}, false},
		{"403 model_access", &APIError{Status: 403, Code: "1211", Message: "Model access denied"}, false},
		{"nil", nil, false},
	} {
		if got := tc.err.PaymentRequired(); got != tc.want {
			t.Errorf("%s: PaymentRequired() = %v, want %v (%+v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// CreditWall is the subset of the above that names a top-uppable BALANCE. It
// gates the "bench the model, keep the account" path for providers whose free
// lane serves through a negative balance, so a false positive there would
// strand a whole account's worth of working traffic.
func TestCreditWallClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *APIError
		want bool
	}{
		// The measured vendor answer, verbatim (api.cline.bot 2026-09-16).
		{"cline insufficient_credits", &APIError{
			Status: 402, Code: "insufficient_credits", Type: "upstream_error",
			Message: "Insufficient balance. Your Cline Credits balance is $-0.01",
		}, true},
		{"credit balance wording", &APIError{Status: 402, Message: "your credit balance is too low"}, true},
		// A 402 with no balance wording stays PaymentRequired-only: the
		// #80 terminal read is the conservative default there.
		{"bare 402", &APIError{Status: 402}, false},
		{"402 key revoked wording", &APIError{Status: 402, Message: "key disabled by policy"}, false},
		// Same wording, wrong status: 403/429 belong to their own ladders.
		{"403 credits", &APIError{Status: 403, Code: "insufficient_credits"}, false},
		{"nil", nil, false},
	} {
		if got := tc.err.CreditWall(); got != tc.want {
			t.Errorf("%s: CreditWall() = %v, want %v (%+v)", tc.name, got, tc.want, tc.err)
		}
	}
}
