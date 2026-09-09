package server

import (
	"encoding/json"
	"strings"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/translat"
	"onegw/internal/types"
)

// assertField decodes a prepared body and checks the top-level string
// field; want "" means the key must be absent.
func assertField(t *testing.T, body []byte, key, want string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	got, _ := m[key].(string)
	if want == "" {
		if _, ok := m[key]; ok {
			t.Errorf("%s = %q, want absent (body %s)", key, got, body)
		}
		return
	}
	if got != want {
		t.Errorf("%s = %q, want %q (body %s)", key, got, want, body)
	}
}

func TestAlwaysThinkingModel(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"glm-*", "z-ai/glm-*"}}
	for model, want := range map[string]bool{
		"glm-5.3-flash":      true,
		"glm-4.5-air":        true,
		"z-ai/glm-5.3":       true,
		"openai/gpt-5.4":     false,
		"claude-opus-5":      false,
		"minimax/minimax-m3": false,
	} {
		if got := def.AlwaysThinkingModel(model); got != want {
			t.Errorf("AlwaysThinkingModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestCoerceEffort(t *testing.T) {
	for in, want := range map[string]string{
		"none": "low", "minimal": "low", "medium": "low",
		"xhigh": "max", // client ladders above high map onto GLM's max
		"low":   "low", "high": "high", "max": "max",
		"ultra": "high", // unrecognized values land on the accepted middle
	} {
		if got := coerceEffort(in); got != want {
			t.Errorf("coerceEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAlwaysThinking400Signature(t *testing.T) {
	yes := []types.APIError{
		{Status: 400, Code: "1210", Message: "use low, high or max"},
		{Status: 400, Code: "400001", Type: "invalid_request_error",
			Message: "The request is invalid: 该模型始终思考，不支持关闭思考；请使用 low、high 或 max。"},
		{Status: 400, Message: "This model always thinks; please use low, high, or max."},
	}
	for _, e := range yes {
		e := e
		if !alwaysThinking400(&e) {
			t.Errorf("alwaysThinking400(%q code=%q) = false, want true", e.Message, e.Code)
		}
	}
	no := []types.APIError{
		{Status: 400, Type: "invalid_request", Message: "missing messages"},     // plain bad request
		{Status: 400, Code: "400001", Message: "invalid parameter temperature"}, // code alone is not enough
		{Status: 429, Message: "该模型始终思考"},                                       // wrong status class
	}
	for i := range no {
		if alwaysThinking400(&no[i]) {
			t.Errorf("alwaysThinking400(%v) = true, want false", no[i])
		}
	}
}

func TestLearnAlwaysThinking(t *testing.T) {
	def := &provider.Def{} // no globs configured
	if def.AlwaysThinkingModel("glm-5.3-flash") {
		t.Fatal("unlearned model must not be always-thinking")
	}
	if !def.LearnAlwaysThinking("glm-5.3-flash") {
		t.Fatal("first learn must report newly learned")
	}
	if def.LearnAlwaysThinking("glm-5.3-flash") {
		t.Error("second learn of the same model must not report newly learned")
	}
	if !def.AlwaysThinkingModel("glm-5.3-flash") {
		t.Error("learned model must be reported always-thinking")
	}
	if def.AlwaysThinkingModel("glm-5.1") {
		t.Error("other models must not inherit the learned flag")
	}
}

func TestAdaptAlwaysThinking(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"glm-*"}}

	// GLM error 1210: explicit disable knobs must be rewritten.
	body := []byte(`{"model":"m","reasoning_effort":"none","thinking":{"type":"disabled"},"enable_thinking":false,"messages":[]}`)
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := m["reasoning_effort"].(string); got != "low" {
		t.Errorf("reasoning_effort = %q, want low (body %s)", got, out)
	}
	for _, key := range []string{"thinking", "enable_thinking"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s not dropped: %s", key, out)
		}
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Errorf("messages lost: %s", out)
	}

	// Supported values pass through untouched.
	body = []byte(`{"model":"m","reasoning_effort":"high","messages":[]}`)
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	assertField(t, out, "reasoning_effort", "high")

	// No knob present → nothing added.
	body = []byte(`{"model":"m","messages":[]}`)
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	assertField(t, out, "reasoning_effort", "")

	// Non-matching model → untouched (OpenAI accepts "none").
	body = []byte(`{"model":"m","reasoning_effort":"none","messages":[]}`)
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "gpt-5.4-mini", def)
	assertField(t, out, "reasoning_effort", "none")

	// nil def (never configured) → untouched.
	out, _ = prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", nil)
	assertField(t, out, "reasoning_effort", "none")
}

func TestAdaptAlwaysThinkingPreservesNumbers(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"*"}}
	body := []byte(`{"model":"m","temperature":0.7,"reasoning_effort":"medium","max_tokens":4096,"messages":[]}`)
	out, err := prepareUpstreamBody(translat.FmtOpenAI, translat.FmtOpenAI, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"reasoning_effort":"low"`) {
		t.Errorf("want reasoning_effort low in %s", got)
	}
	if !strings.Contains(got, "0.7") || !strings.Contains(got, "4096") {
		t.Errorf("numeric fidelity broken: %s", got)
	}
}

func TestPrepareUpstreamBodyCrossFormatAlwaysThinking(t *testing.T) {
	def := &provider.Def{AlwaysThinking: []string{"glm-*"}}

	// OpenAI-surface effort values reach the Responses wire on the
	// cross-format path — every rejected value must arrive coerced
	// (EncodeResponsesRequest emits reasoning.effort from u.ReasoningEffort).
	for in, want := range map[string]string{
		// xhigh coerces to max on the unified struct; the Responses
		// encoder then clamps max→high (its enum tops out at high).
		"none": "low", "minimal": "low", "medium": "low", "xhigh": "high",
	} {
		body := []byte(`{"model":"m","reasoning_effort":"` + in + `","messages":[]}`)
		out, err := prepareUpstreamBody(translat.FmtResponses, translat.FmtOpenAI, body, "glm-5.3-flash", def)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		r, _ := m["reasoning"].(map[string]any)
		if got, _ := r["effort"].(string); got != want {
			t.Errorf("effort %q → reasoning.effort = %q, want %q (body %s)", in, got, want, out)
		}
	}

	// An Anthropic-surface thinking budget encodes to a Responses upstream
	// as a derived effort that can violate the low|high|max enum; the
	// unified knob must be dropped instead (upstream default applies).
	body := []byte(`{"model":"m","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[]}`)
	out, err := prepareUpstreamBody(translat.FmtResponses, translat.FmtAnthropic, body, "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"reasoning"`) {
		t.Errorf("thinking budget leaked onto an always-thinking Responses upstream: %s", out)
	}

	// No knob present → nothing invented.
	out, err = prepareUpstreamBody(translat.FmtResponses, translat.FmtOpenAI,
		[]byte(`{"model":"m","messages":[]}`), "glm-5.3-flash", def)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"reasoning"`) {
		t.Errorf("knob invented on an always-thinking upstream: %s", out)
	}

	// Non-listed model / nil def → effort passes through untouched.
	for _, d := range []*provider.Def{nil, {AlwaysThinking: []string{"glm-*"}}} {
		out, err = prepareUpstreamBody(translat.FmtResponses, translat.FmtOpenAI,
			[]byte(`{"model":"m","reasoning_effort":"medium","messages":[]}`), "gpt-5.4-mini", d)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		r, _ := m["reasoning"].(map[string]any)
		if got, _ := r["effort"].(string); got != "medium" {
			t.Errorf("non-listed model effort = %q, want medium passthrough (def %v, body %s)", got, d, out)
		}
	}
}
