package translat

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"onegw/internal/types"
)

func TestOpenAIRequestToUnifiedToAnthropic(t *testing.T) {
	body := `{
		"model": "gpt-5.5",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "list files"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "ls", "arguments": "{\"path\":\"/\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.txt\nb.txt"}
		],
		"tools": [{"type": "function", "function": {"name": "ls", "description": "list", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}}],
		"max_tokens": 100,
		"stream": true,
		"temperature": 0.7
	}`
	u, err := DecodeOpenAIRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u.Model != "gpt-5.5" || !u.Stream || u.MaxTokens != 100 {
		t.Fatalf("bad unified request: %+v", u)
	}
	if len(u.System) != 1 || u.System[0].Text != "be terse" {
		t.Fatalf("system not hoisted: %+v", u.System)
	}
	if len(u.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(u.Messages))
	}
	asst := u.Messages[1]
	if len(asst.Content) != 1 || asst.Content[0].Type != types.PartToolUse || asst.Content[0].Name != "ls" {
		t.Fatalf("tool_use not decoded: %+v", asst)
	}
	if !strings.Contains(string(asst.Content[0].Args), `"path"`) {
		t.Fatalf("args not object: %s", asst.Content[0].Args)
	}
	tool := u.Messages[2]
	if tool.Content[0].Type != types.PartToolResult || tool.Content[0].ToolUseID != "call_1" {
		t.Fatalf("tool result not decoded: %+v", tool)
	}

	out, err := EncodeAnthropicRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var ar map[string]any
	if err := json.Unmarshal(out, &ar); err != nil {
		t.Fatal(err)
	}
	if ar["max_tokens"].(float64) != 100 {
		t.Fatalf("max_tokens lost: %v", ar["max_tokens"])
	}
	msgs := ar["messages"].([]any)
	// user + assistant(tool_use) + user(tool_result)
	if len(msgs) != 3 {
		t.Fatalf("want 3 anthropic messages, got %d: %s", len(msgs), out)
	}
	asstBlocks := msgs[1].(map[string]any)["content"].([]any)
	if asstBlocks[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("tool_use block missing: %s", out)
	}
	toolBlocks := msgs[2].(map[string]any)["content"].([]any)
	tb := toolBlocks[0].(map[string]any)
	if tb["type"] != "tool_result" || tb["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result block wrong: %v", tb)
	}
	if _, ok := ar["system"]; !ok {
		t.Fatalf("system lost")
	}
}

func TestAnthropicRequestToUnifiedToOpenAI(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 1024,
		"system": "be brief",
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_9", "name": "read", "input": {"path": "x"}}
			]},
			{"role": "user", "content": [
				{"type": "text", "text": "read file"},
				{"type": "tool_result", "tool_use_id": "toolu_9", "content": "contents here"}
			]}
		],
		"tools": [{"name": "read", "description": "read a file", "input_schema": {"type": "object"}}],
		"thinking": {"type": "enabled", "budget_tokens": 5000}
	}`
	u, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u.Thinking == nil || u.Thinking.BudgetTokens != 5000 {
		t.Fatalf("thinking lost: %+v", u.Thinking)
	}
	// user msg: text part + tool_result part; assistant: tool_use
	if len(u.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(u.Messages))
	}
	out, err := EncodeOpenAIRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var or map[string]any
	if err := json.Unmarshal(out, &or); err != nil {
		t.Fatal(err)
	}
	msgs := or["messages"].([]any)
	// system + user(read file) + assistant(tool_calls) + user tool
	want := 4
	if len(msgs) != want {
		t.Fatalf("want %d openai messages, got %d: %s", want, len(msgs), out)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_9" {
		t.Fatalf("tool message wrong: %v", toolMsg)
	}
}

func TestGeminiRequestRoundTrip(t *testing.T) {
	body := `{
		"contents": [
			{"role": "user", "parts": [{"text": "hi"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "get", "args": {"k": "v"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "get", "response": {"result": "ok"}}}]}
		],
		"systemInstruction": {"parts": [{"text": "sys"}]},
		"generationConfig": {"maxOutputTokens": 512, "temperature": 0.5}
	}`
	u, err := DecodeGeminiRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.System) != 1 || u.System[0].Text != "sys" {
		t.Fatalf("systemInstruction lost: %+v", u.System)
	}
	if len(u.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(u.Messages))
	}
	if u.Messages[1].Content[0].Type != types.PartToolUse {
		t.Fatalf("functionCall not decoded: %+v", u.Messages[1])
	}
	if u.Messages[2].Content[0].Type != types.PartToolResult {
		t.Fatalf("functionResponse not decoded: %+v", u.Messages[2])
	}
	out, err := EncodeGeminiRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var gr map[string]any
	if err := json.Unmarshal(out, &gr); err != nil {
		t.Fatal(err)
	}
	genCfg, ok := gr["generationConfig"].(map[string]any)
	if !ok || genCfg["maxOutputTokens"].(float64) != 512 {
		t.Fatalf("generationConfig wrong: %s", out)
	}
}

func TestTranslateStreamAnthropicToOpenAI(t *testing.T) {
	upstream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-x","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"ls","input":{}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"p\":"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	var sb strings.Builder
	usage, err := TranslateStream(strings.NewReader(upstream), &sb, nil, FmtAnthropic, FmtOpenAI, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 25 || usage.OutputTokens != 42 {
		t.Fatalf("usage wrong: %+v", usage)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "data: ") {
		t.Fatalf("missing SSE prefix: %q", out[:40])
	}
	if !strings.Contains(out, `"reasoning_content"`) == true && strings.Count(out, "tool_calls") < 2 {
		t.Fatalf("tool_calls chunks missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason missing: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":25`) || !strings.Contains(out, `"completion_tokens":42`) {
		t.Fatalf("usage chunk missing: %s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "[DONE]") {
		t.Fatalf("[DONE] missing")
	}
}

func TestTranslateStreamOpenAIToAnthropic(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-x","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":2}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var sb strings.Builder
	usage, err := TranslateStream(strings.NewReader(upstream), &sb, nil, FmtOpenAI, FmtAnthropic, "gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage wrong: %+v", usage)
	}
	out := sb.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
		`"input_tokens":11`,
		`"output_tokens":7`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestTranslateStreamGeminiToOpenAI(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hey"}]}}],"modelVersion":"gemini-3"}  ,`,
		``,
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3,"totalTokenCount":12}}`,
		``,
	}, "\n")

	var sb strings.Builder
	usage, err := TranslateStream(strings.NewReader(strings.ReplaceAll(upstream, "  ,", "")), &sb, nil, FmtGemini, FmtOpenAI, "gemini-3")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 3 {
		t.Fatalf("usage wrong: %+v", usage)
	}
	if !strings.Contains(sb.String(), "Hey") || !strings.Contains(sb.String(), `"finish_reason":"stop"`) {
		t.Fatalf("bad output: %s", sb.String())
	}
}

func TestOpenAIResponseRoundTrip(t *testing.T) {
	body := `{
		"id": "chatcmpl-1",
		"object": "chat.completion",
		"model": "gpt-x",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"message": {"role": "assistant", "content": "calling", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "ls", "arguments": "{\"path\":\".\"}"}}
			]}
		}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}
	}`
	u, err := DecodeOpenAIResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u.StopReason != types.StopToolUse || u.Usage.InputTokens != 100 {
		t.Fatalf("bad decode: %+v", u)
	}
	// To anthropic and back
	an, err := EncodeAnthropicResponse(u)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := DecodeAnthropicResponse(an)
	if err != nil {
		t.Fatal(err)
	}
	if u2.StopReason != types.StopToolUse {
		t.Fatalf("stop reason changed: %s", u2.StopReason)
	}
	found := false
	for _, p := range u2.Content {
		if p.Type == types.PartToolUse && p.Name == "ls" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_use lost: %s", an)
	}
}

func TestErrorEncoding(t *testing.T) {
	e := &types.APIError{Status: 429, Type: "rate_limit_error", Message: "slow down"}
	got := EncodeError(FmtAnthropic, e)
	if !strings.Contains(string(got), "rate_limit_error") {
		t.Fatalf("anthropic error wrong: %s", got)
	}
	got = EncodeError(FmtGemini, e)
	if !strings.Contains(string(got), "RESOURCE_EXHAUSTED") {
		t.Fatalf("gemini error wrong: %s", got)
	}
	got = EncodeError(FmtOpenAI, e)
	if !strings.Contains(string(got), "429") {
		t.Fatalf("openai error wrong: %s", got)
	}
}

var _ = io.EOF
