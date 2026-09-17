// The loopback callback listener's port choice: oauth.callback_port decides it,
// and a reload that moves it has to move the socket with it.

package server

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"onegw/internal/config"
)

// freePorts asks the kernel for n distinct ports nobody holds, then gives them
// back. All the listeners are open at once on purpose: released one at a time,
// the kernel happily hands the same port to the next call.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	lns := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
	return ports
}

// moveFixture is the oauth fixture with oauth.callback_port set and one extra
// account that owns its own login. callback_port has to live in the [oauth]
// table, which must precede the [[oauth.accounts]] array of tables, so it is
// spliced in front of that block; the extra account goes on the end, where it
// is still part of the same array.
func moveFixture(t *testing.T, base string, port int) string {
	t.Helper()
	head := strings.Replace(base, "\n[[oauth.accounts]]",
		"\n[oauth]\ncallback_port = "+strconv.Itoa(port)+"\n\n[[oauth.accounts]]", 1)
	if head == base {
		t.Fatal("fixture has no [[oauth.accounts]] anchor to insert [oauth] before")
	}
	return head + `
# A second account with its own login, so a test can start a fresh browser
# sign-in while xai/main is still pending on the old callback port.
[[oauth.accounts]]
provider = "xai"
account  = "spare"
service  = "xai"
`
}

// TestOAuthCallbackPortMovedByReload: the session reads oauth.callback_port on
// every login, so a reload that moves it must move the listener with it. The
// old socket cannot simply ride out its idle grace here — a login left pending
// on it is exactly what makes the plain idle release refuse — so this is the
// case that has to close it outright.
func TestOAuthCallbackPortMovedByReload(t *testing.T) {
	old := browserCallbackPort
	browserCallbackPort = callbackPortFromSession // let the config decide
	t.Cleanup(func() { browserCallbackPort = old })

	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	fixture := oauthFixture(idpURL, upstreamURL)
	ports := freePorts(t, 2)

	srv, h, path := newTestServerFromFile(t, moveFixture(t, fixture, ports[0]))
	if got := redirectHost(t, loginRedirect(t, h, "xai/main")); got != hostPort(ports[0]) {
		t.Fatalf("first login redirect host = %q, want %s", got, hostPort(ports[0]))
	}

	// A reload moves the port, exactly like a SIGHUP after an edit.
	if err := os.WriteFile(path, []byte(moveFixture(t, fixture, ports[1])), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load with callback_port %d: %v", ports[1], err)
	}
	srv.Reload(cfg)

	if got := redirectHost(t, loginRedirect(t, h, "xai/spare")); got != hostPort(ports[1]) {
		t.Fatalf("after the reload the redirect host = %q, want %s — the listener did not move",
			got, hostPort(ports[1]))
	}
	// And the listener it left behind is gone, not merely unreachable.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", hostPort(ports[0]), 200*time.Millisecond)
		if err != nil {
			break // released
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("the old loopback listener %s still accepts after the port moved", hostPort(ports[0]))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hostPort(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func loginRedirect(t *testing.T, h http.Handler, key string) string {
	t.Helper()
	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key="+key, "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", key, w.Code, w.Body.String())
	}
	return decodeJSON[browserPrompt](t, w.Body.String()).Prompt.VerifyURL
}

// redirectHost is the host:port of the loopback redirect the login advertises.
func redirectHost(t *testing.T, verifyURL string) string {
	t.Helper()
	cb, err := url.Parse(mustParseQuery(t, verifyURL).Get("redirect_uri"))
	if err != nil {
		t.Fatalf("redirect_uri in %q: %v", verifyURL, err)
	}
	return cb.Host
}
