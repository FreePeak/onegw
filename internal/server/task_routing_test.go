package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"onegw/internal/config"
)

// taskRoutingToml: two providers, one combo [cheap, strong], task routing
// switchable. Both models have declared tiers; only deep reasons.
const taskRoutingToml = `# task routing test fixture
[server]
data_dir = "memory"
admin_password = "pw-test"
%s

[auth]
keys = ["key-a"]

[[providers]]
name = "cheap"
kind = "openai"
base_url = "%s"
api_key = "k1"
models = ["fast"]
[[providers.tier]]
model = "fast"
power = 30

[[providers]]
name = "strong"
kind = "openai"
base_url = "%s"
api_key = "k2"
models = ["deep"]
[[providers.tier]]
model = "deep"
power = 95
reasoning = true

[[combo]]
name = "stack"
targets = ["cheap/fast", "strong/deep"]
`

// taskRoutingCall posts a chat request to combo "stack" and returns the
// upstream model that answered (the stub echoes it).
func taskRoutingCall(t *testing.T, h http.Handler, prompt string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    "stack",
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer key-a")
	w := do(t, h, r)
	if w.Code != 200 {
		t.Fatalf("upstream call failed: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Model
}

// The wiring proof for issue #54: with [server] task_routing = "on", a
// heavy request routed to combo "stack" must reach strong/deep first even
// though the combo lists cheap/fast first. With the switch off (default),
// the configured order holds.
func TestTaskRoutingServerWiring(t *testing.T) {
	upCheap := upstreamStub("fast")
	defer upCheap.Close()
	upStrong := upstreamStub("deep")
	defer upStrong.Close()

	run := func(t *testing.T, taskRouting string) string {
		t.Helper()
		_, h, _ := newTestServerFromFile(t, fmt.Sprintf(taskRoutingToml, taskRouting, upCheap.URL, upStrong.URL))
		heavy := strings.Repeat("please investigate, debug, refactor and analyze the architecture. ", 700)
		return taskRoutingCall(t, h, heavy)
	}

	t.Run("off keeps configured order", func(t *testing.T) {
		if got := run(t, ""); got != "fast" {
			t.Fatalf("off: got %q, want fast (configured first target)", got)
		}
	})

	t.Run("on tries best fit first", func(t *testing.T) {
		if got := run(t, "task_routing = \"on\""); got != "deep" {
			t.Fatalf("on: got %q, want deep (best fit for heavy task)", got)
		}
	})
}

// Config reload swaps the task-routing switch live (SIGHUP path).
func TestTaskRoutingReload(t *testing.T) {
	upCheap := upstreamStub("fast")
	defer upCheap.Close()
	upStrong := upstreamStub("deep")
	defer upStrong.Close()

	srv, h, path := newTestServerFromFile(t, fmt.Sprintf(taskRoutingToml, "", upCheap.URL, upStrong.URL))
	heavy := strings.Repeat("debug the architecture end-to-end. ", 1200)

	// Off by default: cheap first.
	if got := taskRoutingCall(t, h, heavy); got != "fast" {
		t.Fatalf("default must keep order, got %q", got)
	}

	// Flip the switch on disk and hot-reload (the SIGHUP path).
	cfgPath := fmt.Sprintf(taskRoutingToml, "task_routing = \"on\"", upCheap.URL, upStrong.URL)
	if err := os.WriteFile(path, []byte(cfgPath), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload load: %v", err)
	}
	srv.Reload(cfg)
	if got := taskRoutingCall(t, h, heavy); got != "deep" {
		t.Fatalf("after reload+on must reorder, got %q", got)
	}
}

// The decision lands in the #19 ring (kind "task_routing") when the
// reorder changes the order, and not when the configured order already
// fits.
func TestTaskRoutingDecisionLogged(t *testing.T) {
	upCheap := upstreamStub("fast")
	defer upCheap.Close()
	upStrong := upstreamStub("deep")
	defer upStrong.Close()

	srv, h, _ := newTestServerFromFile(t, fmt.Sprintf(taskRoutingToml, "task_routing = \"on\"", upCheap.URL, upStrong.URL))
	taskRoutingCall(t, h, strings.Repeat("debug the architecture. ", 1000))

	entries := srv.reqlog.latest(50)
	found := false
	for _, e := range entries {
		if e.Kind == "task_routing" {
			found = true
			if !strings.Contains(e.Err, "task=heavy") || !strings.Contains(e.Err, "strong/deep > cheap/fast") {
				t.Fatalf("decision detail = %q", e.Err)
			}
		}
	}
	if !found {
		t.Fatal("no task_routing row in the log ring")
	}
}
