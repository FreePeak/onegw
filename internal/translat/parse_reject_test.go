package translat

import "testing"

// Regression (2026-09-09 live incident, 22 client-visible 400s in ~40 h):
// b-ai's one-api distributor fans each request to heterogeneous GLM
// backend nodes, and at least one node rejects large (>= ~228 KB) valid
// bodies with a bare "Invalid request body. (request id: …)" 400 —
// while byte-identical replays of the same bodies served 200 through
// other nodes minutes later. A body onegw itself parsed and rewrote is
// well-formed JSON, so this shape is the node's fault, not the client's:
// it must rewrite to a retryable 502-class fault so combos fall through
// instead of surfacing a lying invalid_request.
func TestUpstreamParseRejected(t *testing.T) {
	const live = "Invalid request body. (request id: 20260909040605170262326c955d568gwvECluW)"
	cases := []struct {
		name     string
		status   int
		typ, msg string
		want     bool
	}{
		{"live incident shape", 400, "api_error", live, true},
		{"client-side doubled rendering", 400, "api_error", live + "\n" + live, true},
		{"real schema 400 stays terminal", 400, "invalid_request_error",
			"The request is invalid: 该模型始终思考，不支持关闭思考；请使用 low、high 或 max。. Please check the request body, required fields, and request format.", false},
		{"named-param 400 stays terminal", 400, "invalid_request_error", "invalid parameter temperature", false},
		{"missing request-id trailer", 400, "api_error", "Invalid request body.", false},
		{"other type with same text", 400, "upstream_error", live, false},
		{"other status", 500, "api_error", live, false},
		{"empty", 400, "", "", false},
	}
	for _, tc := range cases {
		if got := UpstreamParseRejected(tc.status, tc.typ, tc.msg); got != tc.want {
			t.Errorf("%s: UpstreamParseRejected(%d,%q,%q)=%v, want %v", tc.name, tc.status, tc.typ, tc.msg, got, tc.want)
		}
	}
}
