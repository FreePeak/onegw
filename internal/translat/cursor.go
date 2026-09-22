package translat

// Cursor wire format (issue #12 follow-up): Cursor IDE OAuth upstreams,
// Connect-RPC protobuf over HTTP/2.
//
// Two services, two paths, one kind:
//   - AgentService (agent.api5.cursor.sh /agent.v1.AgentService/Run): TEXT
//     turns. Verified live 2026-09-09: a non-empty system prompt in the run
//     request kills the turn (stream ends after the context handshake, zero
//     content, 3/3 probes), while the same turn without one streams text +
//     usage 3/3. The gateway therefore folds system text into the user turn.
//   - ChatService (api2.cursor.sh /aiserver.v1.ChatService/
//     StreamUnifiedChatWithTools): the tool-capable path. Tool defs ride as
//     MCP-style blobs (field 34); tool calls stream back on field 1.
//
// The server layer (provider.DoCursor) drives transport; this file owns the
// wire: framing, protobuf codec, checksum/headers, request builders, and
// response decoders shared by both paths.
//
// Provenance: ported from 9router open-sse/{executors/cursor.js,
// utils/cursorProtobuf.js, utils/cursorChecksum.js} (read-only reference),
// with the live-probe deltas noted inline.

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"onegw/internal/types"
)

// unknownToolName is the placeholder used for a tool call whose
// upstream name could not be decoded. Matches the convention in
// gemini.go (toolNameForMsgs) and server/toolblocks.go
// (normalizeToolBlocks).
const unknownToolName = "unknown_tool"

// CursorClientVersion fingerprints the impersonated Cursor IDE build.
// 9router pins 3.12.17 (their commit 6994cd1f); accepted upstream today.
const CursorClientVersion = "3.12.17"

// CursorClientCommit is the IDE build hash sent alongside the version.
const CursorClientCommit = "0fb762053c34788bb7760d5673f8a6d4c8589d50"

// CursorAgentEndpointHost is the AgentService host (h2-only upstream).
const CursorAgentEndpointHost = "https://agent.api5.cursor.sh"

// CursorAgentRunPath is the AgentService streaming-RPC path.
const CursorAgentRunPath = "/agent.v1.AgentService/Run"

// CursorChatPath is the ChatService streaming-RPC path (api2 host).
const CursorChatPath = "/aiserver.v1.ChatService/StreamUnifiedChatWithTools"

// CursorChatEndpointHost is the ChatService kind-default host.
const CursorChatEndpointHost = "https://api2.cursor.sh"

// ---------------------------------------------------------------------------
// Protobuf primitives
// ---------------------------------------------------------------------------

const (
	pbVarint  = 0
	pbFixed64 = 1
	pbLen     = 2
	pbFixed32 = 5
)

func pbAppendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func pbTag(b []byte, field, wire int) []byte {
	return pbAppendVarint(b, uint64(field)<<3|uint64(wire))
}

func pbBytes(b []byte, field int, v []byte) []byte {
	b = pbTag(b, field, pbLen)
	b = pbAppendVarint(b, uint64(len(v)))
	return append(b, v...)
}

func pbString(b []byte, field int, s string) []byte {
	return pbBytes(b, field, []byte(s))
}

func pbUvarint(b []byte, field int, v uint64) []byte {
	b = pbTag(b, field, pbVarint)
	return pbAppendVarint(b, v)
}

// pbField is one decoded protobuf field.
type pbField struct {
	Num   int
	Wire  int
	Value []byte // LEN + fixed payloads
	Num64 uint64 // varint values
}

// pbDecode splits a protobuf message into ordered fields. Unrecognized wire
// types fail loudly: a Cursor schema change must error, not decode garbage.
func pbDecode(data []byte) ([]pbField, error) {
	var out []pbField
	pos := 0
	for pos < len(data) {
		tag, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return nil, fmt.Errorf("cursor: bad protobuf tag at byte %d", pos)
		}
		pos += n
		f := pbField{Num: int(tag >> 3), Wire: int(tag & 7)}
		switch f.Wire {
		case pbVarint:
			v, n := binary.Uvarint(data[pos:])
			if n <= 0 {
				return nil, fmt.Errorf("cursor: bad varint in field %d", f.Num)
			}
			f.Num64 = v
			pos += n
		case pbLen:
			l, n := binary.Uvarint(data[pos:])
			if n <= 0 || int(l) < 0 || pos+n+int(l) > len(data) {
				return nil, fmt.Errorf("cursor: bad length in field %d", f.Num)
			}
			pos += n
			f.Value = data[pos : pos+int(l)]
			pos += int(l)
		case pbFixed64:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("cursor: truncated fixed64 in field %d", f.Num)
			}
			f.Value = data[pos : pos+8]
			pos += 8
		case pbFixed32:
			if pos+4 > len(data) {
				return nil, fmt.Errorf("cursor: truncated fixed32 in field %d", f.Num)
			}
			f.Value = data[pos : pos+4]
			pos += 4
		default:
			return nil, fmt.Errorf("cursor: unsupported wire type %d (field %d)", f.Wire, f.Num)
		}
		out = append(out, f)
	}
	return out, nil
}

// pbGet returns the first occurrence of a field number.
func pbGet(fields []pbField, num int) (pbField, bool) {
	for _, f := range fields {
		if f.Num == num {
			return f, true
		}
	}
	return pbField{}, false
}

func pbFirst(fields []pbField, num int) string {
	if f, ok := pbGet(fields, num); ok {
		return string(f.Value)
	}
	return ""
}

func boolUint(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// cursorUUID returns a random RFC 4122 v4 UUID (crypto/rand). commandcode.go
// owns the package-level newUUID; this kind-local alias keeps call sites
// symmetric without touching that file.
func cursorUUID() string { return newUUID() }

// ---------------------------------------------------------------------------
// Connect-RPC framing
// ---------------------------------------------------------------------------

const (
	connectFlagGzip    = 0x01
	connectFlagTrailer = 0x02
)

// wrapConnectFrame prefixes payload with the 5-byte Connect-RPC frame header
// (1 flag byte + u32 BE length). Requests always go uncompressed — Cursor
// rejects compressed request frames.
func wrapConnectFrame(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// decompressConnectFrame inflates a compressed frame body (gzip, with a raw
// deflate fallback for the TRAILER-variant frames Cursor occasionally emits).
func decompressConnectFrame(payload []byte) ([]byte, error) {
	if zr, err := gzip.NewReader(bytes.NewReader(payload)); err == nil {
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err == nil {
			return raw, nil
		}
	}
	zr := flate.NewReader(bytes.NewReader(payload))
	defer zr.Close()
	return io.ReadAll(zr)
}

// readConnectFrames splits a response stream into uncompressed frame
// payloads, gunzipping where flagged and skipping trailers. Unknown flag
// bits pass the payload through untouched (forward compatible).
func readConnectFrames(r io.Reader, yield func(payload []byte) error) error {
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		flags := hdr[0]
		length := binary.BigEndian.Uint32(hdr[1:5])
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		if flags&connectFlagGzip != 0 {
			raw, err := decompressConnectFrame(payload)
			if err != nil {
				return fmt.Errorf("cursor: frame decompress: %w", err)
			}
			payload = raw
		}
		if flags&connectFlagTrailer != 0 {
			// End-of-stream metadata. Normally `{}`, but Cursor also posts
			// the turn's error here — an unauthenticated ChatService call
			// returns 200 + a trailer frame carrying
			// {"error":{"code":"unauthenticated",...}} and zero content
			// frames (live 2026-09-20), which otherwise degrades into the
			// misleading "empty response (model may be unavailable)".
			if ae, ok := CursorJSONError(payload); ok {
				return ae
			}
			continue
		}
		if err := yield(payload); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// Checksum + header fingerprints (Jyh cipher)
// ---------------------------------------------------------------------------

// cursorChecksum renders the x-cursor-checksum value: a time-derived 6-byte
// Jyh-cipher blob, custom-alphabet base64 (unpadded), then the machine id.
// Byte-for-byte port of 9router's generateCursorChecksum.
func cursorChecksum(machineID string, now time.Time) string {
	ts := now.UnixMilli() / 1_000_000 // Math.floor(Date.now() / 1e6) — Date.now() is milliseconds
	// The reference implementation (9router cursorChecksum.js, mirrored from
	// the Cursor IDE) extracts the timestamp bytes with JS bitwise shifts,
	// which mask the shift count to 5 bits: >>40 acts as >>8, >>32 as >>0.
	// Replicate that exactly — the resulting byte pattern is what upstream
	// validates (vector: 6fn747On… for Date=1788945600000, machineId
	// b9854c5c-…; a naive Go >>40 passes 0x00 and upstream rejects auth).
	blob := []byte{
		byte(jsShift(ts, 40)), byte(jsShift(ts, 32)), byte(jsShift(ts, 24)),
		byte(jsShift(ts, 16)), byte(jsShift(ts, 8)), byte(jsShift(ts, 0)),
	}
	t := byte(165)
	for i := range blob {
		blob[i] = ((blob[i] ^ t) + byte(i)) & 0xFF
		t = blob[i]
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var sb strings.Builder
	for i := 0; i < len(blob); i += 3 {
		a := blob[i]
		b, c := byte(0), byte(0)
		if i+1 < len(blob) {
			b = blob[i+1]
		}
		if i+2 < len(blob) {
			c = blob[i+2]
		}
		sb.WriteByte(alphabet[a>>2])
		sb.WriteByte(alphabet[(a&3)<<4|b>>4])
		if i+1 < len(blob) {
			sb.WriteByte(alphabet[(b&15)<<2|c>>6])
		}
		if i+2 < len(blob) {
			sb.WriteByte(alphabet[c&63])
		}
	}
	return sb.String() + machineID
}

// jsShift replicates JavaScript's signed 32-bit right shift with the shift
// count masked to 5 bits (ECMA-262 <<, >>, >>> all take count & 31).
func jsShift(v int64, s uint) int64 {
	return int64(int32(v) >> (s & 31))
}

// cursorUUIDv5 derives the x-session-id: UUIDv5 in the DNS namespace over
// the bearer token (9router's generateSessionId, uuid.v5(token, DNS)).
func cursorUUIDv5(token string) string {
	// RFC 4122 DNS namespace 6ba7b810-9dad-11d1-80b4-00c04fd430c8
	ns, _ := hexUUID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	sum := sha1.Sum(append(ns, token...))
	sum[6] = (sum[6] & 0x0f) | 0x50 // version 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func hexUUID(s string) ([]byte, error) {
	var v [16]byte
	j := 0
	for i := 0; i < len(s) && j < 32; i++ {
		c := s[i]
		var lo byte
		switch {
		case c >= '0' && c <= '9':
			lo = c - '0'
		case c >= 'a' && c <= 'f':
			lo = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			lo = c - 'A' + 10
		default:
			continue
		}
		if j%2 == 0 {
			v[j/2] |= lo << 4
		} else {
			v[j/2] |= lo
		}
		j++
	}
	if j != 32 {
		return nil, fmt.Errorf("cursor: bad uuid %q", s)
	}
	return v[:], nil
}

// cursorTokenSHA256 is the x-client-key header: sha256(token) hex.
func cursorTokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// CursorMachineIDFallback derives a machine id from the token when the
// account config carries none (9router: generateHashed64Hex(token,
// "machineId")). Upstream accepts it; the real storage.serviceMachineId is
// preferred and set per account in config.
func CursorMachineIDFallback(token string) string {
	return sha256Hex(token + "machineId")
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// CursorHeaders builds the full Cursor upstream header set as "key: value"
// pairs (the caller Set()s them). Machine id: explicit config value, else
// the token-derived fallback. Ghost mode always on (privacy mode).
func CursorHeaders(token, machineID string) []string {
	if machineID == "" {
		machineID = CursorMachineIDFallback(token)
	}
	return []string{
		"Authorization: Bearer " + token,
		"connect-accept-encoding: gzip",
		"connect-protocol-version: 1",
		"Content-Type: application/connect+proto",
		"User-Agent: connect-es/1.6.1",
		"x-amzn-trace-id: Root=" + cursorUUID(),
		"x-client-key: " + cursorTokenSHA256(token),
		"x-cursor-checksum: " + cursorChecksum(machineID, time.Now()),
		"x-cursor-client-version: " + CursorClientVersion,
		"x-cursor-client-commit: " + CursorClientCommit,
		"x-cursor-client-type: ide",
		"x-cursor-client-os: macos",
		"x-cursor-client-arch: aarch64",
		"x-cursor-client-device-type: desktop",
		"x-cursor-config-version: " + cursorUUID(),
		"x-cursor-timezone: Asia/Ho_Chi_Minh",
		"x-ghost-mode: true",
		"x-request-id: " + cursorUUID(),
		"x-session-id: " + cursorUUIDv5(token),
	}
}

// ---------------------------------------------------------------------------
// AgentService request
// ---------------------------------------------------------------------------

// buildAgentRunFrame encodes agent.v1.AgentClientMessage.run_request (field
// 1): empty ConversationState (fresh session), user_message action with the
// (system-folded) text, requested model {1: name, 7: true}. The caller MUST
// NOT pass system text in field 8 — the upstream kills such turns (live
// evidence in the file header); system content rides inside userText.
func buildAgentRunFrame(userText, model string) []byte {
	var userMessage []byte // UserMessageAction.user_message {1: text, 2: uuid}
	userMessage = pbString(userMessage, 1, userText)
	userMessage = pbString(userMessage, 2, cursorUUID())

	userAction := pbBytes(nil, 1, userMessage)  // UserMessageAction
	conversation := pbBytes(nil, 1, userAction) // ConversationAction.user_action
	requestedModel := append(pbString(nil, 1, model), pbUvarint(nil, 7, 1)...)

	var runRequest []byte                    // agent.v1.RunRequest
	runRequest = pbBytes(runRequest, 1, nil) // empty ConversationStateStructure
	runRequest = pbBytes(runRequest, 2, conversation)
	runRequest = pbBytes(runRequest, 9, requestedModel)

	clientMessage := pbBytes(nil, 1, runRequest) // AgentClientMessage.run_request
	return wrapConnectFrame(clientMessage)
}

// requestContextResponse answers the AgentService context handshake:
// AgentClientMessage.exec_client_message {2: request_context_result
// {1: success {1: empty}}} — the live-proven wire bytes are 120652040a020a00.
func requestContextResponse() []byte {
	success := pbBytes(nil, 1, nil)
	result := pbBytes(nil, 1, success) // field 1: request_context_result.success
	exec := pbBytes(nil, 10, result)   // field 10: exec_client_message.request_context_result
	// field 2 of AgentClientMessage, no extra wrapper (live-proven:
	// 120652040a020a00 = f2{ f10{ f1{ f1{} } } }).
	return wrapConnectFrame(pbBytes(nil, 2, exec))
}

// CursorAgentReply is the handshake response frame for the exec-request
// field-10 (request_server_info) variant. Write it mid-stream whenever
// CursorAgentNeedsReply reports true.
var CursorAgentReply = requestContextResponse()

// CursorAgentNeedsReply reports whether a decoded AgentServerMessage is the
// context handshake (exec_server_request carrying request_server_info,
// field 10). Any other exec variant is an IDE-tool request the gateway
// cannot service — the caller fails the turn.
func CursorAgentNeedsReply(payload []byte) bool {
	fields, err := pbDecode(payload)
	if err != nil {
		return false
	}
	if f, ok := pbGet(fields, 2); ok { // exec_server_request
		if ef, eerr := pbDecode(f.Value); eerr == nil {
			if _, isCtx := pbGet(ef, 10); isCtx {
				return true
			}
		}
	}
	return false
}

// CursorAgentEvents decodes one AgentServerMessage payload into unified
// events: text deltas (interaction_update.1.1.1), the terminal stop with
// upstream usage (interaction_update.14), or an error for unsupported exec
// requests. The empty agent_error frame (field 13, empty) is benign noise —
// observed immediately after healthy DONE frames in live probes.
func CursorAgentEvents(payload []byte) []StreamEvent {
	fields, err := pbDecode(payload)
	if err != nil {
		return []StreamEvent{cursorUpstreamErr(err)}
	}
	var out []StreamEvent
	if f, ok := pbGet(fields, 1); ok { // interaction_update
		if uf, uerr := pbDecode(f.Value); uerr == nil {
			if t, ok := pbGet(uf, 1); ok { // text message
				if tf, terr := pbDecode(t.Value); terr == nil {
					if delta := pbFirst(tf, 1); delta != "" {
						out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartText, Text: delta})
					}
				}
			}
			if d, ok := pbGet(uf, 14); ok { // done + usage
				out = append(out, StreamEvent{Kind: EvStop, StopReason: "stop", Usage: readAgentUsage(d.Value)})
			}
		}
	}
	if f, ok := pbGet(fields, 2); ok { // exec_server_request
		if ef, eerr := pbDecode(f.Value); eerr == nil {
			if _, isCtx := pbGet(ef, 10); !isCtx {
				out = append(out, StreamEvent{Kind: EvError, Err: &types.APIError{
					Status:  502,
					Type:    "upstream_error",
					Message: "cursor AgentService requested an unsupported IDE tool",
				}})
			}
		}
	}
	return out
}

// readAgentUsage parses the done-frame usage {1: input, 2: output}.
func readAgentUsage(b []byte) *types.Usage {
	f, err := pbDecode(b)
	if err != nil {
		return nil
	}
	u := &types.Usage{}
	if v, ok := pbGet(f, 1); ok {
		u.InputTokens = int64(v.Num64)
	}
	if v, ok := pbGet(f, 2); ok {
		u.OutputTokens = int64(v.Num64)
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	return u
}

func cursorUpstreamErr(err error) StreamEvent {
	return StreamEvent{Kind: EvError, Err: &types.APIError{Status: 502, Type: "upstream_error", Message: err.Error()}}
}

// EncodeCursorAgentRequest renders the unified request as AgentService run
// bytes. The upstream carries exactly ONE user action per run, so the whole
// conversation is flattened into the user turn: system text first (folded —
// field 8 is forbidden, see header), then prior turns as "Assistant:" /
// "Tool <name>:" lines, the latest user text last. One-shot turn model:
// multi-turn context rides in the prompt, exactly like 9router's
// buildAgentRunFrame history encoding flattens it.
func EncodeCursorAgentRequest(u *types.ChatRequest) ([]byte, error) {
	if len(u.Messages) == 0 {
		return nil, fmt.Errorf("cursor: empty messages")
	}
	var sys, convo []string
	// System is hoisted out of Messages in the unified model; without this the
	// AgentService turn lost the whole system prompt (field 8 is forbidden, so
	// it must fold into the user text — live-verified rule, see file header).
	for _, p := range u.System {
		if p.Type == types.PartText && strings.TrimSpace(p.Text) != "" {
			sys = append(sys, p.Text)
		}
	}
	for i := range u.Messages {
		m := &u.Messages[i]
		switch m.Role {
		case types.RoleSystem:
			if t := m.FlattenText(); t != "" {
				sys = append(sys, t)
			}
		case types.RoleAssistant:
			if t := m.FlattenText(); t != "" {
				convo = append(convo, "Assistant: "+t)
			}
		case types.RoleTool:
			name := m.Name
			if name == "" {
				name = m.ToolCallID
			}
			if name == "" {
				name = "result"
			}
			convo = append(convo, "Tool "+name+": "+m.FlattenText())
		default:
			if t := m.FlattenText(); t != "" {
				convo = append(convo, t)
			}
		}
	}
	userText := strings.Join(convo, "\n\n")
	if userText == "" {
		userText = "Continue."
	}
	if len(sys) > 0 {
		userText = "[System]\n" + strings.Join(sys, "\n\n") + "\n\n" + userText
	}
	return buildAgentRunFrame(userText, cursorRequestedModel(u.Model)), nil
}

// cursorRequestedModel maps a client model id onto the id Cursor's
// AgentService recognizes. Cursor has no "auto" lane: both reference
// implementations rewrite auto* → "default" before the wire, and live
// evidence agrees — "auto" ends the turn with zero content (the gateway
// surfaces it as 502 empty response) while "default" answers.
//
// A PROVIDER PREFIX is meaningless upstream and must not ride the wire. An id
// that reaches this builder still carrying one — "cursor/auto" from a direct
// route entry, i.e. a client asking for the string /v1/models advertised
// (2026-09-21) — made Cursor answer `400 AI Model Not Found`, which the gateway
// relayed verbatim. Only the last path segment is a model id.
//
// The auto-{cost,balance,intelligence} preference cannot ride: it needs
// Cursor's model_parameters sub-fields, which this builder does not emit, so
// the preference is dropped and the lane becomes Cursor's own default.
// Upgrade path: add the ModelDetails parameter field numbers and emit
// {id:"optimization", value:"cost"|"balance"|"intelligence"}.
func cursorRequestedModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	switch model {
	case "auto", "auto-cost", "auto-balance", "auto-intelligence":
		return "default"
	}
	return model
}

// ---------------------------------------------------------------------------
// ChatService request
// ---------------------------------------------------------------------------

const (
	cuRoleUser      = 1
	cuRoleAssistant = 2
	cuModeChat      = 1
	cuModeAgent     = 2
)

// ---------------------------------------------------------------------------
// ChatService tool-result encoding (9router cursorProtobuf.js constants,
// cross-checked against cursor-api's Rust proto: ConversationMessage.ToolResult
// rides field 18; the nested ClientSideToolV2Result + call echo link the
// result back to the model's own invocation so the agentic loop survives a
// follow-up request).
// ---------------------------------------------------------------------------

const (
	cuMsgToolResults   = 18 // ConversationMessage.ToolResult (repeated)
	trCallID           = 1  // ToolResult.tool_call_id
	trName             = 2  // ToolResult.tool_name
	trIndex            = 3  // ToolResult.tool_index
	trRawArgs          = 5  // ToolResult.raw_args (original call arguments)
	trResult           = 8  // ToolResult.result (ClientSideToolV2Result)
	trToolCall         = 11 // ToolResult.tool_call (ClientSideToolV2Call echo)
	clientSideToolV2   = 19 // CLIENT_SIDE_TOOL_V2.MCP
	cv2Tool            = 1  // ClientSideToolV2*.tool (varint clientSideToolV2)
	cv2MCPParams       = 27 // ClientSideToolV2Call.mcp_params
	cv2CallID          = 3  // ClientSideToolV2Call.call_id
	cv2Name            = 9  // ClientSideToolV2Call.name
	cv2RawArgs         = 10 // ClientSideToolV2Call.raw_args
	cv2ToolIndex       = 48 // ClientSideToolV2Call.tool_index
	cv2MCPResult       = 28 // ClientSideToolV2Result.mcp_result
	cv2ResultCallID    = 35 // ClientSideToolV2Result.call_id
	cv2ResultToolIndex = 49 // ClientSideToolV2Result.tool_index
	mcpResultSelected  = 1  // MCPResult.selected_tool
	mcpResultContent   = 2  // MCPResult.result
)

// encodeMcpResult renders MCPResult {1: selected_tool, 2: result}.
func encodeMcpResult(selectedTool, result string) []byte {
	var r []byte
	r = pbString(r, mcpResultSelected, selectedTool)
	r = pbString(r, mcpResultContent, result)
	return r
}

// encodeClientSideToolV2Result renders the result half of a tool turn:
// {1: tool=19(MCP), 28: MCPResult, 35: call_id, 49: tool_index}.
func encodeClientSideToolV2Result(callID, selectedTool, result string) []byte {
	var b []byte
	b = pbUvarint(b, cv2Tool, clientSideToolV2)
	b = pbBytes(b, cv2MCPResult, encodeMcpResult(selectedTool, result))
	b = pbString(b, cv2ResultCallID, callID)
	b = pbUvarint(b, cv2ResultToolIndex, 1)
	return b
}

// encodeMcpParamsForCall renders MCPParams.tools[0] {1: name, 3: raw_args,
// 4: server "custom"} — the same nested shape encodeMcpTool declares upstream,
// so the echoed call matches the original MCP tool blob.
func encodeMcpParamsForCall(name, rawArgs string) []byte {
	var t []byte
	t = pbString(t, 1, name)
	t = pbString(t, 3, rawArgs)
	t = pbString(t, 4, "custom")
	return pbBytes(nil, 1, t)
}

// encodeClientSideToolV2Call renders the call echo Cursor uses to correlate a
// result with its invocation: {1: tool=19, 27: MCPParams, 3: call_id, 9: name,
// 10: raw_args, 48: tool_index}.
func encodeClientSideToolV2Call(callID, name, rawArgs string) []byte {
	var b []byte
	b = pbUvarint(b, cv2Tool, clientSideToolV2)
	b = pbBytes(b, cv2MCPParams, encodeMcpParamsForCall(name, rawArgs))
	b = pbString(b, cv2CallID, callID)
	b = pbString(b, cv2Name, name)
	b = pbString(b, cv2RawArgs, rawArgs)
	b = pbUvarint(b, cv2ToolIndex, 1)
	return b
}

// encodeToolResult renders one ConversationMessage.ToolResult: the call id +
// name echoed plainly (unified names are already bare — no mcp_custom_ prefix
// gymnastics needed), raw_args defaults to "{}" because a follow-up tool
// message does not carry the original call's arguments, and the result body
// rides field 8/11 so Cursor can continue the exact agentic turn.
func encodeToolResult(name, callID, rawArgs, result string) []byte {
	if callID == "" {
		callID = name
	}
	if name == "" {
		name = callID
	}
	if rawArgs == "" || rawArgs == "null" {
		rawArgs = "{}"
	}
	var b []byte
	b = pbString(b, trCallID, callID)
	b = pbString(b, trName, name)
	b = pbUvarint(b, trIndex, 1)
	b = pbString(b, trRawArgs, rawArgs)
	b = pbBytes(b, trResult, encodeClientSideToolV2Result(callID, name, result))
	b = pbBytes(b, trToolCall, encodeClientSideToolV2Call(callID, name, rawArgs))
	return b
}

// cursorToolDirective builds the tool-commit block prepended to the first
// user turn whenever the request declares tools: OmniRoute's
// TOOL_COMMIT_DIRECTIVE verbatim (without it, composer-2.5 narrates intent
// and ends ~20% of tool turns without invoking — live A/B 56%→69% with it),
// plus tool_choice and output-constraint lines Cursor's request proto has no
// native field for (response_format / max_tokens / stop surface as prompt
// rules — directToolChoiceHint / appendChatOptions port).
func cursorToolDirective(u *types.ChatRequest) string {
	var b strings.Builder
	b.WriteString("You are serving an OpenAI-compatible API request and the client has provided executable tools.\n")
	b.WriteString("When a tool is needed to answer (real-time data, web/search lookups, file or project operations), you MUST issue the actual tool call. Do NOT describe what you are about to do as prose and then stop — call the tool.\n")
	b.WriteString("Answer directly only when no tool is needed.\n")
	b.WriteString("Do not emit duplicate tool calls: call each operation once, then continue after the tool result is returned.\n")
	b.WriteString("Never claim that tools are unavailable.")
	// Composer-only: the IDE composer family does not return protobuf tool
	// calls — it writes the invocation into visible text using marker blocks.
	// Without this extra line, ~31% of turns end as narration ("I will call
	// the tool…") without actually emitting a block. Live A/B: this nudges
	// the rate from 69% → ~80%.
	if cursorComposerModel(u.Model) {
		b.WriteString("\nWhen calling a tool, emit the invocation block directly. Do NOT describe the call in prose — just write the tool name and arguments in the invocation format.")
	}
	switch tc := u.ToolChoice.(type) {
	case types.ToolChoiceTool:
		if tc.Name != "" {
			b.WriteString("\n\nYou MUST call the `" + tc.Name + "` tool now and not any other tool.")
		}
	case types.ToolChoiceMode:
		if tc == types.ToolChoiceAny {
			b.WriteString("\n\nYou MUST call at least one of the available tools now; do not answer without calling a tool.")
		}
	}
	if cons := cursorOutputConstraints(u); cons != "" {
		b.WriteString("\n\nOUTPUT CONSTRAINTS:\n" + cons)
	}
	return b.String()
}

// cursorOutputConstraints renders max_tokens / stop / response_format as
// prompt instructions (OmniRoute buildCursorOutputConstraints port).
func cursorOutputConstraints(u *types.ChatRequest) string {
	var cs []string
	if u.MaxTokens > 0 {
		cs = append(cs, fmt.Sprintf("Keep the answer within about %d output tokens.", u.MaxTokens))
	}
	if len(u.StopSequences) == 1 {
		cs = append(cs, "Do not include any text at or after this stop sequence: "+u.StopSequences[0])
	} else if len(u.StopSequences) > 1 {
		cs = append(cs, "Stop before any of these sequences: "+strings.Join(u.StopSequences, ", "))
	}
	if u.ResponseFormat != nil {
		switch u.ResponseFormat.Type {
		case "json_object":
			cs = append(cs, "Return a single valid JSON object and no surrounding prose or code fences.")
		case "json_schema":
			if u.ResponseFormat.Schema != nil {
				cs = append(cs, "Return only valid JSON (no prose or code fences) matching this schema: "+string(u.ResponseFormat.Schema))
			}
		}
	}
	return strings.Join(cs, "\n")
}

// cursorThinkingLevel maps reasoning_effort onto the ChatService enum
// (UNSPECIFIED/MEDIUM/HIGH — 9router THINKING_LEVEL).
func cursorThinkingLevel(effort string) uint64 {
	switch effort {
	case "medium":
		return 1
	case "high", "xhigh", "max":
		return 2
	default:
		return 0
	}
}

// EncodeCursorChatRequest renders the unified request as ChatService bytes:
// StreamUnifiedChatRequestWithTools {1: StreamUnifiedChatRequest}. Message
// roles map user→USER, everything else→ASSISTANT (9router encodeRequest
// wire behavior, verified upstream-tolerant). Tool defs ride as MCP blobs
// (field 34); tool RESULTS from prior turns ride as ConversationMessage
// ToolResult blobs (field 18, 9router encodeToolResult shape) so the agentic
// loop survives a follow-up request — without them Cursor sees the result as
// unattached prose and repeats or abandons the call. System is hoisted out of
// Messages in the unified model, so it is folded into the first plain user
// turn (ChatService has no system slot), alongside the tool-commit directive
// when tools are declared.
func EncodeCursorChatRequest(u *types.ChatRequest) ([]byte, error) {
	if len(u.Messages) == 0 {
		return nil, fmt.Errorf("cursor: empty messages")
	}
	hasTools := len(u.Tools) > 0
	mode := uint64(cuModeChat)
	if hasTools {
		mode = cuModeAgent
	}

	// textOf flattens only PartText parts: PartToolResult text rides field 18,
	// never the content field (duplicating it muddies the model's context).
	textOf := func(m *types.Message) string {
		seen := false
		var b strings.Builder
		for _, p := range m.Content {
			if p.Type != types.PartText {
				continue
			}
			if seen {
				b.WriteByte('\n')
			}
			seen = true
			b.WriteString(p.Text)
		}
		return b.String()
	}

	// Fold hoisted system + the tool-commit directive into the FIRST plain
	// user turn (OmniRoute TOOL_COMMIT_DIRECTIVE, live A/B 56%→69% tool calls:
	// composer-2.5 otherwise narrates intent and ends ~20% of turns without
	// invoking a declared tool).
	prefix := ""
	if len(u.System) > 0 {
		var sys []string
		for _, p := range u.System {
			if p.Type == types.PartText && strings.TrimSpace(p.Text) != "" {
				sys = append(sys, p.Text)
			}
		}
		prefix = strings.Join(sys, "\n\n")
	}
	if hasTools {
		if d := cursorToolDirective(u); d != "" {
			if prefix != "" {
				prefix += "\n\n"
			}
			prefix += d
		}
	}
	firstUser := -1
	if prefix != "" {
		for i := range u.Messages {
			if u.Messages[i].Role == types.RoleUser && textOf(&u.Messages[i]) != "" {
				firstUser = i
				break
			}
		}
	}

	var req []byte
	type msgID struct {
		id   string
		role uint64
	}
	ids := make([]msgID, 0, len(u.Messages))
	for i := range u.Messages {
		m := &u.Messages[i]
		role := uint64(cuRoleUser)
		if m.Role == types.RoleAssistant {
			role = cuRoleAssistant
		}
		content := textOf(m)
		if i == firstUser {
			content = prefix + "\n\n" + content
		}
		var msg []byte
		msg = pbString(msg, 1, content) // content
		msg = pbUvarint(msg, 2, role)   // role
		id := cursorUUID()
		msg = pbString(msg, 13, id) // message id
		msg = pbUvarint(msg, 29, boolUint(hasTools))
		msg = pbUvarint(msg, 47, mode)
		for _, p := range m.Content {
			if p.Type != types.PartToolResult {
				continue
			}
			name := p.Name
			if name == "" {
				name = m.Name
			}
			callID := p.ToolUseID
			if callID == "" {
				callID = m.ToolCallID
			}
			msg = pbBytes(msg, cuMsgToolResults, encodeToolResult(name, callID, string(p.Args), p.Text))
		}
		req = pbBytes(req, 1, msg)
		ids = append(ids, msgID{id: id, role: role})
	}
	// Static scaffold (9router UNKNOWN_* constants, byte-compatible).
	req = pbUvarint(req, 2, 1)
	req = pbBytes(req, 3, nil) // empty instruction
	req = pbUvarint(req, 4, 1)
	req = pbBytes(req, 5, append(pbString(nil, 1, cursorRequestedModel(u.Model)), pbBytes(nil, 4, nil)...)) // Model{name, empty}
	req = pbBytes(req, 8, nil)                                                                              // web tool
	req = pbUvarint(req, 13, 1)
	req = pbBytes(req, 15, cursorSetting())
	req = pbUvarint(req, 19, 1)
	req = pbString(req, 23, cursorUUID()) // conversation_id
	req = pbBytes(req, 26, cursorMetadata())
	req = pbUvarint(req, 27, boolUint(hasTools)) // is_agentic
	if hasTools {
		req = pbBytes(req, 29, pbAppendVarint(nil, 1)) // supported_tools [1]
		for _, t := range u.Tools {
			req = pbBytes(req, 34, encodeMcpTool(t.Name, t.Description, string(t.Schema)))
		}
	}
	for _, mid := range ids {
		req = pbBytes(req, 30, append(pbString(nil, 1, mid.id), pbUvarint(nil, 3, mid.role)...))
	}
	req = pbUvarint(req, 35, 0) // large_context
	req = pbUvarint(req, 38, 0)
	req = pbUvarint(req, 46, mode) // unified_mode
	req = pbBytes(req, 47, nil)
	req = pbUvarint(req, 48, boolUint(!hasTools)) // should_disable_tools
	req = pbUvarint(req, 49, cursorThinkingLevel(u.ReasoningEffort))
	req = pbUvarint(req, 51, 0)
	req = pbUvarint(req, 53, 1)
	if hasTools {
		req = pbString(req, 54, "Agent")
	} else {
		req = pbString(req, 54, "Ask")
	}
	return wrapConnectFrame(pbBytes(nil, 1, req)), nil
}

// cursorSetting emits the fixed CursorSetting blob (9router
// encodeCursorSetting): settings path + empty unknowns + two varints.
func cursorSetting() []byte {
	s6 := append(pbBytes(nil, 1, nil), pbBytes(nil, 2, nil)...)
	s := pbString(nil, 1, "cursor\\aisettings")
	s = pbBytes(s, 3, nil)
	s = pbBytes(s, 6, s6)
	s = pbUvarint(s, 8, 1)
	s = pbUvarint(s, 9, 1)
	return s
}

// cursorMetadata emits the Metadata blob: platform/arch/version/cwd/time.
// Static darwin/arm64 values (the gateway's own identity; upstream does not
// validate them against the account).
func cursorMetadata() []byte {
	m := pbString(nil, 1, "darwin")
	m = pbString(m, 2, "arm64")
	m = pbString(m, 3, "v22.0.0")
	m = pbString(m, 4, "/")
	m = pbString(m, 5, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	return m
}

// encodeMcpTool declares one tool (MCPTool: name, description, JSON-schema
// params, server "custom") — 9router encodeMcpTool shape.
func encodeMcpTool(name, desc, schema string) []byte {
	var t []byte
	if name != "" {
		t = pbString(t, 1, name)
	}
	if desc != "" {
		t = pbString(t, 2, desc)
	}
	if schema == "" || schema == "null" {
		schema = `{"type":"object"}`
	}
	t = pbString(t, 3, schema)
	t = pbString(t, 4, "custom")
	return t
}

// ---------------------------------------------------------------------------
// ChatService response decoding
// ---------------------------------------------------------------------------

// cursorToolCall is one decoded ClientSideToolV2Call.
type cursorToolCall struct {
	ID   string
	Name string
	Args string
}

// decodeToolCall extracts id/name/args from a ClientSideToolV2Call blob. The
// authoritative name/args live inside MCPParams (27) → tools list (1) → tool
// {1: name, 3: raw_args}; top-level name (9) / raw_args (10) are fallback.
// tool_call_id (3) can be multi-line; the first line is the id.
func decodeToolCall(b []byte) (*cursorToolCall, error) {
	f, err := pbDecode(b)
	if err != nil {
		return nil, err
	}
	tc := &cursorToolCall{}
	if v, ok := pbGet(f, 3); ok {
		tc.ID = strings.SplitN(string(v.Value), "\n", 2)[0]
	}
	if v, ok := pbGet(f, 27); ok { // MCPParams
		if mp, merr := pbDecode(v.Value); merr == nil {
			if t, ok := pbGet(mp, 1); ok { // tools[0]
				if tool, terr := pbDecode(t.Value); terr == nil {
					tc.Name = pbFirst(tool, 1)
					tc.Args = pbFirst(tool, 3)
				}
			}
		}
	}
	if tc.Name == "" {
		tc.Name = pbFirst(f, 9)
	}
	if tc.Args == "" {
		tc.Args = pbFirst(f, 10)
	}
	if tc.ID == "" || tc.Name == "" {
		return nil, nil // incomplete fragment; skip
	}
	return tc, nil
}

// CursorChatState carries per-stream decoder state: tool fragment ids map to
// unified part indexes so arg fragments land on the right tool call. For the
// IDE composer family, field 25 carries thinking AND the visible answer (with
// any inline tool-call markers) in one stream — split on the last </think>;
// thinkBuf/emitted track that split so only new visible suffix bytes become
// PartText and tool-call deltas.
type CursorChatState struct {
	started  bool
	toolIdx  map[string]int
	next     int
	thinkBuf string // composer field-25 accumulator
	emitted  int    // residual-text bytes already emitted as content
	// toolsSent is how many parsed inline calls have already been emitted; the
	// remainder rides the next frame (Cursor appends a block at a time).
	toolsSent int
	// finish is the finish_reason the synthesizer ends the turn with:
	// "tool_calls" once a tool call was emitted, else "stop".
	finish string
}

// cursorComposerModel reports the IDE composer family (composer-2.5,
// composer-2, provider-prefixed forms). Those models pack the visible answer
// into ChatService field 25 after a </think> tag — not field 1 text.
func cursorComposerModel(model string) bool {
	id := model
	if i := strings.LastIndex(model, "/"); i >= 0 {
		id = model[i+1:]
	}
	return strings.HasPrefix(strings.ToLower(id), "composer")
}

// visibleComposerContent returns the answer suffix after the last </think>
// in a composer field-25 blob. Empty until the end tag appears (9router
// visibleComposerContentFromThinking).
func visibleComposerContent(thinking string) string {
	const endTag = "</think>"
	i := strings.LastIndex(thinking, endTag)
	if i < 0 {
		return ""
	}
	return stripComposerFinal(strings.TrimLeft(thinking[i+len(endTag):], " \t\r\n"))
}

// CursorChatEvents decodes one ChatService StreamUnifiedChatResponseWithTools
// payload: tool calls (field 1) and text/thinking (field 2.1 / 2.25.1).
// Composer models: field 25 is split so only post-</think> text becomes
// PartText; the thinking prefix is dropped (no signature for Anthropic
// thinking blocks). Non-composer models keep field 25 as PartThinking.
func CursorChatEvents(payload []byte, model string, st *CursorChatState) []StreamEvent {
	fields, err := pbDecode(payload)
	if err != nil {
		return []StreamEvent{cursorUpstreamErr(err)}
	}
	var out []StreamEvent
	start := func() {
		if !st.started {
			st.started = true
			out = append(out, StreamEvent{Kind: EvStart, Model: model})
		}
	}
	if f, ok := pbGet(fields, 1); ok { // ClientSideToolV2Call
		if tc, terr := decodeToolCall(f.Value); terr == nil && tc != nil {
			start()
			if idx, seen := st.toolIdx[tc.ID]; seen {
				if tc.Args != "" {
					out = append(out, StreamEvent{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: tc.Args})
				}
			} else {
				idx := st.next
				st.next++
				if st.toolIdx == nil {
					st.toolIdx = map[string]int{}
				}
				st.toolIdx[tc.ID] = idx
				out = append(out, StreamEvent{Kind: EvPartStart, Index: idx, PartType: types.PartToolUse, ToolID: tc.ID, ToolName: tc.Name})
				if tc.Args != "" {
					out = append(out, StreamEvent{Kind: EvDelta, Index: idx, PartType: types.PartToolUse, ToolArgs: tc.Args})
				}
			}
		}
	}
	if f, ok := pbGet(fields, 2); ok { // StreamUnifiedChatResponse
		if rf, rerr := pbDecode(f.Value); rerr == nil {
			if t, ok := pbGet(rf, 1); ok && len(t.Value) > 0 {
				start()
				out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartText, Text: string(t.Value)})
			}
			if t, ok := pbGet(rf, 25); ok { // thinking {1: text}
				if tf, terr := pbDecode(t.Value); terr == nil {
					if d, ok := pbGet(tf, 1); ok && len(d.Value) > 0 {
						chunk := string(d.Value)
						if cursorComposerModel(model) {
							// Composer packs thinking + answer in field 25.
							// Emit only the new visible suffix after </think>
							// as content; never leak the thinking prefix.
							st.thinkBuf += chunk
							vis := visibleComposerContent(st.thinkBuf)
							out = append(out, st.composerVisibleEvents(vis, start)...)
						} else {
							start()
							out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartThinking, Thinking: chunk})
						}
					}
				}
			}
		}
	}
	return out
}

// composerVisibleEvents turns composer visible text into events: the residual
// answer text plus, once an invocation block closes, one EvPartStart + EvDelta
// per parsed tool call. An agent CLI that passes tool schemas then gets a real
// tool-call turn like any other model; without this the markers leak as content
// and the call is lost. Args are complete on arrival (Cursor writes them whole,
// no fragment merging), so each call is an atomic start+args pair rather than
// the ChatService fragment dance.
//
// Text is emitted by BYTE OFFSET into the residual, which only ever grows while
// the open block does not (a new block appends its residual after the previous
// one) — so emitted stays a valid cursor for the whole turn.
func (st *CursorChatState) composerVisibleEvents(vis string, start func()) []StreamEvent {
	var out []StreamEvent
	residual, calls, _ := composerScan(vis)
	safe := residual[:composerPartialMarkerCut(residual)]
	if len(safe) > st.emitted {
		start()
		out = append(out, StreamEvent{Kind: EvDelta, PartType: types.PartText, Text: safe[st.emitted:]})
		st.emitted = len(safe)
	}
	if len(calls) == 0 {
		// No CLOSED block yet. An open block (open >= 0) is mid-arrival and
		// never parsed; a frame with no marker at all has nothing to call. The
		// text holdback above covered the invocation bytes either way.
		return out
	}
	st.finish = "tool_calls"
	start()
	for _, c := range calls[st.toolsSent:] {
		idx := st.next
		st.next++
		out = append(out, StreamEvent{Kind: EvPartStart, Index: idx,
			PartType: types.PartToolUse, ToolID: composerToolCallID(idx), ToolName: c.Name})
		out = append(out, StreamEvent{Kind: EvDelta, Index: idx,
			PartType: types.PartToolUse, ToolArgs: c.Args})
	}
	st.toolsSent = len(calls)
	return out
}

// ---------------------------------------------------------------------------
// Error frames (in-200 JSON errors)
// ---------------------------------------------------------------------------

// CursorJSONError decodes a JSON error frame delivered INSIDE a 200 stream
// (payload starting with '{'). resource_exhausted maps to 429; everything
// else surfaces as 400 api_error (9router createErrorResponse semantics).
func CursorJSONError(payload []byte) (*types.APIError, bool) {
	if len(payload) == 0 || payload[0] != '{' {
		return nil, false
	}
	var jerr struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Debug struct {
					Error   string `json:"error"`
					Details struct {
						Title  string `json:"title"`
						Detail string `json:"detail"`
					} `json:"details"`
				} `json:"debug"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &jerr); err != nil {
		return nil, false
	}
	msg := jerr.Error.Message
	for _, d := range jerr.Error.Details {
		if d.Debug.Details.Title != "" {
			msg = d.Debug.Details.Title
			break
		}
		if d.Debug.Details.Detail != "" {
			msg = d.Debug.Details.Detail
			break
		}
	}
	if msg == "" {
		return nil, false
	}
	if jerr.Error.Code == "resource_exhausted" {
		return &types.APIError{Status: 429, Type: "rate_limit_error", Code: "rate_limited", Message: msg}, true
	}
	if jerr.Error.Code == "unauthenticated" || jerr.Error.Code == "permission_denied" {
		// An expired/rotated session token. 401 (not 400) so the pool benches
		// the account and a combo falls through to a live sibling instead of
		// reporting a caller-side error that is not the caller's fault.
		return &types.APIError{Status: 401, Type: "authentication_error", Message: msg}, true
	}
	return &types.APIError{Status: 400, Type: "api_error", Message: msg}, true
}

// DecodeCursorError normalizes a non-200 Cursor upstream body (Connect-RPC
// JSON error shape {"error":{code,message,details}}) into an APIError.
func DecodeCursorError(body []byte, status int) *types.APIError {
	if ae, ok := CursorJSONError(body); ok {
		if ae.Status == 400 && status >= 400 {
			ae.Status = status
		}
		return ae
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("cursor upstream error (status %d)", status)
	}
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return &types.APIError{Status: 502, Type: "upstream_error", Message: msg}
}

// composerToolCallID synthesizes the OpenAI-shaped id for an inline composer
// call — Cursor's text dialect carries no id of its own.
func composerToolCallID(idx int) string { return fmt.Sprintf("call_cursor_%d", idx) }

// ---------------------------------------------------------------------------
// Server stream hooks (unified plumbing)
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// SSE synthesis (provider-side consumption)
// ---------------------------------------------------------------------------

// ReadCursorFrames exposes the Connect-RPC frame reader for the provider
// transport: yields uncompressed payload frames, gunzipping where flagged,
// skipping trailers.
func ReadCursorFrames(r io.Reader, yield func(payload []byte) error) error {
	return readConnectFrames(r, yield)
}

// CursorFrame re-frames a decoded payload as an uncompressed Connect-RPC
// frame (the provider's duplex pump forwards decoded payloads this way).
func CursorFrame(payload []byte) []byte { return wrapConnectFrame(payload) }

// cursorSSE emits one OpenAI chat.completion.chunk line.
func cursorSSE(sb *strings.Builder, id string, created int64, model string, delta map[string]any, finish string, usage map[string]any) {
	c := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		c["usage"] = usage
	}
	if raw, err := json.Marshal(c); err == nil {
		sb.WriteString("data: ")
		sb.Write(raw)
		sb.WriteString("\n\n")
	}
}

// cursorUsageJSON renders the usage block (estimated flag mirrors the
// server's own estimation for ChatService streams; AgentService usage is
// upstream-reported).
func cursorUsageJSON(in, out int64, estimated bool) map[string]any {
	u := map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
	if estimated {
		u["estimated"] = true
	}
	return u
}

// CursorSSEStream converts a Connect-RPC frame stream into a synthetic
// OpenAI chat.completion.chunk SSE stream. agent selects the AgentService
// decoder (text deltas + usage on the done frame); otherwise ChatService
// (tool calls, text, thinking; usage estimated downstream). The stream ends
// with a finish chunk + [DONE]. Mid-stream failures surface as a chunkless
// reader error — nothing half-written reaches the client because the
// provider wraps this in a synthetic 200 whose body only starts after the
// first decoded event.
//
// cancel (may be nil) is invoked the moment the agent turn completes: the
// upstream NEVER half-closes its response — after the done frame it emits
// agent_error keepalives every 10s indefinitely (live-verified) — so the
// gateway must tear the stream down itself; waiting for EOF hangs the
// client response open forever.
func CursorSSEStream(frames io.Reader, model string, agent bool, cancel func()) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		var sb strings.Builder
		id := "chatcmpl-cursor-" + cursorUUID()
		created := time.Now().Unix()
		var chatSt CursorChatState
		started := false
		var usage *types.Usage
		start := func() {
			if !started {
				started = true
				cursorSSE(&sb, id, created, model, map[string]any{"role": "assistant", "content": ""}, "", nil)
			}
		}
		// flush writes the current sb buffer to the pipe and resets it,
		// so the client sees each chunk as it is emitted instead of
		// buffering until stream end. Cursor's ChatService keeps the
		// connection open with 10s keepalive frames, so without this the
		// client receives nothing until a timeout.
		flush := func() {
			if sb.Len() > 0 {
				_, _ = io.Copy(pw, strings.NewReader(sb.String()))
				sb.Reset()
			}
		}
		ferr := ReadCursorFrames(frames, func(payload []byte) error {
			if ae, ok := CursorJSONError(payload); ok {
				return ae
			}
			var events []StreamEvent
			if agent {
				events = CursorAgentEvents(payload)
			} else {
				events = CursorChatEvents(payload, model, &chatSt)
			}
			for _, e := range events {
				switch e.Kind {
				case EvStart:
					// CursorChatEvents emits EvStart once per stream (the
					// first frame that carries text or a tool call). It is
					// always followed in the same frame by the EvDelta or
					// EvPartStart that triggered it, so this case is a
					// redundant guard rather than a fix for a starved
					// stream: it keeps the role chunk written even if a
					// future decoder emits EvStart on its own.
					start()
					flush()
				case EvDelta:
					start()
					switch e.PartType {
					case types.PartThinking:
						cursorSSE(&sb, id, created, model, map[string]any{"reasoning_content": e.Thinking}, "", nil)
					case types.PartToolUse:
						cursorSSE(&sb, id, created, model, map[string]any{
							"tool_calls": []any{map[string]any{"index": e.Index, "function": map[string]any{"arguments": e.ToolArgs}}},
						}, "", nil)
					default:
						cursorSSE(&sb, id, created, model, map[string]any{"content": e.Text}, "", nil)
					}
					// Every part type flushes, not just text: a thinking-only
					// or tool-args-only turn would otherwise sit in sb until
					// stream end, which is the stall this stream exists to
					// avoid.
					flush()
					if !agent && e.PartType == types.PartToolUse {
						// A ChatService turn that carried tool calls must end
						// with finish_reason "tool_calls" (the client's agentic
						// loop keys on it). Cursor raises no Stop event on that
						// path — the stream is terminated by the caller — so
						// remember it here, alongside any inline composer call.
						chatSt.finish = "tool_calls"
					}
				case EvPartStart:
					start()
					name := e.ToolName
					if name == "" {
						// decodeToolCall can return an empty Name when the
						// upstream frame carries no MCPParams and no top-level
						// name (field 9). An empty function.name is rejected
						// by strict OpenAI-format validators (xai), so fall back
						// to the same placeholder the Anthropic repair uses.
						name = unknownToolName
					}
					cursorSSE(&sb, id, created, model, map[string]any{
						"tool_calls": []any{map[string]any{
							"index": e.Index, "id": e.ToolID, "type": "function",
							"function": map[string]any{"name": name, "arguments": ""},
						}},
					}, "", nil)
					flush()
				case EvStop:
					if e.Usage != nil {
						usage = e.Usage
					}
					if agent {
						// Agent turn complete: cursor streams 10s keepalives
						// forever after this frame — stop reading, tear the
						// upstream down (cancel), finish the SSE.
						if usage != nil {
							cursorSSE(&sb, id, created, model, map[string]any{}, "stop", cursorUsageJSON(usage.InputTokens, usage.OutputTokens, false))
						} else {
							cursorSSE(&sb, id, created, model, map[string]any{}, "stop", nil)
						}
						sb.WriteString("data: [DONE]\n\n")
						if cancel != nil {
							cancel()
						}
						_, _ = io.Copy(pw, strings.NewReader(sb.String()))
						sb.Reset()
						pw.CloseWithError(nil)
						return io.EOF
					}
				case EvError:
					if e.Err != nil {
						return e.Err
					}
				}
			}
			return nil
		})
		// Cursor sometimes ends a turn with zero content frames (model ids
		// not usable on the chosen service — observed live). Surface as an
		// error so combos fall through instead of answering nothing.
		var streamErr error
		if ferr != nil {
			if ae, ok := ferr.(*types.APIError); ok {
				streamErr = ae
			} else {
				streamErr = &types.APIError{Status: 502, Type: "upstream_error", Message: "cursor: " + ferr.Error()}
			}
		} else if !started {
			streamErr = &types.APIError{Status: 502, Type: "upstream_error",
				Message: "cursor: empty response (model may be unavailable on this path)"}
		}
		if streamErr != nil {
			pw.CloseWithError(streamErr)
			return
		}
		finish := chatSt.finish
		if finish == "" {
			finish = "stop"
		}
		if usage != nil {
			cursorSSE(&sb, id, created, model, map[string]any{}, finish, cursorUsageJSON(usage.InputTokens, usage.OutputTokens, false))
		} else {
			cursorSSE(&sb, id, created, model, map[string]any{}, finish, nil)
		}
		sb.WriteString("data: [DONE]\n\n")
		_, _ = io.Copy(pw, strings.NewReader(sb.String()))
		pw.CloseWithError(nil)
	}()
	return pr
}
