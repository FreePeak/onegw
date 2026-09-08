package translat

// Direction tests for the unified usage convention (issue #31): unified
// InputTokens is the TOTAL prompt size, cache-INCLUSIVE, with cache
// read/write as subsets. Every client-format × upstream-format pair must
// preserve the upstream's own prompt size:
//   - openai client: prompt_tokens is cache-inclusive
//   - anthropic client: input_tokens + cache_read + cache_creation
//   - gemini client: promptTokenCount is cache-inclusive
//
// All vendor shapes (issues #31/#33) decode to the same unified meaning:
// OpenAI (prompt_tokens_details), DeepSeek (prompt_cache_hit/miss_tokens),
// Kimi (top-level cached_tokens), Anthropic (exclusive input + cache
// fields), Gemini (promptTokenCount + cachedContentTokenCount), and OpenAI
// Responses (input_tokens_details).

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/types"
)

// usageCase is one upstream shape with its known numbers: prompt total
// (cache-inclusive), output, cached read, cache write.
type usageCase struct {
	name          string
	format        Format
	body          string // non-streaming response body
	sse           string // streaming fixture (empty = non-streaming only)
	in, out, cr   int64
	cw, reasoning int64
}

const (
	// promptSize is the same total prompt size (100) across every fixture;
	// promptDelta is how many of those tokens were cache hits (30). The
	// anthropic fixture reports 70 exclusive + 30 cached read, and variants
	// add cache write where the vendor has one.
	promptSize = int64(100)
	outputSize = int64(20)
	cacheRead  = int64(30)
)

var usageFixtures = []usageCase{
	{
		name: "openai", format: FmtOpenAI,
		body: `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`,
		sse: joinSSE(
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
			``,
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`,
			``,
			`data: [DONE]`,
			``,
		),
		in: promptSize, out: outputSize, cr: cacheRead,
	},
	{
		name: "deepseek", format: FmtOpenAI,
		body: `{"id":"c2","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}}`,
		in:   promptSize, out: outputSize, cr: cacheRead,
	},
	{
		name: "kimi", format: FmtOpenAI,
		body: `{"id":"c3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"cached_tokens":30}}`,
		in:   promptSize, out: outputSize, cr: cacheRead,
	},
	{
		name: "anthropic", format: FmtAnthropic,
		body: `{"id":"m1","role":"assistant","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn","usage":{"input_tokens":70,"cache_read_input_tokens":30,"cache_creation_input_tokens":15,"output_tokens":20}}`,
		sse: joinSSE(
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[],"usage":{"input_tokens":70,"cache_read_input_tokens":30,"cache_creation_input_tokens":15,"output_tokens":1}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":20}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		),
		in: promptSize + 15, out: outputSize, cr: cacheRead, cw: 15,
	},
	{
		name: "gemini", format: FmtGemini,
		body: `{"candidates":[{"content":{"parts":[{"text":"Hi"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"totalTokenCount":125,"cachedContentTokenCount":30,"thoughtsTokenCount":5}}`,
		sse: joinSSE(
			`data: {"candidates":[{"content":{"parts":[{"text":"Hi"}],"role":"model"},"index":0}]}`,
			``,
			`data: {"candidates":[{"content":{"parts":[],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"totalTokenCount":125,"cachedContentTokenCount":30,"thoughtsTokenCount":5}}`,
			``,
		),
		in: promptSize, out: outputSize, cr: cacheRead, reasoning: 5,
	},
	{
		name: "responses", format: FmtResponses,
		body: `{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}}}`,
		sse: joinSSE(
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"r1","model":"m"}}`,
			``,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":"Hi"}`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}}}}`,
			``,
		),
		in: promptSize, out: outputSize, cr: cacheRead, reasoning: 5,
	},
	{
		// Grok CLI's Responses dialect (FmtOpenAIResponses).
		name: "grok", format: FmtOpenAIResponses,
		sse: joinSSE(
			`data: {"type":"response.created","response":{"id":"g1","model":"m"}}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"Hi"}`,
			``,
			`data: {"type":"response.completed","response":{"id":"g1","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":5}}}}`,
			``,
		),
		in: promptSize, out: outputSize, cr: cacheRead, reasoning: 5,
	},
	{
		// CommandCode: usage carries no cache fields.
		name: "commandcode", format: FmtCommandCode,
		sse: joinSSE(
			`{"type":"start","messageId":"cmpl-1"}`,
			`{"type":"start-step"}`,
			`{"type":"text-start","id":"t1"}`,
			`{"type":"text-delta","text":"Hi"}`,
			`{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":100,"outputTokens":20}}`,
			`{"type":"finish"}`,
		),
		in: promptSize, out: outputSize,
	},
}

func joinSSE(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// clientPromptTotal extracts the prompt size a client surface reports in its
// own wire convention.
func clientPromptTotal(t *testing.T, client Format, body string) int64 {
	t.Helper()
	var total float64
	switch client {
	case FmtOpenAI:
		// Works for both a non-streaming body (bare JSON) and a stream
		// (last usage chunk wins).
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "data: ")
			if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
				continue
			}
			var m struct {
				Usage *struct {
					PromptTokens float64 `json:"prompt_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(line), &m) == nil && m.Usage != nil {
				total = m.Usage.PromptTokens
			}
		}
	case FmtAnthropic:
		// Anthropic convention: input_tokens EXCLUDES cache read/write.
		// Track the latest values across message_start + message_delta.
		var input, cr, cw float64
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "data: ")
			if line == "" || !strings.HasPrefix(line, "{") {
				continue
			}
			var m struct {
				Type    string `json:"type"`
				Message *struct {
					Usage *struct {
						InputTokens              float64 `json:"input_tokens"`
						CacheReadInputTokens     float64 `json:"cache_read_input_tokens"`
						CacheCreationInputTokens float64 `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Usage *struct {
					InputTokens              float64 `json:"input_tokens"`
					CacheReadInputTokens     float64 `json:"cache_read_input_tokens"`
					CacheCreationInputTokens float64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			if m.Message != nil && m.Message.Usage != nil {
				input, cr, cw = m.Message.Usage.InputTokens, m.Message.Usage.CacheReadInputTokens, m.Message.Usage.CacheCreationInputTokens
			} else if m.Usage != nil {
				// message_delta or a non-streaming response body: final
				// cumulative values.
				if m.Usage.InputTokens > 0 {
					input = m.Usage.InputTokens
				}
				if m.Usage.CacheReadInputTokens > 0 {
					cr = m.Usage.CacheReadInputTokens
				}
				if m.Usage.CacheCreationInputTokens > 0 {
					cw = m.Usage.CacheCreationInputTokens
				}
			}
		}
		total = input + cr + cw
	case FmtGemini:
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "data: ")
			if line == "" || !strings.HasPrefix(line, "{") {
				continue
			}
			var m struct {
				UsageMetadata *struct {
					PromptTokenCount float64 `json:"promptTokenCount"`
				} `json:"usageMetadata"`
			}
			if json.Unmarshal([]byte(line), &m) == nil && m.UsageMetadata != nil {
				total = m.UsageMetadata.PromptTokenCount
			}
		}
	default:
		t.Fatalf("no client prompt total for format %s", client)
	}
	return int64(total)
}

// TestUsageDirectionNonStream pins the prompt-size preservation for every
// (upstream shape × client surface) pair through the buffered path.
func TestUsageDirectionNonStream(t *testing.T) {
	for _, f := range usageFixtures {
		if f.body == "" {
			continue // stream-only fixture (grok, commandcode)
		}
		for _, client := range []Format{FmtOpenAI, FmtAnthropic, FmtGemini} {
			t.Run(f.name+"->"+string(client), func(t *testing.T) {
				resp, err := DecodeResponse(f.format, []byte(f.body))
				if err != nil {
					t.Fatal(err)
				}
				u := resp.Usage
				if u.InputTokens != f.in || u.OutputTokens != f.out ||
					u.CacheReadTokens != f.cr || u.CacheWriteTokens != f.cw || u.ReasoningTokens != f.reasoning {
					t.Fatalf("unified usage not normalized: got %+v, want in=%d out=%d cr=%d cw=%d rs=%d",
						u, f.in, f.out, f.cr, f.cw, f.reasoning)
				}
				if u.InputTokens < u.CacheReadTokens+u.CacheWriteTokens {
					t.Fatalf("cache subsets exceed inclusive input: %+v", u)
				}
				out, err := EncodeResponse(client, resp)
				if err != nil {
					t.Fatal(err)
				}
				if total := clientPromptTotal(t, client, string(out)); total != f.in {
					t.Fatalf("client-visible prompt size %d != upstream total %d; body=%s", total, f.in, out)
				}
			})
		}
	}
}

// TestUsageDirectionStream pins the same preservation through the SSE
// translate path (decoders + stream encoders), asserting both the returned
// unified usage and the client-visible prompt size.
func TestUsageDirectionStream(t *testing.T) {
	for _, f := range usageFixtures {
		if f.sse == "" {
			continue
		}
		for _, client := range []Format{FmtOpenAI, FmtAnthropic, FmtGemini} {
			t.Run(f.name+"->"+string(client), func(t *testing.T) {
				var sb strings.Builder
				usage, err := TranslateStream(strings.NewReader(f.sse), &sb, nil, f.format, client, "m")
				if err != nil {
					t.Fatal(err)
				}
				if usage.InputTokens != f.in || usage.OutputTokens != f.out ||
					usage.CacheReadTokens != f.cr || usage.CacheWriteTokens != f.cw || usage.ReasoningTokens != f.reasoning {
					t.Fatalf("streamed unified usage: got %+v, want in=%d out=%d cr=%d cw=%d rs=%d",
						usage, f.in, f.out, f.cr, f.cw, f.reasoning)
				}
				if total := clientPromptTotal(t, client, sb.String()); total != f.in {
					t.Fatalf("client-visible stream prompt size %d != upstream total %d; body=%s", total, f.in, sb.String())
				}
			})
		}
	}
}

// TestGeminiTotalTokenCountIncludesCacheWrite: unified InputTokens is
// cache-inclusive, so the encoded totalTokenCount covers cache writes
// without adding subsets on top.
func TestGeminiTotalTokenCountIncludesCacheWrite(t *testing.T) {
	resp := &types.ChatResponse{Model: "m", Usage: types.Usage{
		InputTokens: 115, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 15,
	}}
	out, err := EncodeGeminiResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		UsageMetadata struct {
			PromptTokenCount int64 `json:"promptTokenCount"`
			TotalTokenCount  int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m.UsageMetadata.PromptTokenCount != 115 {
		t.Fatalf("promptTokenCount %d, want 115", m.UsageMetadata.PromptTokenCount)
	}
	if m.UsageMetadata.TotalTokenCount != 115+20 {
		t.Fatalf("totalTokenCount %d, want %d (must include cache write via inclusive input)", m.UsageMetadata.TotalTokenCount, 115+20)
	}
}
