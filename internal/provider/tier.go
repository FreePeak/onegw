package provider

import "path"

// ModelTier is the task-routing capability declaration for one model on
// a provider. Power scores range 0–150: higher = more capable. Vision
// and Reasoning flags gate hard-miss penalties; Context and MaxOut bound
// size-based penalties (0 = unknown, penalty not applied).
type ModelTier struct {
	Model     string `toml:"model"`     // exact id or path.Match glob ("*") does not cross "/")
	Power     int    `toml:"power"`     // 0–150 capability score
	Vision    bool   `toml:"vision"`    // handles image inputs
	Reasoning bool   `toml:"reasoning"` // reasoning model (o3, claude, deepseek-r1, etc.)
	Context   int    `toml:"context"`   // token context window (0 = unknown)
	MaxOut    int    `toml:"max_out"`   // max output tokens (0 = unknown)
}

// Tier looks up the task-routing metadata for model. Exact id match is
// checked first, then path.Match globs. Returns the first matching tier.
// The caller may mutate the returned value safely; returned false when
// no tier matches (caller should use defaults).
func (d *Def) Tier(model string) (ModelTier, bool) {
	for i := range d.Tiers {
		if d.Tiers[i].Model == model {
			return d.Tiers[i], true
		}
	}
	for i := range d.Tiers {
		if ok, err := path.Match(d.Tiers[i].Model, model); err == nil && ok {
			return d.Tiers[i], true
		}
	}
	return ModelTier{}, false
}
