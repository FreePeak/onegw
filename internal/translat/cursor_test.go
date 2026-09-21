package translat

// Cursor wire format tests. The checksum/session vectors are cross-checked
// against 9router's cursorChecksum.js (the reference implementation the
// port preserves byte-for-byte); the protobuf vectors against the live
// 2026-09-09 probes that proved the AgentService contract (system prompt
// kills the turn, constant handshake reply, done-frame usage).

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"onegw/internal/types"
)

func TestCursorChecksumMatchesReference(t *testing.T) {
	// Vector computed with 9router's cursorChecksum.js (the reference the
	// port preserves byte-for-byte): checksum("b9854c5c-...", Date=1788945600000).
	const machineID = "b9854c5c-64ac-418c-bd14-68085958ef73"
	const want = "6fn747On" + machineID
	got := cursorChecksum(machineID, time.UnixMilli(1788945600000))
	if got != want {
		t.Fatalf("checksum mismatch: got %q want %q", got, want)
	}
	// Structural properties: machineId suffix, 8-char base64url prefix
	// (6 bytes → 8 unpadded symbols).
	if !strings.HasSuffix(got, machineID) {
		t.Fatalf("checksum must end with machineId: %q", got)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	prefix := got[:len(got)-len(machineID)]
	if len(prefix) != 8 {
		t.Fatalf("checksum prefix must be 8 chars, got %d: %q", len(prefix), prefix)
	}
	for _, c := range prefix {
		if !strings.ContainsRune(alphabet, c) {
			t.Fatalf("checksum prefix char %q outside base64url alphabet", c)
		}
	}
}

func TestCursorUUIDv5IsStable(t *testing.T) {
	// uuid.v5(token, DNS-namespace) is deterministic: same token → same id.
	a := cursorUUIDv5("tok-abc")
	b := cursorUUIDv5("tok-abc")
	if a != b {
		t.Fatalf("uuidv5 must be deterministic: %q vs %q", a, b)
	}
	if len(a) != 36 || a[14] != '5' {
		t.Fatalf("uuidv5 shape wrong: %q (version nibble at [14] must be 5)", a)
	}
	if c := cursorUUIDv5("tok-other"); c == a {
		t.Fatal("different tokens must map to different session ids")
	}
}

func TestPBRoundtrip(t *testing.T) {
	var b []byte
	b = pbString(b, 1, "hello")
	b = pbUvarint(b, 2, 300)
	b = pbBytes(b, 3, []byte{0xde, 0xad})
	f, err := pbDecode(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 3 {
		t.Fatalf("want 3 fields, got %d", len(f))
	}
	if got := pbFirst(f, 1); got != "hello" {
		t.Fatalf("field 1: %q", got)
	}
	if v, _ := pbGet(f, 2); v.Num64 != 300 {
		t.Fatalf("field 2: %d", v.Num64)
	}
	if v, _ := pbGet(f, 3); !bytes.Equal(v.Value, []byte{0xde, 0xad}) {
		t.Fatalf("field 3: %x", v.Value)
	}
}

func TestPBDecodeRejectsBadWireType(t *testing.T) {
	// Wire type 3 (start-group) is unsupported: schema drift must error.
	if _, err := pbDecode([]byte{0x0b}); err == nil {
		t.Fatal("wire type 3 must fail decode")
	}
}

func TestEncodeCursorAgentRequestFoldsSystem(t *testing.T) {
	// THE regression: a system prompt in the run request field 8 kills the
	// AgentService turn (live-verified 3/3 probes on 2026-09-09). The
	// builder must never emit field 8 and must fold system into user text.
	u := &types.ChatRequest{
		Model: "gpt-5.2",
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: []types.Part{{Type: types.PartText, Text: "Be terse."}}},
			{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "Reply PONG"}}},
		},
	}
	body, err := EncodeCursorAgentRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	// Frame: 5-byte header + AgentClientMessage.
	if body[0] != 0 || body[1] != 0 {
		t.Fatalf("uncompressed connect frame expected, flags=%d", body[0])
	}
	f, err := pbDecode(body[5:])
	if err != nil {
		t.Fatal(err)
	}
	runReqField, ok := pbGet(f, 1)
	if !ok {
		t.Fatal("run_request (field 1) missing")
	}
	rr, err := pbDecode(runReqField.Value)
	if err != nil {
		t.Fatal(err)
	}
	if _, has8 := pbGet(rr, 8); has8 {
		t.Fatal("run request must NOT carry a system prompt in field 8 (kills the turn upstream)")
	}
	// Conversation → user action → user message {1: text, 2: uuid}.
	conv, _ := pbGet(rr, 2)
	ua, _ := pbDecode(conv.Value)
	umf, _ := pbGet(ua, 1)
	um, _ := pbDecode(umf.Value)
	text := pbFirst(um, 1)
	if !strings.Contains(text, "Be terse.") || !strings.Contains(text, "Reply PONG") {
		t.Fatalf("system text must be folded into the user turn, got %q", text)
	}
	if strings.Index(text, "Be terse.") > strings.Index(text, "Reply PONG") {
		t.Fatal("system text must precede the user turn")
	}
	// Model rides on field 9 {1: name}.
	mf, _ := pbGet(rr, 9)
	mm, _ := pbDecode(mf.Value)
	if got := pbFirst(mm, 1); got != "gpt-5.2" {
		t.Fatalf("model: %q", got)
	}
}

// TestEncodeCursorAgentRequestRewritesAutoLane pins the model-id mapping
// Cursor needs: it has no "auto" model, and sending the id verbatim ends the
// AgentService turn with zero content (live: cursor/auto → empty,
// cursor/default → "PONG"). Real ids must ride unchanged.
func TestEncodeCursorAgentRequestRewritesAutoLane(t *testing.T) {
	wireModel := func(model string) string {
		t.Helper()
		u := &types.ChatRequest{Model: model, Messages: []types.Message{
			{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "hi"}}},
		}}
		body, err := EncodeCursorAgentRequest(u)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		f, err := pbDecode(body[5:])
		if err != nil {
			t.Fatal(err)
		}
		rrF, ok := pbGet(f, 1)
		if !ok {
			t.Fatal("run_request (field 1) missing")
		}
		rr, err := pbDecode(rrF.Value)
		if err != nil {
			t.Fatal(err)
		}
		mf, _ := pbGet(rr, 9)
		mm, _ := pbDecode(mf.Value)
		return pbFirst(mm, 1)
	}
	for model, want := range map[string]string{
		"auto":              "default",
		"auto-cost":         "default",
		"auto-balance":      "default",
		"auto-intelligence": "default",
		"gpt-5.2":           "gpt-5.2",
		"composer-2.5":      "composer-2.5",
		// A PROVIDER PREFIX must not ride the wire: this id arrives via a
		// direct route entry, i.e. a client asking for the string /v1/models
		// advertises (live 2026-09-21 — cursor answered its own
		// `400 AI Model Not Found` for it). Only the last segment is a model.
		"cursor/auto":        "default",
		"cursor/cursor/auto": "default",
		"openrouter/gpt-5.2": "gpt-5.2",
	} {
		if got := wireModel(model); got != want {
			t.Fatalf("agent wire: model %q said %q, want %q", model, got, want)
		}
		// Tool schemas and "-thinking" ids ride ChatService instead, which
		// carries the model in Model{1} under field 5 — same mapping.
		body, err := EncodeCursorChatRequest(&types.ChatRequest{Model: model, Messages: []types.Message{
			{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "hi"}}},
		}})
		if err != nil {
			t.Fatalf("%s: chat: %v", model, err)
		}
		cf, err := pbDecode(body[5:])
		if err != nil {
			t.Fatal(err)
		}
		inner, _ := pbGet(cf, 1)
		cr, err := pbDecode(inner.Value)
		if err != nil {
			t.Fatal(err)
		}
		cmf, _ := pbGet(cr, 5)
		cmm, _ := pbDecode(cmf.Value)
		if got := pbFirst(cmm, 1); got != want {
			t.Fatalf("chat wire: model %q said %q, want %q", model, got, want)
		}
	}
}

func TestEncodeCursorChatRequestCarriesTools(t *testing.T) {
	u := &types.ChatRequest{
		Model:    "claude-4.5-haiku",
		Messages: []types.Message{{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "weather?"}}}},
		Tools: []types.Tool{{
			Name:        "get_weather",
			Description: "Get weather",
			Schema:      []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
	body, err := EncodeCursorChatRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	f, err := pbDecode(body[5:])
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := pbGet(f, 1)
	if !ok {
		t.Fatal("StreamUnifiedChatRequestWithTools.request missing")
	}
	req, err := pbDecode(wrapped.Value)
	if err != nil {
		t.Fatal(err)
	}
	// Messages on field 1 (user content), tools on field 34.
	msgs := 0
	for _, fd := range req {
		if fd.Num == 1 {
			msgs++
		}
	}
	if msgs != 1 {
		t.Fatalf("want 1 message, got %d", msgs)
	}
	toolField, ok := pbGet(req, 34)
	if !ok {
		t.Fatal("MCP tool (field 34) missing")
	}
	tool, err := pbDecode(toolField.Value)
	if err != nil {
		t.Fatal(err)
	}
	if got := pbFirst(tool, 1); got != "get_weather" {
		t.Fatalf("tool name: %q", got)
	}
	if got := pbFirst(tool, 4); got != "custom" {
		t.Fatalf("tool server: %q", got)
	}
	// Agentic mode must be flagged on for tool-bearing requests.
	if v, _ := pbGet(req, 27); v.Num64 != 1 {
		t.Fatal("is_agentic (field 27) must be 1 with tools")
	}
}

func TestCursorAgentEventsTextAndUsage(t *testing.T) {
	// Build an AgentServerMessage: interaction_update{1: text{1: "PONG"}}
	var textMsg []byte
	textMsg = pbString(textMsg, 1, "PONG")
	var update []byte
	update = pbBytes(update, 1, textMsg)
	update = pbBytes(update, 14, func() []byte {
		var u []byte
		u = pbUvarint(u, 1, 11859)
		u = pbUvarint(u, 2, 6)
		return u
	}())
	var msg []byte
	msg = pbBytes(msg, 1, update)

	events := CursorAgentEvents(msg)
	var gotText string
	var usage *types.Usage
	for _, e := range events {
		switch e.Kind {
		case EvDelta:
			gotText = e.Text
		case EvStop:
			usage = e.Usage
		}
	}
	if gotText != "PONG" {
		t.Fatalf("text delta: %q", gotText)
	}
	if usage == nil || usage.InputTokens != 11859 || usage.OutputTokens != 6 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestCursorAgentNeedsReply(t *testing.T) {
	// exec_server_request{10: request_server_info} → reply needed.
	var exec []byte
	exec = pbBytes(exec, 10, nil)
	var msg []byte
	msg = pbBytes(msg, 2, exec)
	if !CursorAgentNeedsReply(msg) {
		t.Fatal("field-10 exec request must trigger the handshake reply")
	}
	// Other exec variants (IDE tools) must NOT be answered with the context
	// reply — the turn fails instead.
	var shell []byte
	shell = pbBytes(shell, 11, nil)
	var msg2 []byte
	msg2 = pbBytes(msg2, 2, shell)
	if CursorAgentNeedsReply(msg2) {
		t.Fatal("non-context exec request must not be answered")
	}
}

func TestCursorChatEventsToolCallStream(t *testing.T) {
	// StreamUnifiedChatResponseWithTools{1: ClientSideToolV2Call
	// {3: id, 27: MCPParams{1: tool{1: name, 3: args}}}} fragment 1, then an
	// args continuation on the same id.
	var tool []byte
	tool = pbString(tool, 1, "get_weather")
	tool = pbString(tool, 3, `{"city":`)
	var mcpParams []byte
	mcpParams = pbBytes(mcpParams, 1, tool)
	var call []byte
	call = pbString(call, 3, "call_1\nextra")
	call = pbBytes(call, 27, mcpParams)
	var frame1 []byte
	frame1 = pbBytes(frame1, 1, call)

	st := &CursorChatState{}
	events := CursorChatEvents(frame1, "claude-4.5-haiku", st)
	var sawStart bool
	var sawArgs string
	for _, e := range events {
		if e.Kind == EvPartStart && e.ToolName == "get_weather" && e.ToolID == "call_1" {
			sawStart = true
		}
		if e.Kind == EvDelta && e.PartType == types.PartToolUse {
			sawArgs += e.ToolArgs
		}
	}
	if !sawStart || sawArgs != `{"city":` {
		t.Fatalf("first fragment: start=%v args=%q", sawStart, sawArgs)
	}

	// Continuation fragment: same id, more args (top-level raw_args shape).
	var call2 []byte
	call2 = pbString(call2, 3, "call_1")
	call2 = pbString(call2, 9, "get_weather")
	call2 = pbString(call2, 10, `"Paris"}`)
	var frame2 []byte
	frame2 = pbBytes(frame2, 1, call2)
	events = CursorChatEvents(frame2, "claude-4.5-haiku", st)
	var cont string
	idx := -1
	for _, e := range events {
		if e.Kind == EvDelta && e.PartType == types.PartToolUse {
			cont += e.ToolArgs
			idx = e.Index
		}
	}
	if cont != `"Paris"}` || idx != 0 {
		t.Fatalf("continuation: args=%q idx=%d", cont, idx)
	}

	// Text + thinking ride field 2.
	var resp []byte
	resp = pbString(resp, 1, "Hello")
	var think []byte
	think = pbString(think, 1, "reasoning")
	var withThink []byte
	withThink = pbString(withThink, 1, "Hello")
	withThink = pbBytes(withThink, 25, think)
	var frame3 []byte
	frame3 = pbBytes(frame3, 2, withThink)
	events = CursorChatEvents(frame3, "claude-4.5-haiku", st)
	var text, thinking string
	for _, e := range events {
		if e.Kind == EvDelta && e.PartType == types.PartText {
			text += e.Text
		}
		if e.Kind == EvDelta && e.PartType == types.PartThinking {
			thinking += e.Thinking
		}
	}
	if text != "Hello" || thinking != "reasoning" {
		t.Fatalf("text=%q thinking=%q", text, thinking)
	}
}

// chatThinkingFrame builds one ChatService content frame with optional field-1
// text and/or field-25 thinking (the composer answer channel).
func chatThinkingFrame(text, thinking string) []byte {
	var resp []byte
	if text != "" {
		resp = pbString(resp, 1, text)
	}
	if thinking != "" {
		var th []byte
		th = pbString(th, 1, thinking)
		resp = pbBytes(resp, 25, th)
	}
	return pbBytes(nil, 2, resp)
}

func TestCursorComposerThinkingSplitsToContent(t *testing.T) {
	// Live composer dialect (9router cursor-composer-thinking.test.js): the
	// visible answer rides field 25 after </think>. Emitting the whole blob
	// as reasoning_content left clients with an empty answer and a thinking
	// box full of the real reply.
	st := &CursorChatState{}
	events := CursorChatEvents(chatThinkingFrame("", "private reasoning that must not leak</think>OK"), "composer-2.5", st)
	var text, thinking string
	for _, e := range events {
		if e.Kind == EvDelta && e.PartType == types.PartText {
			text += e.Text
		}
		if e.Kind == EvDelta && e.PartType == types.PartThinking {
			thinking += e.Thinking
		}
	}
	if text != "OK" {
		t.Fatalf("composer visible content=%q, want OK", text)
	}
	if thinking != "" {
		t.Fatalf("composer must not emit thinking parts, got %q", thinking)
	}

	// Streaming across frames: thinking first, then tag+partial answer, then rest.
	st = &CursorChatState{}
	var streamed string
	for _, chunk := range []string{"private reasoning", " that must not leak</think>O", "K"} {
		for _, e := range CursorChatEvents(chatThinkingFrame("", chunk), "composer-2.5-fast", st) {
			if e.Kind == EvDelta && e.PartType == types.PartText {
				streamed += e.Text
			}
			if e.Kind == EvDelta && e.PartType == types.PartThinking {
				t.Fatalf("composer stream leaked thinking: %q", e.Thinking)
			}
		}
	}
	if streamed != "OK" {
		t.Fatalf("streamed content=%q, want OK", streamed)
	}

	// Provider-prefixed id still matches.
	st = &CursorChatState{}
	events = CursorChatEvents(chatThinkingFrame("", "x</think>Y"), "cursor/composer-2", st)
	text = ""
	for _, e := range events {
		if e.Kind == EvDelta && e.PartType == types.PartText {
			text += e.Text
		}
	}
	if text != "Y" {
		t.Fatalf("prefixed composer content=%q, want Y", text)
	}
}

func TestCursorNonComposerThinkingStaysThinking(t *testing.T) {
	// Non-composer models keep field 25 as reasoning — never promote the
	// post-</think> suffix into content (would invent an answer).
	st := &CursorChatState{}
	events := CursorChatEvents(chatThinkingFrame("", "private reasoning</think>SHOULD_NOT_APPEAR"), "gpt-5.3-codex", st)
	var text, thinking string
	for _, e := range events {
		if e.Kind == EvDelta && e.PartType == types.PartText {
			text += e.Text
		}
		if e.Kind == EvDelta && e.PartType == types.PartThinking {
			thinking += e.Thinking
		}
	}
	if text != "" {
		t.Fatalf("non-composer invented content %q", text)
	}
	if thinking != "private reasoning</think>SHOULD_NOT_APPEAR" {
		t.Fatalf("thinking=%q", thinking)
	}
}

func TestCursorSSEStreamComposerThinkingAsContent(t *testing.T) {
	// End-to-end: synthetic OpenAI SSE must carry content=OK and must not
	// leak the thinking prefix into reasoning_content.
	payload := chatThinkingFrame("", "private reasoning that must not leak</think>OK")
	stream := CursorSSEStream(bytes.NewReader(wrapConnectFrame(payload)), "composer-2.5", false, nil)
	out := readAllString(t, stream)
	if !strings.Contains(out, `"content":"OK"`) {
		t.Fatalf("SSE must carry visible content: %s", out)
	}
	if strings.Contains(out, "private reasoning") {
		t.Fatalf("SSE leaked thinking prefix: %s", out)
	}
	if strings.Contains(out, `"reasoning_content"`) {
		t.Fatalf("composer SSE must not emit reasoning_content: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("SSE must end with [DONE]: %s", out)
	}
}

func TestCursorJSONErrorRateLimit(t *testing.T) {
	ae, ok := CursorJSONError([]byte(`{"error":{"code":"resource_exhausted","message":"quota"}}`))
	if !ok {
		t.Fatal("error frame must decode")
	}
	if ae.Status != 429 {
		t.Fatalf("resource_exhausted must map to 429, got %d", ae.Status)
	}
	if ae.Message != "quota" {
		t.Fatalf("message: %q", ae.Message)
	}
	// Non-error payload: not an error frame.
	if _, ok := CursorJSONError([]byte(`{"other":1}`)); ok {
		t.Fatal("payload without error.message must not decode as error")
	}
	// Protobuf payload: not JSON.
	if _, ok := CursorJSONError([]byte{0x0a, 0x01, 0x78}); ok {
		t.Fatal("binary payload must not decode as JSON error")
	}
}

// A 200 + zero content frames + an error in the TRAILER frame is how Cursor
// reports an unauthenticated session (and any other turn-level failure). The
// trailer used to be skipped as opaque metadata, so the turn surfaced as the
// misleading "empty response (model may be unavailable on this path)".
// Live wire capture 2026-09-20 (api2 ChatService, composer-2.5).
func TestCursorSSEStreamTrailerError(t *testing.T) {
	trailer := []byte(`{"error":{"code":"unauthenticated","message":"Error","details":[{"debug":{"error":"ERROR_NOT_LOGGED_IN","details":{"title":"Authentication error","detail":"If you are logged in, try logging out and back in."}}}]}}`)
	frame := wrapConnectFrame(trailer)
	frame[0] = connectFlagTrailer

	stream := CursorSSEStream(bytes.NewReader(frame), "composer-2.5", false, nil)
	buf := make([]byte, 4096)
	_, err := stream.Read(buf)
	if err == nil {
		t.Fatal("a trailer-carried error must surface as a reader error")
	}
	if !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("error must carry the upstream reason, got: %v", err)
	}
	if strings.Contains(err.Error(), "empty response") {
		t.Fatalf("trailer error must not degrade to the empty-response message: %v", err)
	}
	if ae, ok := err.(*types.APIError); !ok || ae.Status != 401 {
		t.Fatalf("unauthenticated must map to 401 so the pool benches it, got %v", err)
	}
}

func TestDecodeCursorErrorStatusLift(t *testing.T) {
	// Non-200 body: status passes through.
	ae := DecodeCursorError([]byte(`{"error":{"code":"unauthenticated","message":"bad token"}}`), 401)
	if ae.Status != 401 {
		t.Fatalf("status: %d", ae.Status)
	}
}

func TestCursorSSEStreamAgentHappyPath(t *testing.T) {
	// A full agent stream: text frame then done frame → OpenAI SSE with
	// content delta + usage + [DONE].
	var textMsg []byte
	textMsg = pbString(textMsg, 1, "PONG")
	var update []byte
	update = pbBytes(update, 1, textMsg)
	var frameA []byte
	frameA = pbBytes(frameA, 1, update)
	var done []byte
	done = pbBytes(done, 14, func() []byte {
		var u []byte
		u = pbUvarint(u, 1, 10)
		u = pbUvarint(u, 2, 2)
		return u
	}())
	var frameB []byte
	frameB = pbBytes(frameB, 1, done)

	stream := CursorSSEStream(bytes.NewReader(concatFrames(
		wrapConnectFrame(frameA),
		wrapConnectFrame(frameB),
	)), "gpt-5.2", true, nil)
	out := readAllString(t, stream)
	if !strings.Contains(out, `"content":"PONG"`) {
		t.Fatalf("SSE must carry the text delta: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":10`) {
		t.Fatalf("SSE must carry upstream usage: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("SSE must end with [DONE]: %s", out)
	}
}

func TestCursorSSEStreamEmptyIsError(t *testing.T) {
	// Zero content frames (model not usable on the service — observed live)
	// must surface as a reader error, not an empty success.
	stream := CursorSSEStream(bytes.NewReader(wrapConnectFrame(func() []byte {
		var msg []byte
		return pbBytes(msg, 13, nil) // benign agent_error, no content
	}())), "claude-4.5-sonnet", true, nil)
	buf := make([]byte, 4096)
	_, err := stream.Read(buf)
	if err == nil {
		t.Fatal("empty stream must error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("error: %v", err)
	}
}

func TestCursorSSEStreamToolCallNoOutput(t *testing.T) {
	// A tool-call-only stream (no text delta): the upstream emits
	// EvStart on the first fragment and EvPartStart for the tool.
	// Without a case for EvStart in CursorSSEStream's switch the
	// role chunk was never written if no text delta followed, and
	// the client received zero output. Regression: the SSE must
	// open with the assistant role chunk and carry the tool name.
	var tool []byte
	tool = pbString(tool, 1, "get_weather")
	var mcpParams []byte
	mcpParams = pbBytes(mcpParams, 1, tool)
	var call []byte
	call = pbString(call, 3, "call_1")
	call = pbBytes(call, 27, mcpParams)
	var frame []byte
	frame = pbBytes(frame, 1, call)

	stream := CursorSSEStream(bytes.NewReader(wrapConnectFrame(frame)), "claude-4.5-haiku", false, nil)
	out := readAllString(t, stream)
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Fatalf("SSE must open with the role chunk, got: %s", out)
	}
	if !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("SSE must carry the tool name, got: %s", out)
	}
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("SSE must not carry an empty tool name, got: %s", out)
	}
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("SSE must carry tool_calls block, got: %s", out)
	}
}

func TestCursorSSEStreamThinkingDeltaFlushes(t *testing.T) {
	// Every part type must flush, not just text. A turn whose first frames
	// carry only reasoning_content (or only tool-arg fragments) would sit in
	// the sb buffer until stream end — the exact stall incremental flushing
	// exists to prevent. Cursor's ChatService holds the connection on 10s
	// keepalives, so "until stream end" means "until the client times out".
	var inner []byte
	inner = pbString(inner, 1, "REASONING_CHUNK_XYZ")
	var frame []byte
	frame = pbBytes(frame, 2, pbBytes(nil, 25, inner))

	pr, pw := io.Pipe()
	stream := CursorSSEStream(pr, "claude-4.5-haiku", false, nil)
	if _, err := pw.Write(wrapConnectFrame(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	defer pw.Close()

	chunks := make(chan string, 8)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				chunks <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	var seen strings.Builder
	deadline := time.After(2 * time.Second)
	for !strings.Contains(seen.String(), "REASONING_CHUNK_XYZ") {
		select {
		case c := <-chunks:
			seen.WriteString(c)
		case <-deadline:
			t.Fatalf("reasoning_content not delivered within 2s while the upstream stream is still open; got: %s", seen.String())
		}
	}
}

func TestCursorSSEStreamToolNameEmptyFallback(t *testing.T) {
	// EvPartStart must never emit a tool call with an empty
	// function.name. Strict validators reject it. The code path
	// is defensive: decodeToolCall already returns nil for an
	// empty name, but if the guard is ever bypassed the SSE
	// must fall back to unknownToolName instead of "".
	if unknownToolName != "unknown_tool" {
		t.Fatalf("unknownToolName = %q, want %q", unknownToolName, "unknown_tool")
	}
}

func concatFrames(frames ...[]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func readAllString(t *testing.T, r interface{ Read([]byte) (int, error) }) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func TestCursorSSEStreamFlushesBeforeStreamEnd(t *testing.T) {
	// The load-bearing fix in CursorSSEStream is the incremental flush:
	// Cursor's ChatService parks the connection on 10s keepalive frames,
	// so a client that waits for stream end sees nothing until timeout.
	// This test reads ONE chunk from the returned reader while the upstream
	// frame source is still open. Without flush() the first chunk stays in
	// the sb buffer and this read blocks until the source closes.
	var tool []byte
	tool = pbString(tool, 1, "get_weather")
	var mcpParams []byte
	mcpParams = pbBytes(mcpParams, 1, tool)
	var call []byte
	call = pbString(call, 3, "call_1")
	call = pbBytes(call, 27, mcpParams)
	var frame []byte
	frame = pbBytes(frame, 1, call)

	pr, pw := io.Pipe()
	stream := CursorSSEStream(pr, "claude-4.5-haiku", false, nil)

	// Feed the tool frame but leave the pipe open: no EOF, no EvStop.
	if _, err := pw.Write(wrapConnectFrame(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	defer pw.Close()

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := stream.Read(buf)
		if n == 0 && err != nil {
			got <- ""
			return
		}
		got <- string(buf[:n])
	}()

	select {
	case chunk := <-got:
		if !strings.Contains(chunk, "data: ") {
			t.Fatalf("first chunk is not an SSE frame: %q", chunk)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no chunk within 3s while the upstream stream is still open: CursorSSEStream buffers until stream end (missing flush)")
	}
}
