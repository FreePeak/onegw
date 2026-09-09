package translat

import (
	"encoding/json"
	"testing"

	"onegw/internal/types"
)

// Regression (2026-09-09 10:52 live incident): b-ai's one-api proxies the
// gateway bearer token through an INTERNAL auth/verify service on every
// request. When that service hiccups ("read tcp ...: connection reset"),
// the proxy answers HTTP 401 with the transport error text — the gateway
// key is fine and the next request succeeds. Surfacing a terminal 401 to
// clients killed combo fall-through for a transient upstream fault.
func TestUpstreamAuthVerifyFailed(t *testing.T) {
	const live = "鉴权服务请求失败: Post \"http://ainft-chat-service.apenft-market-production.svc.cluster.local:3210/v1/internal/auth/verify\": read tcp 172.31.74.66:59496->10.100.219.47:3210: read: connection reset by peer"
	cases := []struct {
		name     string
		status   int
		typ, msg string
		want     bool
	}{
		{"live incident shape", 401, "upstream_error", live, true},
		{"english mirror", 401, "upstream_error", `Post "https://cdn.aiproxy.io/v1/internal/auth/verify": dial tcp: i/o timeout`, true},
		{"real invalid key stays 401", 401, "authentication_error", "Invalid API key provided", false},
		{"other status with verify text", 500, "upstream_error", "auth/verify exploded", false},
		{"unrelated 401", 401, "authentication_error", "token expired", false},
	}
	for _, tc := range cases {
		if got := UpstreamAuthVerifyFailed(tc.status, tc.typ, tc.msg); got != tc.want {
			t.Errorf("%s: UpstreamAuthVerifyFailed(%d,%q,%q)=%v, want %v", tc.name, tc.status, tc.typ, tc.msg, got, tc.want)
		}
	}
}

func TestSharedConcurrencySignature(t *testing.T) {
	const live = "The request rate exceeds the current model Concurrency limit 1200. Please reduce the request frequency or contact Tencent Cloud support to request a higher limit."
	if !(&types.APIError{Status: 429, Message: live}).SharedConcurrency() {
		t.Fatal("live concurrency-limit 429 must classify as shared")
	}
	// Live 2026-09-09: new-api/z-ai engine cold-prefill admission wall.
	const admission = "BackendAdmissionRejected: Engine cold-request admission rejected: dp_rank=0, policies=prefill_pressure, queued_uncached_tokens=0, inflight_uncached_tokens=0, outstanding_uncached_tokens=0, incoming_uncached_tokens=214293, pending_uncached_prefill_tokens=214293"
	if !(&types.APIError{Status: 429, Message: admission}).SharedConcurrency() {
		t.Fatal("live engine admission-rejected 429 must classify as shared")
	}
	negatives := []types.APIError{
		{Status: 429, Message: "rate limit exceeded for key sk-xxx"},
		{Status: 429, Code: "insufficient_quota", Message: "quota exhausted"},
		{Status: 429, Message: ""}, // empty
		// Guard the 503 substring probes against over-widening: a plain
		// upstream 503 must stay ordinary-retryable, never shared-wall.
		{Status: 503, Message: "down"},
		{Status: 503, Message: "overloaded upstream, try another endpoint"},
	}
	for i, e := range negatives {
		if e.SharedConcurrency() {
			t.Errorf("negative %d (%d %q) must not classify as shared", i, e.Status, e.Message)
		}
	}
	// Engine admission walls also surface as 503 (new-api/z-ai
	// aggregators, live 2026-09-09): same shared-wall medicine.
	for i, e := range []types.APIError{
		{Status: 503, Message: "cache-only admission rejected a cold, unavailable, or overloaded request"},
		{Status: 503, Message: "gateway overloaded: hard concurrency limit reached"},
		{Status: 503, Message: "gateway overloaded: cache-aware admission is unavailable"},
	} {
		if !e.SharedConcurrency() {
			t.Errorf("positive 503 %d (%q) must classify as shared", i, e.Message)
		}
	}
}

// The in-stream variant: one-api proxies can deliver the same auth-verify
// outage as an error object inside a 200 body / mid-stream chunk. The
// normalization must apply there too — otherwise the HTTP-level downgrade
// in provider.Do is bypassed on streaming paths.
func TestInStreamAuthVerify401Normalized(t *testing.T) {
	const live = "鉴权服务请求失败: Post \"http://ainft-chat-service.apenft-market-production.svc.cluster.local:3210/v1/internal/auth/verify\": read tcp 172.31.74.66:59496->10.100.219.47:3210: read: connection reset by peer"

	// In-200 body (DecodeOpenAIResponse).
	_, err := DecodeOpenAIResponse([]byte(`{"error":{"code":401,"message":` + quoteJSON(live) + `,"type":"upstream_error"}}`))
	apiErr, ok := err.(*types.APIError)
	if !ok {
		t.Fatalf("want *types.APIError, got %T", err)
	}
	if apiErr.Status != 502 || apiErr.Type != "upstream_auth_verify_failed" {
		t.Fatalf("in-200 verify outage: got %d/%s, want 502/upstream_auth_verify_failed", apiErr.Status, apiErr.Type)
	}
	if !apiErr.Retryable() {
		t.Fatal("in-200 verify outage must be retryable")
	}

	// Mid-stream chunk (decodeOpenAIStreamEvent).
	events, err := decodeOpenAIStreamEvent(sseEvent{Data: []byte(`{"error":{"code":"401","message":` + quoteJSON(live) + `,"type":"upstream_error"}}`)})
	if err != nil || len(events) != 1 || events[0].Kind != EvError || events[0].Err == nil {
		t.Fatalf("chunk decode: events=%+v err=%v", events, err)
	}
	if events[0].Err.Status != 502 || events[0].Err.Type != "upstream_auth_verify_failed" {
		t.Fatalf("mid-stream verify outage: got %d/%s, want 502/upstream_auth_verify_failed", events[0].Err.Status, events[0].Err.Type)
	}

	// A real invalid-key 401 delivered in-band keeps its terminal status.
	_, err = DecodeOpenAIResponse([]byte(`{"error":{"code":401,"message":"Invalid API key","type":"authentication_error"}}`))
	apiErr, ok = err.(*types.APIError)
	if !ok || apiErr.Status != 401 {
		t.Fatalf("real in-band invalid-key 401 must stay 401, got %T %+v", err, apiErr)
	}
}

// quoteJSON marshals s as a JSON string literal.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The distributor parse-reject shape (peer RCA 84fd1c9) delivered
// in-stream: same terminal-400 defect as the HTTP-level path, same
// rewrite expected through the in-band normalizer.
func TestInStreamParseRejectNormalized(t *testing.T) {
	const live = "Invalid request body. (request id: 20260909040605170262326c955d568gwvECluW)"
	_, err := DecodeOpenAIResponse([]byte(`{"error":{"code":400,"type":"api_error","message":` + quoteJSON(live) + `}}`))
	apiErr, ok := err.(*types.APIError)
	if !ok {
		t.Fatalf("want *types.APIError, got %T", err)
	}
	if apiErr.Status != 502 || apiErr.Type != "upstream_parse_rejected" {
		t.Fatalf("in-200 parse reject: got %d/%s, want 502/upstream_parse_rejected", apiErr.Status, apiErr.Type)
	}
	if !apiErr.Retryable() {
		t.Fatal("in-stream parse reject must be retryable")
	}
	// A genuine schema 400 in-band stays terminal.
	_, err = DecodeOpenAIResponse([]byte(`{"error":{"code":400,"type":"invalid_request_error","message":"invalid parameter temperature"}}`))
	apiErr, ok = err.(*types.APIError)
	if !ok || apiErr.Status != 400 {
		t.Fatalf("real in-band schema 400 must stay 400, got %T %+v", err, apiErr)
	}
}
