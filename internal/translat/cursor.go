package translat

// KindCursor is a skeleton for Cursor's AgentService protobuf-over-HTTP/2
// protocol (api5.cursor.sh). 9router's executor (open-sse/executors/cursor.js
// — read-only reference) speaks Connect-RPC frames wrapping hand-rolled
// agent.v1 protobuf messages, HTTP/2 only, with gzip-compressed frames and a
// checksum header computed by cursorChecksum.js. That subset is larger than
// the ~400-line budget for this issue, so the kind is declared so configs
// validate and fail fast with a clear error instead of being silently
// mis-served as openai-compatible; the full executor is tracked for a
// follow-up.
//
// Cursor skeleton — NOT a working executor yet. See the kind's doc in
// provider.go and the PR body status matrix.
const _ = "cursor-skeleton-placeholder"
