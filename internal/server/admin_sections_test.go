package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestSectionsGetShowsEffectiveDefaults(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, editTestToml)
	w := adminCall(t, h, http.MethodGet, "/admin/config/sections", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET sections: %d %s", w.Code, w.Body.String())
	}
	// The fixture sets no [rotation]; GET must still report the gateway's
	// effective defaults (cooldown_base 10s, and the int 0-sentinel shown as
	// its built-in 4, not the raw 0), so the UI shows real state.
	for _, want := range []string{`"rotation"`, `"cooldown_base":"10s"`, `"flap_threshold":"4"`, `"task_routing"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("GET sections missing %s:\n%s", want, w.Body.String())
		}
	}
}

func TestSectionsPutRoundTrip(t *testing.T) {
	_, h, path := newTestServerFromFile(t, editTestToml)
	before := mustReadFile(t, path)

	// Set a value in a section the file doesn't have: creates the block.
	w := adminCall(t, h, http.MethodPut, "/admin/config/sections",
		`{"section":"rotation","settings":{"cooldown_base":"45s"}}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"cooldown_base"`) {
		t.Fatalf("PUT rotation: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if !strings.Contains(file, "[rotation]") || !strings.Contains(file, `cooldown_base = "45s"`) {
		t.Fatalf("rotation block not written:\n%s", file)
	}
	// The untouched provider comment survives the splice.
	if !strings.Contains(file, "p1 comment that must survive") {
		t.Fatalf("splice clobbered unrelated lines:\n%s", diffLines(before, file))
	}

	// Set a value in the existing [server] block, then reset it to default:
	// the key must be removed (default re-asserts), not left at the default.
	if w := adminCall(t, h, http.MethodPut, "/admin/config/sections",
		`{"section":"server","settings":{"max_body_bytes":1048576}}`, true); w.Code != http.StatusOK {
		t.Fatalf("PUT server: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(mustReadFile(t, path), "max_body_bytes = 1048576") {
		t.Fatalf("server max_body_bytes not spliced:\n%s", mustReadFile(t, path))
	}
	if w := adminCall(t, h, http.MethodPut, "/admin/config/sections",
		`{"section":"server","settings":{"max_body_bytes":0}}`, true); w.Code != http.StatusOK {
		t.Fatalf("reset server: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(mustReadFile(t, path), "max_body_bytes") {
		t.Fatalf("max_body_bytes should be removed on default:\n%s", mustReadFile(t, path))
	}

	// Nested table key: [saver.external] must get the BARE name.
	if w := adminCall(t, h, http.MethodPut, "/admin/config/sections",
		`{"section":"saver","settings":{"external.url":"http://rtk.local/api"}}`, true); w.Code != http.StatusOK {
		t.Fatalf("PUT saver.external: %d %s", w.Code, w.Body.String())
	}
	file = mustReadFile(t, path)
	if !strings.Contains(file, "[saver.external]") || !strings.Contains(file, `url = "http://rtk.local/api"`) {
		t.Fatalf("nested external.url not written bare:\n%s", file)
	}
	if strings.Contains(file, "external.url =") {
		t.Fatalf("dotted key leaked into the block:\n%s", file)
	}

	// Rejections: unknown section/key, wrong type for an int.
	for _, body := range []string{
		`{"section":"ghost","settings":{"a":"b"}}`,
		`{"section":"server","settings":{"nope":"b"}}`,
		`{"section":"server","settings":{"max_body_bytes":"notanumber"}}`,
	} {
		if w := adminCall(t, h, http.MethodPut, "/admin/config/sections", body, true); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", body, w.Code, w.Body.String())
		}
	}
}
