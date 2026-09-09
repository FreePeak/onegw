package translat

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestCursorAgentReplyHex(t *testing.T) {
	// Live-proven JS reply: 0000000008 120652040a020a00
	want := "0000000008120652040a020a00"
	got := hex.EncodeToString(CursorAgentReply)
	if got != want {
		t.Fatalf("reply hex: got %s want %s", got, want)
	}
	if !bytes.Contains(CursorAgentReply, []byte{0x52, 0x04}) {
		t.Fatal("field-10 wrapper missing")
	}
}
