package provider

// Per-kind wire helpers for the custom-format executors (commandcode,
// openai-responses). Kept out of provider.go to leave the shared plumbing
// file untouched beyond the dispatch cases.

import (
	"crypto/rand"
	"fmt"
)

// ForcedStream reports whether the upstream only answers in streaming mode,
// regardless of what the client asked for (CommandCode /alpha/generate and
// Grok CLI /responses have no non-streaming shape). The server passes
// stream=true upstream for these kinds and aggregates the stream back into
// one completion for non-streaming clients.
func (k Kind) ForcedStream() bool {
	switch k {
	case KindCommandCode, KindOpenAIResponses:
		return true
	default:
		return false
	}
}

// newRequestUUID returns a fresh RFC 4122 v4 UUID for per-request session /
// request id headers (x-session-id, x-grok-req-id, ...).
func newRequestUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
