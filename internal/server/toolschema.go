// Tool-parameter root normalization for strict validators.
//
// Live 2026-09-17 (xai/grok-4.6 chat): xAI's validator rejects a tool
// "parameters" schema whose ROOT is a union (anyOf/oneOf) any branch of which
// does not itself declare an object type:
//
//	ask: tool parameter root must be an object type
//	     (root schema is an anyOf/oneOf union with a non-object branch)
//
// The tripping shape is the common one — a root object carrying `properties`
// plus `anyOf:[{required:[...]},{required:[...]}]`, i.e. "one of these property
// sets" spelled at the root (the ask tool's own schema). Probed through the
// live gateway with everything else held fixed:
//
//	root anyOf, untyped branches      400   (the reported failure)
//	root anyOf, branches retyped      200
//	root anyOf dropped                200   (but loses the constraint)
//	root allOf, object branches       200
//	root with no `type` at all        200   (union absent)
//	nested anyOf/oneOf, any depth     200
//
// Retyping is the only rewrite that passes AND keeps the constraint: under a
// root that is already an object, `type:"object"` on a branch admits no
// different instance, while dropping the union loses the required-alternative
// it expresses. Only object-compatible branches are retyped (no `type` key, or
// `type:"object"`), and only when EVERY branch is — a genuinely mixed union
// (an object branch beside `type:"string"`) is left verbatim rather than
// silently narrowed; that shape is the tool's own semantics to fix.
//
// Only the tool root is touched: nested unions are accepted (and rewriting them
// would change schemas no validator complained about). Scope is the OpenAI chat
// wire (tools[].function.parameters); a Responses-wire upstream encodes tools
// flat and has never been observed to reject this shape, so it is left alone.
//
// Returns body unchanged when there is nothing to fix, so tool-carrying bodies
// that need nothing pay only a cheap substring probe. A body that IS rewritten
// is re-marshalled whole (Go sorts object keys) and so is not byte-faithful to
// the client's original — affordable because the rewrite is deterministic: the
// same client schema produces the same bytes every time, so only the first
// request of a session is a prompt-cache miss.
package server

import (
	"bytes"
	"encoding/json"
)

// normalizeToolRoots declares `type:"object"` on the branches of a tool
// parameters root union.
func normalizeToolRoots(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"anyOf"`)) && !bytes.Contains(body, []byte(`"oneOf"`)) {
		return body
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body // not an object; forward verbatim
	}
	tools, ok := root["tools"].([]any)
	if !ok {
		return body
	}
	changed := false
	for _, tv := range tools {
		t, ok := tv.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		if retypeUnionBranches(params, "anyOf") {
			changed = true
		}
		if retypeUnionBranches(params, "oneOf") {
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// retypeUnionBranches writes `type:"object"` on every branch of node[key] when
// all of them are object-compatible, reporting whether it wrote. The union
// itself is never dropped.
func retypeUnionBranches(node map[string]any, key string) bool {
	branches, ok := node[key].([]any)
	if !ok || len(branches) == 0 {
		return false
	}
	objs := make([]map[string]any, 0, len(branches))
	for _, b := range branches {
		obj, ok := b.(map[string]any)
		if !ok {
			return false
		}
		if tv, has := obj["type"]; has && tv != "object" {
			return false
		}
		objs = append(objs, obj)
	}
	wrote := false
	for _, obj := range objs {
		if _, has := obj["type"]; has {
			continue
		}
		obj["type"] = "object"
		wrote = true
	}
	return wrote
}
