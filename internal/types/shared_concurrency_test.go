package types

import "testing"

// Live 2026-09-10 11:10-11:15 (b-ai/glm-5.3-flash): the reseller's per-MODEL
// TPM wall struck two to three different accounts in the SAME second while a
// fourth account served 200s on the same model — the budget belongs to the
// reseller's lane (Tencent APPID-wide), not to any single key. Classifying it
// as a per-key limit benched healthy credentials on the 10-60s ladder and
// burned a same-target retry into the saturated model on every request.
func TestSharedConcurrencyMatchesModelTPMWall(t *testing.T) {
	const tpm = "The request rate exceeds the current model TPM limit 340000000. Please reduce the request frequency or contact Tencent Cloud support to request a higher limit."
	const concurrency = "The request rate exceeds the current model Concurrency limit 1200. Please reduce the request frequency or contact Tencent Cloud support to request a higher limit."
	for _, body := range []string{tpm, concurrency} {
		if !(&APIError{Status: 429, Type: "upstream_error", Code: "1302", Message: body}).SharedConcurrency() {
			t.Fatalf("model-limit wall must classify as shared: %q", body)
		}
	}
	// Any future "current model <X> limit" variant rides the same medicine.
	if !(&APIError{Status: 429, Message: "The request rate exceeds the current model RPM limit 5000."}).SharedConcurrency() {
		t.Fatal("unseen model-limit variant must classify as shared")
	}
}

// Guard against over-reach: a plain per-key 429 keeps the account ladder.
func TestSharedConcurrencyLeavesPerKey429Alone(t *testing.T) {
	for _, body := range []string{
		"rate limit exceeded, key sk-x",
		"您的账户已达到速率限制", // b-ai's per-account limit (Chinese body)
		"You have reached the request limit[z-ai/glm-5.3-free]: Maximum 8 requests within 1 minutes.",
	} {
		if (&APIError{Status: 429, Type: "upstream_error", Message: body}).SharedConcurrency() {
			t.Fatalf("per-key 429 must NOT classify as shared: %q", body)
		}
	}
	if (&APIError{Status: 400, Message: "The request rate exceeds the current model TPM limit 1."}).SharedConcurrency() {
		t.Fatal("only 429/503 walls classify as shared")
	}
}
