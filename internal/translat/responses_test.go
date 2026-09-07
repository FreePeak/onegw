package translat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/types"
)

func TestEncodeResponsesRequestBasics(t *testing.T) {
	u := &types.ChatRequest{
		Model:     "grok-4.6",
		System:    []types.Part{{Type: types.PartText, Text: "be terse"}},
		Messages:  []types.Message{{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "hi"}}}},
		Stream:    true,
		MaxTokens: 512,
	}
	body, err := EncodeResponsesRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var req rsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if req.Model != "grok-4.6" || !req.Stream {
		t.Fatalf("model/stream: %+v", req)
	}
	if req.Instructions != "be terse" {
		t.Fatalf("instructions = %q", req.Instructions)
	}
	if req.MaxOutputTokens == nil || *req.MaxOutputTokens != 512 {
		t.Fatalf("max_output_tokens = %v", req.MaxOutputTokens)
	}
	if req.Store {
		t.Fatal("store must be false")
	}
	var items []rsItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		t.Fatalf("input not an item array: %v", err)
	}
	if len(items) != 1 || items[0].Role != "user" {
		t.Fatalf("input items: %+v", items)
	}
	var content []rsContent
	if err := json.Unmarshal(items[0].Content, &content); err != nil || content[0].Type != "input_text" || content[0].Text != "hi" {
		t.Fatalf("user content: %s err=%v", items[0].Content, err)
	}
}

func TestEncodeResponsesRequestToolFlow(t *testing.T) {
	// Assistant tool call + tool result must round-trip as function_call /
	// function_call_output items — the agent-loop contract.
	u := &types.ChatRequest{
		Model: "grok-4.5",
		Messages: []types.Message{
			{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "what time?"}}},
			{Role: types.RoleAssistant, Content: []types.Part{
				{Type: types.PartText, Text: "checking"},
				{Type: types.PartToolUse, ID: "call_1", Name: "clock", Args: json.RawMessage(`{"tz":"utc"}`)},
			}},
			{Role: types.RoleUser, Content: []types.Part{
				{Type: types.PartToolResult, ToolUseID: "call_1", Text: "12:34"},
			}},
		},
		Tools: []types.Tool{{Name: "clock", Description: "read clock", Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := EncodeResponsesRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var req rsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	// Tools flattened (no nested "function" wrapper).
	if len(req.Tools) != 1 || req.Tools[0].Name != "clock" || string(req.Tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("tools = %s", body)
	}
	var items []rsItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		t.Fatal(err)
	}
	var sawCall, sawOutput, sawAssistantText bool
	for _, it := range items {
		switch it.Type {
		case "function_call":
			sawCall = it.CallID == "call_1" && it.Name == "clock" && it.Arguments == `{"tz":"utc"}`
		case "function_call_output":
			sawOutput = it.CallID == "call_1" && it.Output == "12:34"
		case "message":
			if it.Role == "assistant" && strings.Contains(string(it.Content), "checking") {
				sawAssistantText = true
			}
		}
	}
	if !sawCall || !sawOutput || !sawAssistantText {
		t.Fatalf("tool flow items wrong: call=%v output=%v text=%v in %s", sawCall, sawOutput, sawAssistantText, req.Input)
	}
}

func TestEncodeResponsesRequestReasoning(t *testing.T) {
	// reasoning.effort is forwarded verbatim for values the Responses API
	// accepts; ""/none omit the knob entirely; max clamps down (never up);
	// a budget-derived effort never overrides an explicit one.
	cases := []struct {
		effort string
		want   string // "" = no reasoning object
	}{
		{"", ""},
		{"none", ""},
		{"minimal", "minimal"},
		{"low", "low"},
		{"high", "high"},
		{"max", "high"},
	}
	for _, c := range cases {
		u := &types.ChatRequest{Model: "grok-4.6", ReasoningEffort: c.effort,
			Messages: []types.Message{{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "x"}}}}}
		body, err := EncodeResponsesRequest(u)
		if err != nil {
			t.Fatal(err)
		}
		var req rsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		got := ""
		if req.Reasoning != nil {
			got = req.Reasoning.Effort
		}
		if got != c.want {
			t.Fatalf("effort %q -> %q, want %q", c.effort, got, c.want)
		}
	}
	// Budget alone (no explicit effort) fills the gap.
	u := &types.ChatRequest{Model: "grok-4.6",
		Thinking: &types.ThinkingCfg{BudgetTokens: 32768},
		Messages: []types.Message{{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "x"}}}}}
	body, err := EncodeResponsesRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var req rsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "high" {
		t.Fatalf("budget-derived effort missing: %+v", req.Reasoning)
	}
	// Budget must NOT override an explicit client effort.
	u.Thinking = &types.ThinkingCfg{BudgetTokens: 32768}
	u.ReasoningEffort = "low"
	body, err = EncodeResponsesRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "low" {
		t.Fatalf("budget overrode explicit effort: %+v", req.Reasoning)
	}
}

func TestDecodeResponsesResponse(t *testing.T) {
	body := []byte(`{
		"id":"resp_1","model":"grok-4.6","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking..."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello "}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]},
			{"type":"function_call","call_id":"c1","name":"clock","arguments":"{}"}
		],
		"usage":{"input_tokens":100,"output_tokens":20,
			"input_tokens_details":{"cached_tokens":40},
			"output_tokens_details":{"reasoning_tokens":8},"total_tokens":120}
	}`)
	cr, err := DecodeResponsesResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if cr.ID != "resp_1" || cr.StopReason != types.StopEndTurn {
		t.Fatalf("meta: %+v", cr)
	}
	var text string
	var toolUses, thinking int
	for _, p := range cr.Content {
		switch p.Type {
		case types.PartText:
			text = p.Text
		case types.PartToolUse:
			toolUses++
			if p.ID != "c1" || p.Name != "clock" {
				t.Fatalf("tool part: %+v", p)
			}
		case types.PartThinking:
			thinking++
		}
	}
	if text != "hello \nworld" {
		t.Fatalf("joined text = %q", text)
	}
	if toolUses != 1 || thinking != 1 {
		t.Fatalf("toolUses=%d thinking=%d", toolUses, thinking)
	}
	if cr.Usage.InputTokens != 100 || cr.Usage.OutputTokens != 20 ||
		cr.Usage.CacheReadTokens != 40 || cr.Usage.ReasoningTokens != 8 {
		t.Fatalf("usage: %+v", cr.Usage)
	}
}

func TestDecodeResponsesResponseIncomplete(t *testing.T) {
	body := []byte(`{"id":"r","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`)
	cr, err := DecodeResponsesResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if cr.StopReason != types.StopMaxTokens {
		t.Fatalf("stop = %q, want max_tokens", cr.StopReason)
	}
}

// ---- streaming ----

func sse(events [][2]string) string {
	var b strings.Builder
	for _, e := range events {
		if e[0] != "" {
			b.WriteString("event: " + e[0] + "\n")
		}
		b.WriteString("data: " + e[1] + "\n\n")
	}
	return b.String()
}

func TestResponsesStreamToOpenAI(t *testing.T) {
	stream := sse([][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"resp_9","model":"grok-4.6"}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel"}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo"}`},
		{"response.output_text.done", `{"type":"response.output_text.done","text":"Hello"}`},
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_9","status":"completed","usage":{"input_tokens":11,"output_tokens":2,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":1}}}}`},
	})
	var out bytes.Buffer
	usage, err := TranslateStream(strings.NewReader(stream), &out, nil, FmtResponses, FmtOpenAI, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, `"content":"Hel"`) || !strings.Contains(s, `"content":"lo"`) {
		t.Fatalf("missing text deltas: %s", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) || !strings.Contains(s, "[DONE]") {
		t.Fatalf("missing terminator: %s", s)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 2 || usage.CacheReadTokens != 4 || usage.ReasoningTokens != 1 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestResponsesStreamToolCallsParallel(t *testing.T) {
	// Two parallel calls: all added events arrive before any delta (the
	// case that merges tool inputs when correlating by stream position).
	stream := sse([][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"r"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","id":"item_a","call_id":"call_a","name":"f"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","id":"item_b","call_id":"call_b","name":"g"}}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"item_b","delta":"{\"y\":1}"}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"item_a","delta":"{\"x\":1}"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","item":{"type":"function_call","id":"item_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}}`},
		{"response.output_item.done", `{"type":"response.output_item.done","item":{"type":"function_call","id":"item_b","call_id":"call_b","name":"g","arguments":"{\"y\":1}"}}`},
		{"response.completed", `{"type":"response.completed","response":{"status":"completed"}}`},
	})
	var out bytes.Buffer
	if _, err := TranslateStream(strings.NewReader(stream), &out, nil, FmtResponses, FmtOpenAI, "grok-4.6"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	// Each call gets its own index; args must not merge.
	if strings.Count(s, `"id":"call_a"`) != 1 || strings.Count(s, `"id":"call_b"`) != 1 {
		t.Fatalf("tool starts wrong: %s", s)
	}
	if !strings.Contains(s, `\"x\":1`) || !strings.Contains(s, `\"y\":1`) {
		t.Fatalf("args missing: %s", s)
	}
	// done-with-full-args must not duplicate the delta'd arguments.
	if strings.Count(s, `\"x\":1`) != 1 {
		t.Fatalf("args duplicated: %s", s)
	}
}

func TestResponsesStreamToolDoneWithoutDelta(t *testing.T) {
	// Some upstreams emit only output_item.done with complete arguments and
	// never stream function_call_arguments.delta — args must reach the
	// client exactly once from the done event.
	stream := sse([][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"r"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","id":"i1","call_id":"c1","name":"f"}}`},
		{"response.output_item.done", `{"type":"response.output_item.done","item":{"type":"function_call","id":"i1","call_id":"c1","name":"f","arguments":"{\"k\":1}"}}`},
		{"response.completed", `{"type":"response.completed","response":{"status":"completed"}}`},
	})
	var out bytes.Buffer
	if _, err := TranslateStream(strings.NewReader(stream), &out, nil, FmtResponses, FmtOpenAI, "grok-4.6"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), `\"k\":1`); n != 1 {
		t.Fatalf("done-only args emitted %d times, want 1: %s", n, out.String())
	}
}

func TestResponsesStreamCompatAliases(t *testing.T) {
	// Unnamed events (payload "type" carries the event), the response.done
	// alias, and the legacy usage field names must all work — upstreams vary.
	stream := sse([][2]string{
		{"", `{"type":"response.created","response":{"id":"r","model":"grok-4.6"}}`},
		{"", `{"type":"response.output_text.delta","delta":"ok"}`},
		{"", `{"type":"response.done","response":{"status":"completed","usage":{"prompt_tokens":9,"completion_tokens":2,"cache_read_input_tokens":3}}}`},
	})
	var out bytes.Buffer
	usage, err := TranslateStream(strings.NewReader(stream), &out, nil, FmtResponses, FmtOpenAI, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"content":"ok"`) {
		t.Fatalf("delta missing: %s", out.String())
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 2 || usage.CacheReadTokens != 3 {
		t.Fatalf("legacy usage fields not mapped: %+v", usage)
	}
}

func TestResponsesStreamReasoningAndError(t *testing.T) {
	stream := sse([][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"r"}}`},
		{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"think"}`},
		{"error", `{"type":"error","error":{"type":"server_error","message":"boom"}}`},
	})
	var out bytes.Buffer
	_, err := TranslateStream(strings.NewReader(stream), &out, nil, FmtResponses, FmtOpenAI, "grok-4.6")
	if err != nil {
		t.Fatalf("error event is terminal, not a translate failure: %v", err)
	}
	if !strings.Contains(out.String(), `"reasoning_content":"think"`) {
		t.Fatalf("reasoning delta missing: %s", out.String())
	}
}

func TestResponsesStreamDisconnectFails(t *testing.T) {
	// Stream ends without response.completed: must surface an error, not a
	// clean finish (9router's stream_disconnected case).
	stream := sse([][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"r"}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"partial"}`},
	})
	var out bytes.Buffer
	_, err := TranslateStream(strings.NewReader(stream), &out, nil, FmtResponses, FmtOpenAI, "grok-4.6")
	if err == nil || !strings.Contains(err.Error(), "before response.completed") {
		t.Fatalf("want disconnect error, got %v", err)
	}
}

func TestResponsesStreamToAnthropic(t *testing.T) {
	stream := sse([][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"r","model":"grok-4.6"}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`},
		{"response.completed", `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":1}}}`},
	})
	var out bytes.Buffer
	usage, err := TranslateStream(strings.NewReader(stream), &out, nil, FmtResponses, FmtAnthropic, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "event: message_start") || !strings.Contains(s, "event: content_block_delta") || !strings.Contains(s, "event: message_stop") {
		t.Fatalf("anthropic SSE shape wrong: %s", s)
	}
	if usage.InputTokens != 5 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestResponsesRoundTripThroughUnified(t *testing.T) {
	// openai chat request -> responses body -> back to unified preserves the
	// conversation (the seam the server relies on for cross-format clients).
	oa := []byte(`{"model":"grok-4.6","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"user","content":"again"}],"max_tokens":10}`)
	u, err := DecodeOpenAIRequest(oa)
	if err != nil {
		t.Fatal(err)
	}
	u.Stream = false
	rb, err := EncodeResponsesRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var req rsRequest
	if err := json.Unmarshal(rb, &req); err != nil {
		t.Fatal(err)
	}
	if req.Instructions != "s" {
		t.Fatalf("instructions = %q", req.Instructions)
	}
	var items []rsItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (system hoisted)", len(items))
	}
	if items[1].Role != "assistant" {
		t.Fatalf("assistant item lost: %+v", items)
	}
}
