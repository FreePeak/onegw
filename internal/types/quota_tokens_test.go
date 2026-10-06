package types

import "testing"

// Quota charging follows the vendor's own total: OpenAI's reasoning_tokens
// is a breakdown of the output count, xAI/Gemini report reasoning on top.
// Without a total the historical input+output+reasoning sum stays.
func TestUsageQuotaTokens(t *testing.T) {
	cases := []struct {
		name string
		u    Usage
		want int64
	}{
		{"openai total excludes double reasoning", Usage{InputTokens: 50, OutputTokens: 30, ReasoningTokens: 20, TotalTokens: 80}, 80},
		{"xai total adds reasoning on top", Usage{InputTokens: 50, OutputTokens: 30, ReasoningTokens: 20, TotalTokens: 100}, 100},
		{"no total keeps the conservative sum", Usage{InputTokens: 50, OutputTokens: 30, ReasoningTokens: 20}, 100},
		{"total below input+output is ignored", Usage{InputTokens: 50, OutputTokens: 30, TotalTokens: 10}, 80},
	}
	for _, c := range cases {
		if got := c.u.QuotaTokens(); got != c.want {
			t.Errorf("%s: QuotaTokens() = %d, want %d", c.name, got, c.want)
		}
	}
}
