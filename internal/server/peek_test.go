package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// The probe replaces three whole-body json.Unmarshal calls with one. These
// cases pin the answers it must keep giving, including every miss the old
// per-peek Unmarshal treated as a zero value.
func TestProbeMatchesWholeBodyUnmarshal(t *testing.T) {
	bodies := map[string]string{
		"openai stream":    `{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hello there"}]}`,
		"anthropic":        `{"model":"claude-sonnet-5-5","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		"tool block turn":  `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url"},{"type":"text","text":"x"}]}]}`,
		"no messages":      `{"model":"m","stream":true}`,
		"stream nested":    `{"model":"m","tools":[{"stream":true}],"messages":[{"role":"user","content":"q"}]}`,
		"stream as string": `{"model":"m","stream":"true","messages":[]}`,
		"escaped key":      `{"model":"m","note":"a \"stream\": true","messages":[{"role":"user","content":"q"}]}`,
		"not json":         `not json at all`,
		"empty":            ``,
		"json array":       `[1,2,3]`,
		"null":             `null`,
		"unicode content":  `{"model":"m","messages":[{"role":"user","content":"héllo 🌍"}]}`,
		"blank first user": `{"model":"m","messages":[{"role":"user","content":"   "},{"role":"user","content":"real"}]}`,
		"assistant first":  `{"model":"m","messages":[{"role":"assistant","content":"a"},{"role":"user","content":"u"}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			// Reference: exactly what the removed code did.
			var refModel struct {
				Model string `json:"model"`
			}
			wantModel := ""
			if json.Unmarshal([]byte(body), &refModel) == nil {
				wantModel = refModel.Model
			}
			var refStream struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal([]byte(body), &refStream)
			wantStream := refStream.Stream

			// Fingerprint reference: the removed conversationFingerprint.
			wantFP := ""
			if len(body) > 0 {
				var refFP struct {
					Messages []struct {
						Role    string `json:"role"`
						Content any    `json:"content"`
					} `json:"messages"`
				}
				if json.Unmarshal([]byte(body), &refFP) == nil {
					for _, m := range refFP.Messages {
						if m.Role != "user" {
							continue
						}
						if txt := firstUserText(m.Content); txt != "" {
							wantFP = "c:" + hashPrefix(txt)
							break
						}
					}
				}
			}

			p, _ := probeBody([]byte(body))
			if got := peekModelFrom(p); got != wantModel {
				t.Errorf("model = %q, want %q", got, wantModel)
			}
			if got := peekStreamFrom(p); got != wantStream {
				t.Errorf("stream = %v, want %v", got, wantStream)
			}
			if got := fingerprintFrom(p); got != wantFP {
				t.Errorf("fingerprint = %q, want %q", got, wantFP)
			}
			// hasStreamFlag must agree with the decode on every case.
			if got := hasStreamFlag([]byte(body)); got != wantStream {
				t.Errorf("hasStreamFlag = %v, want %v", got, wantStream)
			}
		})
	}
}

// hasStreamFlag is a hand-rolled scanner replacing a whole-body Unmarshal in
// the idempotency gate. Nested keys and decoys must not fool it.
func TestHasStreamFlagDepthAndDecoys(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream": false}`, false},
		{`{"a":1,"stream":true,"b":2}`, true},
		{`{"a":1,"stream":false,"b":2}`, false},
		{`{"messages":[{"stream":true}]}`, false},    // depth 2
		{`{"tools":[{"x":{"stream":true}}]}`, false}, // depth 3
		{`{"note":"\"stream\": true"}`, false},       // inside a string
		{`{"note":"stream: true"}`, false},           // not a key
		{`{"streaming":true}`, false},                // prefix key
		{`{"x_stream":true}`, false},                 // different key
		{`{"stream":truex}`, false},                  // not the literal
		{`{"stream":null}`, false},
		{`{"stream":1}`, false},
		{`{"stream":`, false},                             // truncated
		{`{"a":"\\","stream":true}`, true},                // escaped backslash
		{`  {"stream"  :  true  }  `, true},               // whitespace
		{`{"deep":{"a":[{"b":{"stream":true}}]}}`, false}, // depth 4
	}
	for _, c := range cases {
		if got := hasStreamFlag([]byte(c.body)); got != c.want {
			t.Errorf("hasStreamFlag(%s) = %v, want %v", c.body, got, c.want)
		}
	}
}

// A malformed body must still produce the old answer, not a panic.
func TestHasStreamFlagMalformed(t *testing.T) {
	for _, body := range []string{`"`, `{"a":"unterminated`, `{`, ``, `\`} {
		_ = hasStreamFlag([]byte(body)) // must not panic
	}
}

// hashPrefix mirrors fingerprintFrom's own derivation, so the reference
// value is computed independently of the code under test.
func hashPrefix(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
