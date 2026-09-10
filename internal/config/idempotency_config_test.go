package config

import (
	"strings"
	"testing"
	"time"
)

// A typo'd server.idempotency_ttl must fail the load loudly instead of
// silently replaying for the wrong window.
func TestValidateIdempotencyKnobs(t *testing.T) {
	cases := []struct {
		ttl    string
		cache  int
		errSub string // empty = must pass
	}{
		{"", 0, ""},        // default 5s, default 128
		{"5s", 0, ""},      // default capacity
		{"off", 128, ""},   // explicit off
		{"0", 1024, ""},    // hard max capacity
		{"false", 1, ""},   // disabled, minimum capacity
		{"500ms", 128, ""}, // sub-second windows are legal
		{"24hr", 128, "idempotency_ttl"},
		{"-5s", 128, "idempotency_ttl"},
		{"daily", 128, "idempotency_ttl"},
		{"5s", -1, "idempotency_cache"},
		{"5s", 1025, "idempotency_cache"},
	}
	for _, tc := range cases {
		c := &Config{Server: Server{IdempotencyTTL: tc.ttl, IdempotencyCache: tc.cache}}
		err := c.Validate()
		if tc.errSub == "" {
			if err != nil {
				t.Errorf("ttl %q cache %d: unexpected error %v", tc.ttl, tc.cache, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.errSub) {
			t.Errorf("ttl %q cache %d: want error containing %q, got %v", tc.ttl, tc.cache, tc.errSub, err)
		}
	}
}

// Defaults turn the zero config on with the 5s window; "off" and its
// aliases disable; the accessor never returns a non-positive window.
func TestIdempotencyTTLDur(t *testing.T) {
	if d := (&Config{}).IdempotencyTTLDur(); d != 5*time.Second {
		t.Fatalf("zero config: got %v, want default-on 5s", d)
	}
	for _, off := range []string{"0", "off", "false", "disabled"} {
		c := &Config{Server: Server{IdempotencyTTL: off}}
		if d := c.IdempotencyTTLDur(); d != 0 {
			t.Fatalf("ttl %q: got %v, want disabled", off, d)
		}
	}
	c := &Config{Server: Server{IdempotencyTTL: "30s"}}
	if d := c.IdempotencyTTLDur(); d != 30*time.Second {
		t.Fatalf("ttl 30s: got %v", d)
	}
	c = &Config{}
	c.Defaults()
	if c.Server.IdempotencyTTL != "5s" {
		t.Fatalf("Defaults must fill the 5s window, got %q", c.Server.IdempotencyTTL)
	}
}
