package main

import "testing"

// The regression this pins: before the dispatch grew an unknown-command branch,
// any bare word that was not a subcommand fell through to runGateway — so
// `onegw oauth login …` (the form the container docs needed) and every typo
// started a SECOND gateway process, which SO_REUSEPORT binds on the same port
// as the live one. A flag must still start the gateway; a subcommand must run;
// anything else must be reported.
func TestClassifyArg(t *testing.T) {
	cases := map[string]argKind{
		"version":     argCommand,
		"update":      argCommand,
		"oauth":       argCommand,
		"help":        argCommand,
		"-h":          argCommand,
		"--help":      argCommand,
		"-config":     argFlag,
		"--config=x":  argFlag,
		"-listen":     argFlag,
		"login":       argUnknown,
		"connect":     argUnknown,
		"oauth-login": argUnknown,
		"logni":       argUnknown,
	}
	for arg, want := range cases {
		if got := classifyArg(arg); got != want {
			t.Errorf("classifyArg(%q) = %v, want %v", arg, got, want)
		}
	}
}
