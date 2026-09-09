package provider

import (
	"testing"

	"onegw/internal/translat"
)

func TestNewKindsFormatAndDefaults(t *testing.T) {
	cases := []struct {
		kind   Kind
		format translat.Format
		base   string
		path   string
		forced bool
	}{
		{KindCommandCode, translat.FmtCommandCode, "https://api.commandcode.ai/alpha/generate", "", true},
		{KindOpenAIResponses, translat.FmtOpenAIResponses, "https://cli-chat-proxy.grok.com", "/v1/responses", true},
		{KindCursor, translat.FmtOpenAI, "https://api2.cursor.sh", "", true},
		{KindOpenAI, translat.FmtOpenAI, "https://api.openai.com", "/v1/chat/completions", false},
	}
	for _, c := range cases {
		if got := c.kind.Format(); got != c.format {
			t.Errorf("%s: Format()=%s want %s", c.kind, got, c.format)
		}
		if got := c.kind.DefaultBaseURL(); got != c.base {
			t.Errorf("%s: DefaultBaseURL()=%s want %s", c.kind, got, c.base)
		}
		d := &Def{Kind: c.kind}
		if got := d.Path("chat", ""); got != c.path {
			t.Errorf("%s: Path(chat)=%q want %q", c.kind, got, c.path)
		}
		if got := c.kind.ForcedStream(); got != c.forced {
			t.Errorf("%s: ForcedStream()=%v want %v", c.kind, got, c.forced)
		}
	}
}

// joinURL must produce the exact upstream endpoints for both base_url
// conventions (bare host and host with the version segment).
func TestNewKindEndpointJoining(t *testing.T) {
	cases := []struct {
		kind Kind
		base string
		want string
	}{
		{KindCommandCode, "https://api.commandcode.ai/alpha/generate", "https://api.commandcode.ai/alpha/generate"},
		{KindOpenAIResponses, "https://cli-chat-proxy.grok.com", "https://cli-chat-proxy.grok.com/v1/responses"},
		{KindOpenAIResponses, "https://cli-chat-proxy.grok.com/v1", "https://cli-chat-proxy.grok.com/v1/responses"},
	}
	for _, c := range cases {
		d := &Def{Kind: c.kind, BaseURL: c.base}
		got := joinURL(d.Base(nil), d.Path("chat", ""))
		if got != c.want {
			t.Errorf("%s base %s: got %s want %s", c.kind, c.base, got, c.want)
		}
	}
}

func TestNewRequestUUIDShape(t *testing.T) {
	a, b := newRequestUUID(), newRequestUUID()
	if a == b {
		t.Fatalf("uuids not unique: %s", a)
	}
	if len(a) != 36 || a[14] != '4' {
		t.Fatalf("not a v4 uuid: %q", a)
	}
}
