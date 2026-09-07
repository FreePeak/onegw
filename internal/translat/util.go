package translat

import (
	"crypto/rand"
	"encoding/base64"
)

func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// randHex returns n hex chars from crypto/rand.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, n)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0xf])
	}
	return string(out[:n])
}
