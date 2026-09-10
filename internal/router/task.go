// Task-aware combo reordering (issue #54; PRD "Model tiering" adoption
// plan step 2). OmniRoute-style, stateless: classify each request's
// difficulty locally (no LLM call, O(1) bookkeeping), score every target
// of the resolved combo against that classification, and stable-sort the
// target list so the best-fit model is tried first. The full fallback
// chain always remains — reordering never removes a target.
//
// Deliberately NOT copied from OmniRoute: the LLM intent classifier (an
// LLM call to pick a model contradicts fast/low-RAM) and the heuristic
// modelPowerScore name-matching (onegw declares power per model in
// config, on the provider def). The classification numbers mirror
// OmniRoute taskAwareRouting.ts line-for-line, per the PRD anchor.
package router

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// TaskLevel is the classified difficulty of one request.
type TaskLevel int

const (
	TaskLight TaskLevel = iota
	TaskStandard
	TaskHeavy
	TaskCritical
)

// String renders the level for logs.
func (l TaskLevel) String() string {
	switch l {
	case TaskLight:
		return "light"
	case TaskHeavy:
		return "heavy"
	case TaskCritical:
		return "critical"
	default:
		return "standard"
	}
}

// weight is the level's difficulty rank (1..4); heavy and above gate the
// reasoning hard-miss penalty.
func (l TaskLevel) weight() int { return int(l) + 1 }

// taskTargetPower is the model power a combo target should have for this
// task level: `100 - |power - target|` sends the closest fit first.
var taskTargetPower = [...]int{
	TaskLight:    35,
	TaskStandard: 65,
	TaskHeavy:    95,
	TaskCritical: 120,
}

// undeclaredPower is the effective power of a combo target whose provider
// def declares no matching tier: with no capability data it can neither
// win nor lose outright, and it sorts by distance from the middle.
const undeclaredPower = 65

// Keyword regexes: a few word-anchored patterns over the request's text
// content (ReDoS-safe: fixed-length word literals under \b anchors, like
// the reference implementation).
var (
	lightTaskRe    = regexp.MustCompile(`(?i)\b(hi|hello|thanks|thank you|ping|format|rewrite|grammar|translate|summari[sz]e|short|quick|one[- ]?liner|explain briefly)\b`)
	heavyTaskRe    = regexp.MustCompile(`(?i)\b(debug|root cause|architecture|architectural|refactor|migrate|implementation|implement|design|analy[sz]e|investigate|compare|benchmark|whitebox|codebase|end[- ]?to[- ]?end|e2e)\b`)
	criticalTaskRe = regexp.MustCompile(`(?i)\b(critical|security|vulnerability|exploit|rce|remote code execution|supply chain|account takeover|auth bypass|privilege escalation|tenant|cross[- ]tenant|sandbox escape|ssrf|deserialization|prod incident|data exfiltration|bug bounty)\b`)
)

// maxTaskTextBytes bounds the text kept for the keyword regexes. The size
// signals are computed over the full prompt text; only the keyword probe
// is capped, so a bounded buffer keeps the memory footprint flat.
const maxTaskTextBytes = 16 << 10

// TaskSignals are the cheap local facts the classifier reads. Collected
// once per request from the raw body (CollectSignals) and carried to
// Execute on the context (WithTask).
type TaskSignals struct {
	PromptChars int    // prompt text size (chars ≈ bytes)
	Messages    int    // conversation turns
	Tools       int    // tool definitions offered
	MaxTokens   int    // requested output cap (0 = unset)
	Effort      string // reasoning-effort knob, lowercased ("" = unset)
	HasImage    bool   // request carries image content (hard vision gate)
	LightKW     bool
	HeavyKW     bool
	CriticalKW  bool
}

// Task is the classified request: signals plus the decision and the
// reasons that produced it (for the admin log line).
type Task struct {
	TaskSignals
	Level   TaskLevel
	Reasons []string
}

// effortIsHigh reports a high reasoning-effort knob.
func effortIsHigh(effort string) bool {
	switch effort {
	case "high", "xhigh", "max", "maximum", "hard", "deep":
		return true
	}
	return false
}

// effortIsLight reports an absent or explicitly low reasoning knob.
func effortIsLight(effort string) bool {
	if effort == "" {
		return true
	}
	switch effort {
	case "low", "minimal", "none", "off", "disabled":
		return true
	}
	return false
}

// Classify maps collected signals to a task level. Cheap, local signals
// only — a routing hint, not a judgment: light requests stay on fast/
// cheap models while large, tool-heavy, security-sensitive, or reasoning-
// heavy requests try stronger models first. Fallback still tries every
// target, so a misclassification costs an ordering, never a failure.
func (s TaskSignals) Classify() Task {
	var reasons []string
	add := func(cond bool, reason string) bool {
		if cond {
			reasons = append(reasons, reason)
		}
		return cond
	}
	high := effortIsHigh(s.Effort)

	// Critical: huge context, huge output, many tools on a large prompt,
	// or security-domain keywords on an already serious request.
	critical := add(s.PromptChars >= 100_000, "huge-context") ||
		add(s.MaxTokens >= 32768, "huge-output") ||
		add(s.Tools >= 8 && s.PromptChars >= 16_000, "many-tools-large-context") ||
		add(s.CriticalKW && (high || s.Tools >= 3 || s.PromptChars >= 8_000), "critical-domain")
	if critical {
		return Task{TaskSignals: s, Level: TaskCritical, Reasons: reasons}
	}

	// Heavy: two or more independent size/shape/effort signals.
	heavySignals := 0
	if add(s.PromptChars >= 50_000, "large-context") {
		heavySignals++
	}
	if add(s.PromptChars >= 24_000, "medium-large-context") {
		heavySignals++
	}
	if add(s.Messages >= 16, "long-conversation") {
		heavySignals++
	}
	if add(s.Tools >= 4, "many-tools") {
		heavySignals++
	}
	if add(s.MaxTokens >= 8192, "large-output") {
		heavySignals++
	}
	if add(high, "high-reasoning-effort") {
		heavySignals++
	}
	if add(s.CriticalKW, "security-sensitive") {
		heavySignals++
	}
	if add(s.HeavyKW && s.PromptChars >= 4_000, "complex-task") {
		heavySignals++
	}
	if heavySignals >= 2 || s.PromptChars >= 50_000 || high {
		return Task{TaskSignals: s, Level: TaskHeavy, Reasons: reasons}
	}

	// Light: tiny request with no heavy/critical marker.
	light := s.PromptChars <= 2_000 &&
		s.Messages <= 3 &&
		s.Tools == 0 &&
		s.MaxTokens <= 1500 &&
		effortIsLight(s.Effort) &&
		!s.CriticalKW &&
		!s.HeavyKW
	if light || (s.LightKW && s.PromptChars <= 4_000 && s.Tools == 0 && effortIsLight(s.Effort) && !s.CriticalKW) {
		if len(reasons) == 0 {
			reasons = []string{"small-simple-request"}
		}
		return Task{TaskSignals: s, Level: TaskLight, Reasons: reasons}
	}

	if len(reasons) == 0 {
		reasons = []string{"default"}
	}
	return Task{TaskSignals: s, Level: TaskStandard, Reasons: reasons}
}

// tierMeta is a combo target's resolved capability data: the declared
// tier when its provider def has one, else neutral defaults.
type tierMeta struct {
	power     int
	vision    bool
	reasoning bool
	context   int // token context window, 0 = unknown
	maxOut    int // max output tokens, 0 = unknown
	declared  bool
}

// tierFor resolves a target's tier metadata from the provider pool.
// Undeclared targets get neutral power and no capability assertions, so
// they neither win nor lose a reorder they have no data for.
func (r *Router) tierFor(t Target) tierMeta {
	def, ok := r.pool.Get(t.Provider)
	if !ok {
		return tierMeta{power: undeclaredPower}
	}
	tier, ok := def.Tier(t.Model)
	if !ok {
		return tierMeta{power: undeclaredPower}
	}
	return tierMeta{
		power:     tier.Power,
		vision:    tier.Vision,
		reasoning: tier.Reasoning,
		context:   tier.Context,
		maxOut:    tier.MaxOut,
		declared:  true,
	}
}

// scoreTarget computes one target's fit for a classified task. Mirrors
// the PRD anchor: `100 - |power - target|` with hard-miss penalties
// (vision missing -10000, non-reasoning model on heavy task -120, prompt
// > 85% of the model's context -200). Negative scores sort the target to
// the back — it stays in the chain as fallback.
func scoreTarget(m tierMeta, task Task) int {
	score := 100 - abs(m.power-taskTargetPower[task.Level])

	// Hard capability misses: only assertable against a declared tier.
	if m.declared {
		if task.HasImage && !m.vision {
			score -= 10000
		}
		if task.Level.weight() >= TaskHeavy.weight() && !m.reasoning {
			score -= 120
		}
		// Prompt approaching the model's context window (≈4 chars/token).
		if m.context > 0 && task.PromptChars/4 > m.context*85/100 {
			score -= 200
		}
		if m.maxOut > 0 && task.MaxTokens > 0 && task.MaxTokens > m.maxOut {
			score -= 80
		}
	}
	// Overkill/underkill refinements (apply to the effective power,
	// including the undeclared neutral).
	switch task.Level {
	case TaskLight:
		if m.power > 95 {
			score -= 35
		}
	case TaskStandard:
		if m.power > 125 {
			score -= 10
		}
	case TaskHeavy:
		if m.power < 65 {
			score -= 60
		}
	case TaskCritical:
		if m.power < 85 {
			score -= 100
		}
	}
	return score
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// taskKey carries the request's task signals on the context.
type taskKey struct{}

// WithTask tags ctx with the request's task signals. The buffered proxy
// paths call it when task routing is enabled, after collecting them from
// the raw body.
func WithTask(ctx context.Context, sig TaskSignals) context.Context {
	return context.WithValue(ctx, taskKey{}, sig)
}

// TaskFrom extracts the tagged signals; ok=false when absent (task
// routing off, or a path that never collected them) — Execute then keeps
// the configured order byte-for-byte.
func TaskFrom(ctx context.Context) (TaskSignals, bool) {
	if ctx == nil {
		return TaskSignals{}, false
	}
	sig, ok := ctx.Value(taskKey{}).(TaskSignals)
	return sig, ok
}

// applyTaskRouting reorders a combo resolution for the request's task,
// before any account selection. No-op unless task routing is on AND the
// server tagged signals AND this is a multi-target combo: the off path
// stays byte-identical to the pre-#54 behavior. Logs the decision when
// it actually changed the order.
func (r *Router) applyTaskRouting(ctx context.Context, res *Resolution) {
	if !r.taskRoutingOn || !res.IsCombo || len(res.Targets) <= 1 {
		return
	}
	sig, ok := TaskFrom(ctx)
	if !ok {
		return
	}
	task := sig.Classify()
	orig := make([]Target, len(res.Targets))
	copy(orig, res.Targets)
	r.reorderByTask(res, task)
	for i := range orig {
		if orig[i] != res.Targets[i] {
			r.logTaskDecision(res, task)
			return
		}
	}
}

// reorderBySpeed stable-sorts res.Targets by each leg's recent decode
// speed (tokens/sec EWMA, provider speed.go), fastest first. Legs with no
// speed data report 0 and keep the configured order among themselves; the
// full chain is preserved — only the order changes.
func (r *Router) reorderBySpeed(res *Resolution) {
	type scored struct {
		t Target
		s float64
	}
	order := make([]scored, len(res.Targets))
	for i, t := range res.Targets {
		s := 0.0
		if def, ok := r.pool.Get(t.Provider); ok {
			s = def.ModelTPS(t.Model)
		}
		order[i] = scored{t, s}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].s > order[j].s })
	for i := range order {
		res.Targets[i] = order[i].t
	}
}

// reorderByTask stable-sorts res.Targets so the best fit comes first:
// equal scores keep the original order, and no target is ever removed.
func (r *Router) reorderByTask(res *Resolution, task Task) {
	type scored struct {
		t Target
		s int
	}
	order := make([]scored, len(res.Targets))
	for i, t := range res.Targets {
		order[i] = scored{t, scoreTarget(r.tierFor(t), task)}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].s > order[j].s })
	for i := range order {
		res.Targets[i] = order[i].t
	}
}

// maxTaskLogDetail bounds the decision line: the log ring caps stored
// error text at 300 and the console renders ~160.
const maxTaskLogDetail = 280

// logTaskDecision emits one #19-ring row for a reorder that changed the
// order. The sink is server-owned (observeLog's requestLog); TaskLog is
// the narrow seam — nil means silent.
func (r *Router) logTaskDecision(res *Resolution, task Task) {
	if r.TaskLog == nil {
		return
	}
	var b strings.Builder
	b.WriteString("task=")
	b.WriteString(task.Level.String())
	if len(task.Reasons) > 0 {
		b.WriteString(" [")
		b.WriteString(strings.Join(task.Reasons, ","))
		b.WriteString("] ")
	}
	for i, t := range res.Targets {
		if i > 0 {
			b.WriteString(" > ")
		}
		b.WriteString(t.Provider)
		b.WriteString("/")
		b.WriteString(t.Model)
	}
	detail := b.String()
	if len(detail) > maxTaskLogDetail {
		detail = detail[:maxTaskLogDetail]
	}
	r.TaskLog(res.Model, detail)
}

// ---------------------------------------------------------------------------
// Signal collection: raw request body → TaskSignals.
//
// One bounded decode of the body the server has already read. Understands
// the three buffered surfaces' top-level shapes (OpenAI/Anthropic
// messages, Gemini contents) well enough for a routing hint — this is not
// a translation pass and must not become one.
// ---------------------------------------------------------------------------

// taskPart is one content part across the three surfaces: OpenAI/Anthropic
// {type,text} parts, Gemini {text} / {inline_data} parts.
type taskPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	InlineData json.RawMessage `json:"inline_data"` // Gemini image part marker
}

// taskMsg is one conversation turn: content is either a string or an
// array of taskPart.
type taskMsg struct {
	Content json.RawMessage `json:"content"`
	Parts   []taskPart      `json:"parts"` // Gemini contents[].parts
}

// taskProbe mirrors the top-level fields the classifier reads, across the
// OpenAI, Anthropic, and Gemini request shapes. RawMessage keeps the
// decode shallow: nested bodies are walked only where text lives.
type taskProbe struct {
	System       json.RawMessage   `json:"system"`       // Anthropic (string or text blocks)
	Instructions json.RawMessage   `json:"instructions"` // Responses-style system instruction
	Messages     []json.RawMessage `json:"messages"`     // OpenAI / Anthropic turns
	Contents     []json.RawMessage `json:"contents"`     // Gemini turns
	Tools        []json.RawMessage `json:"tools"`        // tool definitions (all three)

	MaxTokens       int64  `json:"max_tokens"`            // OpenAI / Anthropic
	MaxCompletion   int64  `json:"max_completion_tokens"` // OpenAI alt knob
	MaxOutput       int64  `json:"max_output_tokens"`     // Responses alt knob
	GenCfg          genCfg `json:"generationConfig"`      // Gemini
	ReasoningEffort string `json:"reasoning_effort"`      // OpenAI knob
	Reasoning       struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

type genCfg struct {
	MaxOutputTokens int64 `json:"maxOutputTokens"`
}

// CollectSignals extracts the classifier's facts from a raw request body
// in one shallow decode. Malformed JSON yields zero signals, which
// classifies as light — a hint, never an error.
func CollectSignals(body []byte) TaskSignals {
	var probe taskProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return TaskSignals{}
	}
	sig := TaskSignals{
		Messages:  len(probe.Messages) + len(probe.Contents),
		Tools:     len(probe.Tools),
		MaxTokens: int(max4(probe.MaxTokens, probe.MaxCompletion, probe.MaxOutput, probe.GenCfg.MaxOutputTokens)),
		Effort:    strings.ToLower(strings.TrimSpace(firstNonEmpty(probe.ReasoningEffort, probe.Reasoning.Effort))),
	}

	// Bounded text buffer for the keyword probes; prompt size counts the
	// full text regardless of the cap.
	text := make([]byte, 0, 1024)
	addText := func(s string) {
		sig.PromptChars += len(s)
		if len(text) < maxTaskTextBytes && s != "" {
			room := maxTaskTextBytes - len(text)
			if len(s) > room {
				s = s[:room]
			}
			text = append(text, s...)
			text = append(text, '\n')
		}
	}

	collectRaw(probe.System, addText, &sig)
	collectRaw(probe.Instructions, addText, &sig)
	for _, raw := range probe.Messages {
		collectMsg(raw, addText, &sig)
	}
	for _, raw := range probe.Contents {
		collectMsg(raw, addText, &sig)
	}

	sig.LightKW = lightTaskRe.Match(text)
	sig.HeavyKW = heavyTaskRe.Match(text)
	sig.CriticalKW = criticalTaskRe.Match(text)
	return sig
}

// collectRaw handles a RawMessage that is either a JSON string (its text
// counts) or an array of text blocks ({text}); anything else is ignored.
func collectRaw(raw json.RawMessage, addText func(string), sig *TaskSignals) {
	if len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			addText(s)
		}
	case '[':
		var blocks []taskPart
		if json.Unmarshal(raw, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "image_url" || b.Type == "image" || len(b.InlineData) > 0 {
					sig.HasImage = true
				}
				addText(b.Text)
			}
		}
	}
}

// collectMsg walks one conversation turn for text length, keyword text,
// and image markers. Content shapes: string, array of parts (OpenAI/
// Anthropic), or Gemini's parts array on the turn itself.
func collectMsg(raw json.RawMessage, addText func(string), sig *TaskSignals) {
	var m taskMsg
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if m.Parts != nil {
		for _, p := range m.Parts {
			if len(p.InlineData) > 0 || p.Type == "image_url" || p.Type == "image" {
				sig.HasImage = true
			}
			addText(p.Text)
		}
	}
	if len(m.Content) > 0 {
		collectRaw(m.Content, addText, sig)
	}
}

func max4(a, b, c, d int64) int64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	if d > m {
		m = d
	}
	return m
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
