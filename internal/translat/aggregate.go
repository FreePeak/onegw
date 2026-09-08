package translat

import (
	"bufio"
	"io"

	"onegw/internal/types"
)

// wireReader picks the framing for a format: commandcode is NDJSON (bare
// lines), every other format is SSE.
func wireReader(f Format) func(*bufio.Reader, func(sseEvent) error) error {
	if f == FmtCommandCode {
		return readNDJSON
	}
	return readSSE
}

// AggregateStream decodes a stream in `from` format into a unified
// ChatResponse, for clients that requested a non-streaming completion from
// a stream-only upstream (e.g. CommandCode, which has no non-streaming
// mode). In-200 error events abort aggregation: the returned error is a
// *types.APIError carrying the synthesized status. The caller renders the
// response with Encode<Format>Response.
func AggregateStream(body io.Reader, from Format, model string) (*types.ChatResponse, error) {
	br := bufio.NewReaderSize(body, 32<<10)
	dec, decFinish := newStreamDecoder(from)
	var agg AggregateState
	err := wireReader(from)(br, func(ev sseEvent) error {
		events, derr := dec.decode(ev)
		if derr != nil {
			return derr
		}
		for _, e := range events {
			if e.Kind == EvError && e.Err != nil {
				return e.Err
			}
			if !agg.Aggregate(e) {
				return &types.APIError{
					Status:  502,
					Type:    "upstream_error",
					Message: "aggregated response exceeds gateway memory budget",
				}
			}
		}
		return nil
	})
	if err != nil && err != io.EOF {
		if decFinish != nil {
			decFinish()
		}
		return nil, err
	}
	out := agg.Result()
	out.Model = orDefault(out.Model, model)
	return out, nil
}
