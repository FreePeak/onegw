package translat

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// Request: unified -> commandcode
// ---------------------------------------------------------------------------

func commandCodeFixtureRequest() *types.ChatRequest {
	return &types.ChatRequest{
		Model:  "zai-org/GLM-5",
		System: []types.Part{{Type: types.PartText, Text: "be terse"}},
		Messages: []types.Message{
			{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "list files"}}},
			{Role: types.RoleAssistant, Content: []types.Part{
				{Type: types.PartToolUse, ID: "call_1", Name: "ls", Args: json.RawMessage(`{"path":"/"}`)},
			}},
			{Role: types.RoleTool, ToolCallID: "call_1", Name: "ls",
				Content: []types.Part{{Type: types.PartText, Text: "a.txt"}}},
		},
		Tools:     []types.Tool{{Name: "ls", Description: "list", Schema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens: 512,
	}
}

func TestEncodeCommandCodeRequest(t *testing.T) {
	out, err := EncodeCommandCodeRequest(commandCodeFixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		ThreadID string `json:"threadId"`
		Config   struct {
			Date string `json:"date"`
		} `json:"config"`
		Params struct {
			Model       string      `json:"model"`
			Messages    []ccMessage `json:"messages"`
			Stream      bool        `json:"stream"`
			MaxTokens   int         `json:"max_tokens"`
			System      string      `json:"system"`
			Tools       []ccToolDef `json:"tools"`
			Temperature *float64    `json:"temperature"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req.ThreadID == "" {
		t.Fatal("missing threadId")
	}
	p := req.Params
	if p.Model != "zai-org/GLM-5" || !p.Stream || p.MaxTokens != 512 {
		t.Fatalf("bad params: model=%s stream=%v max=%d", p.Model, p.Stream, p.MaxTokens)
	}
	if p.System != "be terse" {
		t.Fatalf("system not hoisted to params.system: %q", p.System)
	}
	// user, assistant (text + tool-call), tool result
	if len(p.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(p.Messages), p.Messages)
	}
	if p.Messages[0].Role != "user" || p.Messages[0].Content[0].Type != "text" {
		t.Fatalf("bad user message: %+v", p.Messages[0])
	}
	if p.Messages[1].Role != "assistant" {
		t.Fatalf("bad assistant: %+v", p.Messages[1])
	}
	var call *ccBlock
	for i := range p.Messages[1].Content {
		if p.Messages[1].Content[i].Type == "tool-call" {
			call = &p.Messages[1].Content[i]
		}
	}
	if call == nil || call.ToolCallID != "call_1" || call.ToolName != "ls" || string(call.Input) != `{"path":"/"}` {
		t.Fatalf("tool-call block not mapped: %+v", p.Messages[1].Content)
	}
	if p.Messages[2].Role != "tool" || p.Messages[2].Content[0].Type != "tool-result" ||
		p.Messages[2].Content[0].ToolCallID != "call_1" || p.Messages[2].Content[0].Output.Value != "a.txt" {
		t.Fatalf("tool-result not mapped: %+v", p.Messages[2])
	}
	if len(p.Tools) != 1 || p.Tools[0].Name != "ls" || !strings.Contains(string(p.Tools[0].InputSchema), `"object"`) {
		t.Fatalf("tools not mapped to input_schema: %+v", p.Tools)
	}
}

// Non-stream clients still get stream:true upstream (forced stream).
func TestEncodeCommandCodeRequestForcesStream(t *testing.T) {
	u := &types.ChatRequest{Model: "m", Stream: false,
		Messages: []types.Message{{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "hi"}}}}}
	out, err := EncodeCommandCodeRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("stream not forced: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Response stream: commandcode NDJSON -> OpenAI SSE
// ---------------------------------------------------------------------------

const ccStreamFixture = `{"type":"start","messageId":"cmpl-9"}
{"type":"start-step"}
{"type":"text-start","id":"t1"}
{"type":"text-delta","text":"Hel"}
{"type":"text-delta","text":"lo"}
{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":7,"outputTokens":2}}
{"type":"finish"}
`

func TestTranslateCommandCodeToOpenAI(t *testing.T) {
	var sb strings.Builder
	usage, err := TranslateStream(strings.NewReader(ccStreamFixture), &sb, nil, FmtCommandCode, FmtOpenAI, "cc-m")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 2 {
		t.Fatalf("usage not captured: %+v", usage)
	}
	out := sb.String()
	if !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Fatalf("deltas missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish missing: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":7`) || !strings.Contains(out, `"completion_tokens":2`) {
		t.Fatalf("usage chunk missing: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatal("missing [DONE]")
	}
	// NDJSON must not be parsed as SSE: no "data:" prefix was sent upstream,
	// and bare JSON lines are consumed as events.
	if strings.Contains(out, `{"type":"start"}`) {
		t.Fatalf("raw upstream line leaked: %s", out)
	}
}

func TestTranslateCommandCodeReasoningAndTools(t *testing.T) {
	up := strings.Join([]string{
		`{"type":"start"}`,
		`{"type":"reasoning-delta","text":"thinking"}`,
		`{"type":"tool-input-start","id":"tu1","toolName":"ls"}`,
		`{"type":"tool-input-delta","id":"tu1","delta":"{\"pa"}`,
		`{"type":"tool-input-delta","id":"tu1","delta":"th\":\"/\"}"}`,
		`{"type":"tool-input-end","id":"tu1"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
		``,
	}, "\n")
	var sb strings.Builder
	if _, err := TranslateStream(strings.NewReader(up), &sb, nil, FmtCommandCode, FmtOpenAI, "cc-m"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"reasoning_content":"thinking"`) {
		t.Fatalf("reasoning delta missing: %s", out)
	}
	if !strings.Contains(out, `"name":"ls"`) || !strings.Contains(out, `"arguments":"{\"pa"`) {
		t.Fatalf("tool call streaming missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish reason not mapped: %s", out)
	}
}

// A consolidated tool-call event (no prior tool-input deltas) becomes one
// complete tool call.
func TestTranslateCommandCodeConsolidatedToolCall(t *testing.T) {
	up := strings.Join([]string{
		`{"type":"start"}`,
		`{"type":"tool-call","toolCallId":"tu9","toolName":"run","input":{"cmd":"ls"}}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
		``,
	}, "\n")
	var sb strings.Builder
	if _, err := TranslateStream(strings.NewReader(up), &sb, nil, FmtCommandCode, FmtOpenAI, "cc-m"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"id":"tu9"`) || !strings.Contains(out, `"arguments":"{\"cmd\":\"ls\"}"`) {
		t.Fatalf("consolidated tool call missing: %s", out)
	}
	// Consolidated call must not be double-emitted via a later input stream.
	if strings.Count(out, `"name":"run"`) != 1 {
		t.Fatalf("tool start emitted more than once: %s", out)
	}
}

// "data:"-prefixed lines and [DONE] are tolerated.
func TestTranslateCommandCodeToleratesDataPrefix(t *testing.T) {
	up := "data: {\"type\":\"start\"}\ndata: {\"type\":\"text-delta\",\"text\":\"x\"}\ndata: [DONE]\n"
	var sb strings.Builder
	if _, err := TranslateStream(strings.NewReader(up), &sb, nil, FmtCommandCode, FmtOpenAI, "cc-m"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `"content":"x"`) {
		t.Fatalf("prefixed delta lost: %s", sb.String())
	}
}

// ---------------------------------------------------------------------------
// In-200 error events
// ---------------------------------------------------------------------------

func TestInspectCommandCodeHeadError(t *testing.T) {
	up := strings.Join([]string{
		`{"type":"start"}`,
		`{"type":"error","message":"rate limit exceeded, retry later"}`,
		``,
	}, "\n")
	head, apiErr, err := InspectCommandCodeHead(strings.NewReader(up))
	if err != nil {
		t.Fatal(err)
	}
	if head != nil {
		t.Fatalf("error head must not replay content: %q", head)
	}
	if apiErr == nil {
		t.Fatal("expected APIError from error event")
	}
	if apiErr.Status != 429 || apiErr.Type != "rate_limit_error" {
		t.Fatalf("wrong synthesized error: %+v", apiErr)
	}
	if !strings.Contains(apiErr.Message, "rate limit exceeded") {
		t.Fatalf("message lost: %+v", apiErr)
	}
}

func TestInspectCommandCodeHeadContent(t *testing.T) {
	up := strings.Join([]string{
		`{"type":"start"}`,
		`{"type":"start-step"}`,
		`{"type":"text-delta","text":"hello"}`,
		`{"type":"finish"}`,
		``,
	}, "\n")
	head, apiErr, err := InspectCommandCodeHead(strings.NewReader(up))
	if err != nil {
		t.Fatal(err)
	}
	if apiErr != nil {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	// Head must stop at (and include) the first terminal event.
	if string(head) != "{\"type\":\"start\"}\n{\"type\":\"start-step\"}\n{\"type\":\"text-delta\",\"text\":\"hello\"}\n" {
		t.Fatalf("bad head: %q", head)
	}
}

func TestParseCommandCodeErrorStatusSynthesis(t *testing.T) {
	cases := []struct {
		event  string
		status int
		typ    string
	}{
		{`{"type":"error","error":{"statusCode":402,"message":"payment required"}}`, 402, "billing_error"},
		{`{"type":"error","message":"unauthorized: invalid api key"}`, 401, "authentication_error"},
		{`{"type":"error","error":"quota exhausted"}`, 403, "permission_error"},
		{`{"type":"error","statusCode":418,"message":"teapot"}`, 418, "server_error"},
		{`{"type":"error","message":"something exploded"}`, 503, "server_error"},
	}
	for _, c := range cases {
		var ev map[string]any
		if err := json.Unmarshal([]byte(c.event), &ev); err != nil {
			t.Fatal(err)
		}
		got := ParseCommandCodeError(ev)
		if got.Status != c.status || got.Type != c.typ {
			t.Fatalf("%s: got %+v, want %d/%s", c.event, got, c.status, c.typ)
		}
	}
}

// ---------------------------------------------------------------------------
// Aggregation (non-stream client on stream-forced upstream)
// ---------------------------------------------------------------------------

func TestAggregateCommandCodeStream(t *testing.T) {
	resp, err := AggregateStream(strings.NewReader(ccStreamFixture), FmtCommandCode, "cc-m")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != types.StopEndTurn {
		t.Fatalf("stop reason: %q", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello" {
		t.Fatalf("content not aggregated: %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage not aggregated: %+v", resp.Usage)
	}
	if resp.Model != "cc-m" {
		t.Fatalf("model fallback missing: %q", resp.Model)
	}
}

func TestAggregateCommandCodeStreamToolCall(t *testing.T) {
	up := strings.Join([]string{
		`{"type":"start"}`,
		`{"type":"tool-call","toolCallId":"tu9","toolName":"run","input":{"cmd":"ls"}}`,
		`{"type":"finish","finishReason":"tool-calls","usage":{"inputTokens":3,"outputTokens":1}}`,
		``,
	}, "\n")
	resp, err := AggregateStream(strings.NewReader(up), FmtCommandCode, "cc-m")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != types.StopToolUse {
		t.Fatalf("stop reason: %q", resp.StopReason)
	}
	var found bool
	for _, p := range resp.Content {
		if p.Type == types.PartToolUse && p.ID == "tu9" && p.Name == "run" && strings.Contains(string(p.Args), `"cmd"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool use not aggregated: %+v", resp.Content)
	}
}

// ---------------------------------------------------------------------------
// Grok CLI (OpenAI Responses)
// ---------------------------------------------------------------------------

func TestEncodeResponsesRequest(t *testing.T) {
	u := commandCodeFixtureRequest()
	u.Stream = false
	out, err := EncodeGrokCliRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Model           string `json:"model"`
		Instructions    string `json:"instructions"`
		Stream          bool   `json:"stream"`
		Store           bool   `json:"store"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Input           []struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Output    string          `json:"output"`
			Arguments string          `json:"arguments"`
		} `json:"input"`
		Tools []struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if !req.Stream || req.Store {
		t.Fatalf("stream/store wrong: stream=%v store=%v", req.Stream, req.Store)
	}
	if req.Model != "zai-org/GLM-5" || req.Instructions != "be terse" || req.MaxOutputTokens != 512 {
		t.Fatalf("bad fields: %+v", req)
	}
	// user message, function_call, function_call_output
	if len(req.Input) != 3 {
		t.Fatalf("want 3 input items, got %d: %+v", len(req.Input), req.Input)
	}
	if req.Input[0].Type != "message" || req.Input[0].Role != "user" || !strings.Contains(string(req.Input[0].Content), "list files") {
		t.Fatalf("bad user item: %+v", req.Input[0])
	}
	if req.Input[1].Type != "function_call" || req.Input[1].CallID != "call_1" || req.Input[1].Name != "ls" ||
		req.Input[1].Arguments != `{"path":"/"}` {
		t.Fatalf("bad function_call: %+v", req.Input[1])
	}
	if req.Input[2].Type != "function_call_output" || req.Input[2].CallID != "call_1" || req.Input[2].Output != "a.txt" {
		t.Fatalf("bad function_call_output: %+v", req.Input[2])
	}
	if len(req.Tools) != 1 || req.Tools[0].Type != "function" || req.Tools[0].Name != "ls" {
		t.Fatalf("tools not flattened: %+v", req.Tools)
	}
	// Chat Completions leftovers must never appear.
	for _, banned := range []string{`"messages"`, `"max_tokens"`, `"stream_options"`, `"frequency_penalty"`} {
		if strings.Contains(string(out), banned) {
			t.Fatalf("chat-completions field leaked: %s in %s", banned, out)
		}
	}
}

func TestTranslateResponsesToOpenAI(t *testing.T) {
	up := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-build"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":" there"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":1}}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var sb strings.Builder
	usage, err := TranslateStream(strings.NewReader(up), &sb, nil, FmtOpenAIResponses, FmtOpenAI, "grok")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 3 || usage.CacheReadTokens != 4 || usage.ReasoningTokens != 1 {
		t.Fatalf("usage not mapped: %+v", usage)
	}
	out := sb.String()
	if !strings.Contains(out, `"content":"Hi"`) || !strings.Contains(out, `"content":" there"`) {
		t.Fatalf("text deltas missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("stream not terminated properly: %s", out)
	}
}

func TestTranslateResponsesToolCall(t *testing.T) {
	up := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"ls"}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path"}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\":\"/\"}"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"ls","arguments":"{\"path\":\"/\"}"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":5,"output_tokens":6}}}`,
		``,
	}, "\n")
	var sb strings.Builder
	if _, err := TranslateStream(strings.NewReader(up), &sb, nil, FmtOpenAIResponses, FmtOpenAI, "grok"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"id":"call_9"`) || !strings.Contains(out, `"name":"ls"`) {
		t.Fatalf("tool start missing: %s", out)
	}
	if !strings.Contains(out, `"arguments":"{\"path"`) || !strings.Contains(out, `"arguments":"\":\"/\"}"`) {
		t.Fatalf("argument deltas missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish missing: %s", out)
	}
}

func TestTranslateResponsesFailedEvent(t *testing.T) {
	up := strings.Join([]string{
		`data: {"type":"response.failed","response":{"error":{"code":"billing","message":"spending limit reached"}}}`,
		``,
	}, "\n")
	var sb strings.Builder
	_, err := TranslateStream(strings.NewReader(up), &sb, nil, FmtOpenAIResponses, FmtOpenAI, "grok")
	apiErr, ok := err.(*types.APIError)
	if !ok {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.Status != 502 || !strings.Contains(apiErr.Message, "spending limit") {
		t.Fatalf("bad error: %+v", apiErr)
	}
}

func TestAggregateResponsesStream(t *testing.T) {
	up := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_3","model":"grok-build"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"pong"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_3","usage":{"input_tokens":2,"output_tokens":1}}}`,
		``,
	}, "\n")
	resp, err := AggregateStream(strings.NewReader(up), FmtOpenAIResponses, "grok")
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_3" || resp.Model != "grok-build" || resp.StopReason != types.StopEndTurn {
		t.Fatalf("bad aggregate: %+v", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "pong" {
		t.Fatalf("content wrong: %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 2 || resp.Usage.OutputTokens != 1 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

func TestAggregateStreamPropagatesError(t *testing.T) {
	up := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_4"}}`,
		``,
		`data: {"type":"response.failed","response":{"error":{"message":"boom"}}}`,
		``,
	}, "\n")
	_, err := AggregateStream(strings.NewReader(up), FmtOpenAIResponses, "grok")
	apiErr, ok := err.(*types.APIError)
	if !ok || !strings.Contains(apiErr.Message, "boom") {
		t.Fatalf("error not propagated: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cursor skeleton
// ---------------------------------------------------------------------------

// newUUID sanity: v4 shape.
func TestNewUUIDShape(t *testing.T) {
	u := newUUID()
	if len(u) != 36 || strings.Count(u, "-") != 4 || u[14] != '4' {
		t.Fatalf("not a v4 uuid: %q", u)
	}
}

var _ io.Reader = (*strings.Reader)(nil)
