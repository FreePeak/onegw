package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"onegw/internal/translat"
)

// askSchema is the live failing tool (2026-09-17, xai/grok-4.6): a root object
// with properties plus a root anyOf spelling "one of these property sets".
const askSchema = `{
  "type": "object",
  "properties": {
    "question": {"type": "string"},
    "options": {"type": "array", "items": {"type": "object"}}
  },
  "anyOf": [
    {"required": ["question", "options"]},
    {"required": ["questions"]}
  ]
}`

func toolBody(t *testing.T, schema string) []byte {
	t.Helper()
	var params any
	if err := json.Unmarshal([]byte(schema), &params); err != nil {
		t.Fatalf("bad test schema: %v", err)
	}
	b, err := json.Marshal(map[string]any{
		"model":    "xai/grok-4.6",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       "ask",
				"parameters": params,
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal test body: %v", err)
	}
	return b
}

// rootUnionBranches returns the "type" declared by each branch of a tool
// parameters root union, in order.
func rootUnionBranches(t *testing.T, body []byte) []string {
	t.Helper()
	var root struct {
		Tools []struct {
			Function struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(root.Tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(root.Tools))
	}
	params := root.Tools[0].Function.Parameters
	return unionTypes(t, params, "anyOf", "oneOf")
}

func unionTypes(t *testing.T, node map[string]any, keys ...string) []string {
	t.Helper()
	for _, k := range keys {
		branches, ok := node[k].([]any)
		if !ok {
			continue
		}
		out := make([]string, 0, len(branches))
		for _, b := range branches {
			obj, ok := b.(map[string]any)
			if !ok {
				t.Fatalf("branch is not an object: %v", b)
			}
			s, _ := obj["type"].(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

// TestNormalizeToolRootsRetypesUnionBranches pins the rule against the live
// validator: the ask-shaped root union is rejected until every branch declares
// an object type, at which point the same request passes.
func TestNormalizeToolRootsRetypesUnionBranches(t *testing.T) {
	out := normalizeToolRoots(toolBody(t, askSchema))
	if bytes.Equal(out, toolBody(t, askSchema)) {
		t.Fatal("ask-shaped root union must be retyped")
	}
	if got := rootUnionBranches(t, out); len(got) != 2 || got[0] != "object" || got[1] != "object" {
		t.Fatalf("branches must all be object-typed, got %q: %s", got, out)
	}
	// The constraint the union expressed has to survive the retype.
	var root struct {
		Tools []struct {
			Function struct {
				Parameters struct {
					Type       string           `json:"type"`
					Properties map[string]any   `json:"properties"`
					AnyOf      []map[string]any `json:"anyOf"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	p := root.Tools[0].Function.Parameters
	if p.Type != "object" || len(p.Properties) != 2 || len(p.AnyOf) != 2 {
		t.Fatalf("parameters shell must be intact, got %+v", p)
	}
	if reqs, _ := p.AnyOf[0]["required"].([]any); len(reqs) != 2 {
		t.Fatalf("branch required list must survive, got %v", p.AnyOf[0])
	}
}

// TestNormalizeToolRootsLeavesAcceptedShapesAlone pins cache stability the same
// way TestNormalizeToolPairsLeavesShapedHistoryAlone does: every shape the live
// validator ACCEPTS must come back byte-identical, so a client's prompt-cache
// prefix is never disturbed by this pass.
func TestNormalizeToolRootsLeavesAcceptedShapesAlone(t *testing.T) {
	accepted := map[string]string{
		// 200 live: branches already typed.
		"typed anyOf": `{"type":"object","properties":{"a":{"type":"string"}},
			"anyOf":[{"type":"object","required":["a"]},{"type":"object","required":["b"]}]}`,
		// 200 live: no union at the root.
		"no union": `{"type":"object","properties":{"a":{"type":"string"}}}`,
		// 200 live: union nested, not at the root.
		"nested anyOf": `{"type":"object","properties":{"a":{"anyOf":[{"required":["x"]},{"required":["y"]}]}}}`,
		// 200 live: root allOf is a different keyword entirely.
		"root allOf": `{"type":"object","properties":{"a":{"type":"string"}},"allOf":[{"required":["a"]}]}`,
		// Not object-compatible: retyping would narrow it, so leave it verbatim.
		"mixed branch": `{"type":"object","properties":{"a":{"type":"string"}},
			"anyOf":[{"type":"object","required":["a"]},{"type":"string"}]}`,
	}
	for name, schema := range accepted {
		body := toolBody(t, schema)
		if out := normalizeToolRoots(body); !bytes.Equal(out, body) {
			t.Errorf("%s: accepted shape must stay byte-identical, got %s", name, out)
		}
	}
	// Union-less bodies short-circuit on the substring probe: same slice back.
	plain := []byte(`{"model":"m","messages":[]}`)
	if out := normalizeToolRoots(plain); &out[0] != &plain[0] {
		t.Error("body with no union keyword must be returned unchanged, not re-marshalled")
	}
}

// TestNormalizeToolRootsOneOf covers the sibling keyword.
func TestNormalizeToolRootsOneOf(t *testing.T) {
	body := toolBody(t, `{"type":"object","properties":{"a":{"type":"string"}},
		"oneOf":[{"required":["a"]},{"required":["b"]}]}`)
	if got := rootUnionBranches(t, normalizeToolRoots(body)); len(got) != 2 || got[0] != "object" {
		t.Fatalf("oneOf branches must be typed, got %q", got)
	}
}

// TestPrepareUpstreamBodyTypesToolRoot wires the pass into the sink both
// server paths share: the live 400 came from a same-format (OpenAI → xai) body
// that buildUpstreamBody otherwise forwards verbatim.
func TestPrepareUpstreamBodyTypesToolRoot(t *testing.T) {
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, toolBody(t, askSchema), "grok-4.6", nil)
	if err != nil {
		t.Fatalf("prepareUpstreamBody: %v", err)
	}
	if got := rootUnionBranches(t, out); len(got) != 2 || got[0] != "object" || got[1] != "object" {
		t.Fatalf("upstream body must carry typed branches, got %q: %s", got, out)
	}
	if !bytes.Contains(out, []byte(`"grok-4.6"`)) {
		t.Fatalf("model rewrite must still apply: %s", out)
	}
}
