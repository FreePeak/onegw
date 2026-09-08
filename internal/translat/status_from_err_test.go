package translat

import (
	"testing"

	"onegw/internal/types"
)

// one-api proxies (b-ai family) answer HTTP 200 and deliver the real
// failure — often a 429 rate limit — as an error object mid-stream. The
// decoder must surface the real status so (a) the dashboard stops showing
// bare "502 upstream_error" for what is actually a per-key 429, and
// (b) retryable limits feed the account-cooldown ladder.
func TestInStreamErrorStatusExtraction(t *testing.T) {
	cases := []struct {
		name string
		data string
		want int
	}{
		{"numeric code", `{"error":{"code":429,"message":"limit"}}`, 429},
		{"numeric string code", `{"error":{"code":"503","message":"overloaded"}}`, 503},
		{"rate-limit text", `{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`, 429},
		{"insufficient quota", `{"error":{"code":"insufficient_quota","message":"quota"}}`, 429},
		{"unrecognized stays 502", `{"error":{"code":"boom","message":"something broke"}}`, 502},
		{"no code stays 502", `{"error":{"message":"something broke"}}`, 502},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs, err := decodeOpenAIStreamEvent(sseEvent{Data: []byte(c.data)})
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != 1 || evs[0].Kind != EvError {
				t.Fatalf("events: %+v", evs)
			}
			if got := evs[0].Err.Status; got != c.want {
				t.Fatalf("status = %d, want %d", got, c.want)
			}
		})
	}
}

// A 200 body carrying an error object (one-api style) maps the same way.
func TestBodyErrorStatusExtraction(t *testing.T) {
	_, err := DecodeOpenAIResponse([]byte(`{"error":{"code":"429","message":"rate limited"}}`))
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*types.APIError)
	if !ok {
		t.Fatalf("want *types.APIError, got %T", err)
	}
	if apiErr.Status != 429 {
		t.Fatalf("status = %d, want 429", apiErr.Status)
	}
}

// Grok responses-format stream errors carry their real code too.
func TestGrokStreamErrorStatusExtraction(t *testing.T) {
	_, err := decodeResponsesStreamEvent(sseEvent{Data: []byte(
		`{"type":"response.failed","response":{"error":{"code":"429","message":"too many requests"}}}`)},
		&responsesStreamState{})
	apiErr, ok := err.(*types.APIError)
	if !ok {
		t.Fatalf("want *types.APIError, got %v", err)
	}
	if apiErr.Status != 429 {
		t.Fatalf("status = %d, want 429", apiErr.Status)
	}
}
