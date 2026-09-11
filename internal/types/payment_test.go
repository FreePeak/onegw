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
