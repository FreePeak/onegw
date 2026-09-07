package translat

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"onegw/internal/types"
)

func nowUnix() int64 { return time.Now().Unix() }

// StreamEvent is one unified streaming event. Decoders convert upstream SSE
// into these; encoders render them into the client's wire format.
type StreamEvent struct {
	// Kind: "start", "part_start", "delta", "part_stop", "stop", "ping", "error".
	Kind string

	// start
	ID    string
	Model string

	// part_start / delta / part_stop
	Index     int    // upstream block index (encoder remaps)
	PartType  string // text | tool_use | thinking
	ToolID    string // part_start (tool_use)
	ToolName  string // part_start (tool_use)
	Text      string // delta (text)
	Thinking  string // delta (thinking)
	Signature string // delta (thinking signature)
	ToolArgs  string // delta (tool arguments, partial JSON)

	// stop
	StopReason string
	StopSeq    string
	Usage      *types.Usage

	Err *types.APIError
}

// Event kinds.
const (
	EvStart     = "start"
	EvPartStart = "part_start"
	EvDelta     = "delta"
	EvPartStop  = "part_stop"
	EvStop      = "stop"
	EvPing      = "ping"
	EvError     = "error"
)

// sseEvent is one parsed Server-Sent Event.
type sseEvent struct {
	Name string
	Data []byte
}

// readSSE reads events from an SSE stream. It never buffers more than one
// event. Returns io.EOF at clean stream end.
func readSSE(r *bufio.Reader, yield func(sseEvent) error) error {
	var name string
	var data bytes.Buffer
	flush := func() error {
		if data.Len() == 0 && name == "" {
			return nil
		}
		ev := sseEvent{Name: name, Data: append([]byte(nil), data.Bytes()...)}
		name = ""
		data.Reset()
		return yield(ev)
	}
	for {
		line, err := readLine(r)
		if len(line) > 0 || err == nil {
			switch {
			case line == "": // event boundary
				if ferr := flush(); ferr != nil {
					return ferr
				}
			case strings.HasPrefix(line, ":"): // comment/keepalive
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if err != nil {
			if err == io.EOF {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				return io.EOF
			}
			return err
		}
	}
}

// readLine reads one \n-terminated line without the trailing newline,
// returning io.EOF only when the stream ends with no partial line.
func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				if sb.Len() == 0 {
					return "", io.EOF
				}
				return sb.String(), io.EOF
			}
			return sb.String(), err
		}
		if chunk == '\n' {
			return strings.TrimSuffix(sb.String(), "\r"), nil
		}
		sb.WriteByte(chunk)
		if sb.Len() > maxLine {
			return sb.String(), fmt.Errorf("sse line exceeds %d bytes", maxLine)
		}
	}
}

const maxLine = 4 << 20 // 4 MiB guards against runaway events

// TranslateStream pipes an upstream stream in `from` format to a client
// stream in `to` format, flushing after every event. It returns the usage
// observed during the stream (best effort; zero if the upstream omitted it).
func TranslateStream(body io.Reader, w io.Writer, flush func(), from, to Format, model string) (types.Usage, error) {
	br := bufio.NewReaderSize(body, 32<<10)
	enc := newStreamEncoder(to, model)
	var usage types.Usage
	err := readSSE(br, func(ev sseEvent) error {
		events, derr := decodeStreamEvent(from, ev)
		if derr != nil {
			return derr
		}
		for _, e := range events {
			if e.Usage != nil {
				usage.Merge(*e.Usage)
			}
			if eerr := enc.encode(w, e); eerr != nil {
				return eerr
			}
		}
		if flush != nil {
			flush()
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return usage, err
	}
	if ferr := enc.finish(w); ferr != nil {
		return usage, ferr
	}
	if flush != nil {
		flush()
	}
	return usage, nil
}
