package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"onegw/internal/config"
)

// jsonProbe holds every top-level field the request path peeks at. One
// decode now serves peekModel, peekStream and conversationFingerprint,
// which each used to walk the whole body separately.
//
// ponytail: kept deliberately narrow — no reflection-heavy reuse of the
// provider request structs. Those change shape per format; a fixed probe
// cannot drift with them the way a shared struct would.
type jsonProbe struct {
	Model    string      `json:"model"`
	Stream   bool        `json:"stream"`
	Messages []probeTurn `json:"messages"`
}

// probeTurn is one conversation turn as the fingerprint needs it. Content
// stays raw: decoding every turn into `any` allocates one map per turn, and
// a 126K-token conversation is ~14000 of them — the fingerprint reads one.
type probeTurn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// probeBody decodes the fields the request path needs. Returns ok=false
// when the body is not a JSON object the probe understands, in which case
// callers fall back to the zero values — the same answer the individual
// peeks gave when their own Unmarshal failed.
func probeBody(body []byte) (jsonProbe, bool) {
	var p jsonProbe
	if len(body) == 0 {
		return p, false
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, false
	}
	return p, true
}

// fingerprintTurns returns the first user turn's text. Scope is unchanged
// from the body-parsing version it replaces: "messages" only, no Gemini
// "contents", so sticky-account behaviour cannot drift with this refactor.
func (p *jsonProbe) fingerprintTurns() (string, bool) {
	for _, m := range p.Messages {
		if m.Role != "user" {
			continue
		}
		var content any
		if err := json.Unmarshal(m.Content, &content); err != nil {
			continue
		}
		if text := firstUserText(content); text != "" {
			return text, true
		}
	}
	return "", false
}

// identityFromBody is the secondary-path form of requestIdentity: it decodes
// the body once. The hot request path passes the probe it already decoded.
func identityFromBody(h http.Header, ak *config.AuthKey, body []byte) string {
	p, _ := probeBody(body)
	return requestIdentity(h, ak, p)
}

// peekModelFrom returns the model from an already-decoded probe.
func peekModelFrom(p jsonProbe) string { return p.Model }

// peekStreamFrom returns the stream flag from an already-decoded probe.
func peekStreamFrom(p jsonProbe) bool { return p.Stream }

// probeModelAndStream is the one-call form for the hot path: the two peeks
// that always run together on every request.
func probeModelAndStream(body []byte) (model string, stream bool, ok bool) {
	p, ok := probeBody(body)
	if !ok {
		return "", false, false
	}
	return peekModelFrom(p), peekStreamFrom(p), true
}

// hasStreamFlag reports the stream flag without decoding the rest of the
// body. The idempotency gate only needs the flag, and it runs on bodies
// that may be large; a targeted scan beats a full decode.
func hasStreamFlag(body []byte) bool {
	return jsonFieldIsTrue(body, "stream")
}

// jsonFieldIsTrue reports whether the top-level key name is bound to JSON
// true. It scans for the key at nesting depth 1 only, so a "stream": true
// buried inside a tool definition or a message cannot be mistaken for the
// top-level flag. Literals are matched exactly (no trailing garbage), and
// a miss is a false — the same answer a failed Unmarshal gave.
func jsonFieldIsTrue(body []byte, name string) bool {
	key := []byte(`"` + name + `"`)
	depth := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			end := skipJSONString(body, i)
			if end < 0 {
				return false
			}
			if depth != 1 || !bytes.Equal(body[i:end+1], key) {
				i = end
				continue
			}
			j := skipJSONSpace(body, end+1)
			if j < len(body) && body[j] == ':' {
				j = skipJSONSpace(body, j+1)
				if !bytes.HasPrefix(body[j:], []byte("true")) {
					return false
				}
				// The literal must END there: `truex` is malformed JSON, and
				// a failed Unmarshal used to read as false.
				k := j + len("true")
				return k == len(body) || isJSONSpace(body[k]) || strings.IndexByte(",}]", body[k]) >= 0
			}
			i = end
		}
	}
	return false
}

// skipJSONString returns the index of the closing quote of the string
// starting at body[i] == '"', or -1 when the body is truncated. Escapes
// are skipped so a \" inside the string does not end it early.
func skipJSONString(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return -1
}
