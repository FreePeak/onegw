package saver

import (
	"encoding/json"
	"hash/fnv"
	"io"

	"onegw/internal/translat"
)

// Per-conversation stickiness state (issue #35). The compress-or-raw
// gate in raw.go decides for the whole body and is not monotonic: a
// request whose compressible content shrinks (or whose re-encode
// overhead grows) relative to the previous turn flips the entire body
// from canonical (re-encoded) form back to the client's raw form —
// re-ordering every key and busting the whole upstream implicit-cache
// prefix. Once a conversation has been sent canonical it stays
// canonical, and per-block fingerprints let below-floor blocks keep
// their compressed form across turns. State is bounded: at most
// convCap conversations with convFPCap fingerprints each; overflow
// resets the table (the next compressed turn re-establishes
// stickiness), which stays far inside the process memory contract.

const (
	convCap    = 4096 // tracked conversations
	convFPCap  = 512  // remembered blocks per conversation
	convFPHead = 256  // fingerprint window (bytes)
)

// convEntry is the stickiness record for one conversation. Presence in
// the table means the conversation has already been sent in canonical
// (re-encoded) form.
type convEntry struct {
	fps map[uint64]struct{}
}

func (c *convEntry) known(fp uint64) bool {
	_, ok := c.fps[fp]
	return ok
}

func (c *convEntry) record(fp uint64) {
	if c.fps == nil {
		c.fps = make(map[uint64]struct{}, 8)
	}
	if _, ok := c.fps[fp]; ok {
		return
	}
	if len(c.fps) >= convFPCap {
		c.fps = make(map[uint64]struct{}, 8) // bounded: drop the old window
	}
	c.fps[fp] = struct{}{}
}

// convGet returns the conversation entry for key, or nil when the
// conversation has never been sent canonical.
func (s *Saver) convGet(key uint64) *convEntry {
	if key == 0 {
		return nil
	}
	s.convMu.Lock()
	defer s.convMu.Unlock()
	return s.convs[key]
}

// convRecord marks the conversation as canonical-sent and remembers the
// fingerprints of blocks compressed this pass.
func (s *Saver) convRecord(key uint64, seen []uint64) {
	if key == 0 {
		return
	}
	s.convMu.Lock()
	defer s.convMu.Unlock()
	if s.convs == nil {
		s.convs = make(map[uint64]*convEntry)
	}
	if _, ok := s.convs[key]; !ok && len(s.convs) >= convCap {
		s.convs = make(map[uint64]*convEntry) // bounded: reset on overflow
	}
	ce := s.convs[key]
	if ce == nil {
		ce = &convEntry{}
		s.convs[key] = ce
	}
	for _, fp := range seen {
		ce.record(fp)
	}
}

// headFP fingerprints a tool-result block by its first convFPHead bytes:
// clients replay history verbatim but may append to older tool output
// (truncation markers), so the stable head — not the whole text — is the
// cross-turn block identity.
func headFP(text string) uint64 {
	if len(text) > convFPHead {
		text = text[:convFPHead]
	}
	h := fnv.New64a()
	io.WriteString(h, text)
	return h.Sum64()
}

// conversationKey fingerprints a conversation from its opening: clients
// replay the full history every turn, so the system field plus the
// first message are byte-stable across the turns of one conversation
// while differing across conversations. Returns 0 when no opening can
// be identified (stickiness then stays off — plain unconditional gate).
func conversationKey(format translat.Format, root map[string]any) uint64 {
	h := fnv.New64a()
	switch format {
	case translat.FmtOpenAI:
		if !hashJSONValue(h, firstMap(root["messages"])) {
			return 0
		}
	case translat.FmtAnthropic:
		if sys, ok := root["system"]; ok {
			if !hashJSONValue(h, sys) {
				return 0
			}
		}
		if !hashJSONValue(h, firstMap(root["messages"])) {
			return 0
		}
	case translat.FmtGemini:
		if si, ok := root["systemInstruction"]; ok {
			if !hashJSONValue(h, si) {
				return 0
			}
		}
		if !hashJSONValue(h, firstMap(root["contents"])) {
			return 0
		}
	default:
		return 0
	}
	return h.Sum64()
}

func firstMap(v any) map[string]any {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	m, _ := arr[0].(map[string]any)
	return m
}

// hashJSONValue hashes the canonical JSON encoding of v (json.Number
// literals verbatim, map keys sorted). Reports success; a nil or
// unmarshalable value leaves the fingerprint unidentified.
func hashJSONValue(h io.Writer, v any) bool {
	if v == nil {
		return false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	_, err = h.Write(b)
	return err == nil
}
