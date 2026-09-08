package translat

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"
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

// statusFromOAErr maps an in-band upstream error object onto an HTTP
// status. one-api proxies (b-ai family) answer HTTP 200 and deliver the
// real failure — often a 429 rate limit — as an error object mid-stream
// or in a 200 body; reporting those as bare 502 both lied on the
// dashboard and starved the account-cooldown ladder of its input.
// Recognized codes map to their status; anything unrecognized stays 502.
func statusFromOAErr(code any, typ, msg string) int {
	switch c := code.(type) {
	case float64:
		if c >= 400 && c < 600 {
			return int(c)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(c)); err == nil && n >= 400 && n < 600 {
			return n
		}
	}
	probe := strings.ToLower(errCodeString(code) + " " + typ + " " + msg)
	switch {
	case strings.Contains(probe, "rate limit"), strings.Contains(probe, "rate_limit"),
		strings.Contains(probe, "too many requests"), strings.Contains(probe, "overloaded"),
		strings.Contains(probe, "insufficient_quota"):
		return 429
	}
	return 502
}
