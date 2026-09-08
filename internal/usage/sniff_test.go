package usage

import (
	"io"
	"strings"
	"testing"
)

// TestSniffVendorUsageShapes pins the six vendor usage shapes through the
// sniffer (issues #31/#33). The sniffer must report unified-convention
// counts: InputTokens is the TOTAL prompt size, cache-INCLUSIVE; cache
// read/write are subsets. Only Anthropic-shaped payloads report an
// exclusive input_tokens, so their cache values are folded in at Usage().
func TestSniffVendorUsageShapes(t *testing.T) {
	cases := []struct {
		name                string
		body                string
		in, out, cr, cw, rs int64
	}{
		{
			// OpenAI chat completions: prompt_tokens includes cached_tokens.
			name: "openai",
			body: `{"id":"c1","usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":30}}}`,
			in:   100, out: 20, cr: 30,
		},
		{
			// DeepSeek: prompt_tokens = hit + miss (already inclusive); the
			// hit is the cached subset.
			name: "deepseek",
			body: `{"id":"c2","usage":{"prompt_tokens":115,"completion_tokens":20,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":85}}`,
			in:   115, out: 20, cr: 30,
		},
		{
			// Kimi/Moonshot: top-level cached_tokens subset.
			name: "kimi",
			body: `{"id":"c3","usage":{"prompt_tokens":100,"completion_tokens":20,"cached_tokens":30}}`,
			in:   100, out: 20, cr: 30,
		},
		{
			// Anthropic: input_tokens EXCLUDES cache read/write; the sniffer
			// must fold both into the inclusive input (70+30+15=115).
			name: "anthropic",
			body: `{"id":"m1","usage":{"input_tokens":70,"cache_read_input_tokens":30,"cache_creation_input_tokens":15,"output_tokens":20}}`,
			in:   115, out: 20, cr: 30, cw: 15,
		},
		{
			// Anthropic with zero cache fields still folds (no-op) and must
			// not be confused by an absent cache section.
			name: "anthropic-nocache",
			body: `{"id":"m2","usage":{"input_tokens":100,"output_tokens":20}}`,
			in:   100, out: 20,
		},
		{
			// Gemini: promptTokenCount includes cachedContentTokenCount.
			name: "gemini",
			body: `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"totalTokenCount":125,"cachedContentTokenCount":30,"thoughtsTokenCount":5}}`,
			in:   100, out: 20, cr: 30, rs: 5,
		},
		{
			// OpenAI Responses: input_tokens includes the cached subset;
			// must NOT be folded a second time.
			name: "responses",
			body: `{"id":"r1","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}}}`,
			in:   100, out: 20, cr: 30, rs: 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sn := NewSniffer(strings.NewReader(tc.body), 0)
			if _, err := io.Copy(io.Discard, sn); err != nil {
				t.Fatal(err)
			}
			in, out, cr, cw, rs, seen := sn.Usage()
			if !seen {
				t.Fatal("usage marker not seen")
			}
			if in != tc.in || out != tc.out || cr != tc.cr || cw != tc.cw || rs != tc.rs {
				t.Fatalf("got (in=%d out=%d cr=%d cw=%d rs=%d), want (in=%d out=%d cr=%d cw=%d rs=%d)",
					in, out, cr, cw, rs, tc.in, tc.out, tc.cr, tc.cw, tc.rs)
			}
		})
	}
}

// Anthropic SSE streams: message_start carries the exclusive input and cache
// counts early; later deltas must not corrupt the folded total.
func TestSniffAnthropicStreamNormalization(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[],"usage":{"input_tokens":70,"cache_read_input_tokens":30,"cache_creation_input_tokens":15,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	sn := NewSniffer(strings.NewReader(body), 0)
	if _, err := io.Copy(io.Discard, sn); err != nil {
		t.Fatal(err)
	}
	in, out, cr, cw, _, _ := sn.Usage()
	if in != 115 || out != 20 || cr != 30 || cw != 15 {
		t.Fatalf("normalized stream usage: in=%d out=%d cr=%d cw=%d, want 115/20/30/15", in, out, cr, cw)
	}
}
