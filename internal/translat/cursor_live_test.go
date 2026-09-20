package translat

// Live Cursor protocol probe — gated by ONEGW_LIVE_CURSOR=1 (never runs in
// CI; burns real upstream quota). Dumps the raw duplex exchange against the
// real AgentService to debug transport-level divergence from the JS probes.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"onegw/internal/types"
)

func TestLiveCursorAgentDuplex(t *testing.T) {
	if os.Getenv("ONEGW_LIVE_CURSOR") != "1" {
		t.Skip("live probe: set ONEGW_LIVE_CURSOR=1")
	}
	token := os.Getenv("CURSOR_TOKEN")
	machineID := os.Getenv("CURSOR_MACHINE_ID")
	if token == "" || machineID == "" {
		t.Skip("live probe: set CURSOR_TOKEN + CURSOR_MACHINE_ID")
	}

	u := &types.ChatRequest{
		Model: "gpt-5.2",
		Messages: []types.Message{
			{Role: types.RoleUser, Content: []types.Part{{Type: types.PartText, Text: "Reply with exactly: PONG"}}},
		},
	}
	reqBody, err := EncodeCursorAgentRequest(u)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("run frame (%d bytes): %s", len(reqBody), hex.EncodeToString(reqBody[:min(64, len(reqBody))]))

	pr, pw := io.Pipe()
	replies := make(chan []byte)
	go func() {
		defer pw.Close()
		if _, err := pw.Write(reqBody); err != nil {
			return
		}
		for f := range replies {
			t.Logf(">>> writing handshake reply (%d bytes)", len(f))
			if _, err := pw.Write(f); err != nil {
				return
			}
		}
	}()

	req, err := http.NewRequest("POST", CursorAgentEndpointHost+CursorAgentRunPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	for _, h := range CursorHeaders(token, machineID) {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	cl := &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true}}
	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("do: %v (after %s)", err, time.Since(start))
	}
	defer resp.Body.Close()
	t.Logf("status %d after %s", resp.StatusCode, time.Since(start))

	var dump bytes.Buffer
	var streamErr error
	func() {
		defer resp.Body.Close()
		streamErr = ReadCursorFrames(resp.Body, func(payload []byte) error {
			if CursorAgentNeedsReply(payload) {
				t.Logf("<<< exec question (%d B) at %s: %x", len(payload), time.Since(start), payload[:min(40, len(payload))])
				replies <- CursorAgentReply
				return nil
			}
			events := CursorAgentEvents(payload)
			dump.WriteString(fmt.Sprintf("frame %dB events=%d\n", len(payload), len(events)))
			for _, e := range events {
				if e.Kind == EvDelta {
					dump.WriteString("  TEXT: " + e.Text + "\n")
				}
				if e.Kind == EvStop {
					dump.WriteString(fmt.Sprintf("  STOP usage=%+v\n", e.Usage))
					// Agent turn complete: cursor keeps the stream open with
					// 10s keepalives forever — stop reading (the production
					// CursorSSEStream does the same and cancels the request).
					return io.EOF
				}
			}
			return nil
		})
	}()
	close(replies)
	err = streamErr
	t.Logf("stream done after %s, err=%v\n%s", time.Since(start), err, dump.String())
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
