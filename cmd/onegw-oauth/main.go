// Command onegw-oauth is the standalone OAuth device-flow CLI (issue #2): it
// prints the verification URL + user code, polls until the operator approves,
// and stores the token in the gateway's data dir where the running onegw picks
// it up — no config edit, no restart.
//
// The gateway binary speaks the same commands as `onegw oauth …`; both call
// internal/oauthcmd, so the two entry points cannot drift. Inside a container
// only the gateway binary exists, which is why the subcommand is the form to
// document for docker (`docker exec -it onegw onegw oauth login …`).
package main

import (
	"os"

	"onegw/internal/oauthcmd"
)

func main() {
	os.Exit(oauthcmd.Run(os.Args[1:]))
}
