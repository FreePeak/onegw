package translat

import (
	"strings"
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
	if err ***REMOVED*** nil {
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

// Live 2026-09-17 (openrouter/stealth/union-alpha): OpenRouter's 429 says
// only "Provider returned error" at the top level and nests the actual
// diagnostic — the vendor's words plus the lane that gated it — under
// error.metadata. Before the fold, ALL THREE decode paths handed
// types.APIError a Message byte-identical to a genuine per-key 429, so
// SharedConcurrency() could not tell them apart and the harness benched a
// healthy key for the upstream's shared-pool wall. The fold must be
// observable on every path the gateway reads errors from.
func TestOpenRouterNestedMetadataSurvivesDecode(t *testing.T) {
	const body = `{"error":{"message":"Provider returned error","code":429,"metadata":{"raw":"stealth/union-alpha is temporarily rate-limited upstream. Please retry shortly.","provider_name":"Stealth","limit_source":"upstream_provider_shared_pool","remedy_hint":"Retry shortly or route to another provider"}}}`
	const raw = "stealth/union-alpha is temporarily rate-limited upstream. Please retry shortly."
	const lane = "upstream_provider_shared_pool"

	cases := []struct {
		name string
		got  func() *types.APIError
	}{
		{"http error body", func() *types.APIError {
			return DecodeOpenAIError([]byte(body), 429)
		}},
		{"200 body error object", func() *types.APIError {
			_, err := DecodeOpenAIResponse([]byte(body))
			e, _ := err.(*types.APIError)
			return e
		}},
		{"mid-stream error event", func() *types.APIError {
			evs, err := decodeOpenAIStreamEvent(sseEvent{Data: []byte(body)})
			if err != nil || len(evs) != 1 {
				return nil
			}
			return evs[0].Err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := c.got()
			if e ***REMOVED*** nil {
				t.Fatal("no error decoded")
			}
			if !strings.Contains(e.Message, raw) || !strings.Contains(e.Message, lane) {
				t.Fatalf("nested metadata lost: %q", e.Message)
			}
			if !e.SharedConcurrency() {
				t.Fatalf("shared-pool 429 must classify as shared, got %+v", e)
			}
		})
	}

	// The fold is strictly additive: an upstream that nests nothing keeps
	// its message verbatim (every other vendor's decode path).
	if e := DecodeOpenAIError([]byte(`{"error":{"message":"Invalid API key","type":"authentication_error","code":401}}`), 401); e.Message != "Invalid API key" {
		t.Fatalf("metadata-less error must keep its message, got %q", e.Message)
	}
}
