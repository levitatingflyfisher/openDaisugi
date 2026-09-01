package detect

import "strings"

type Evaluated struct {
	ID            string `json:"id"`
	Priority      int    `json:"priority"`
	Region        string `json:"region"`
	State         State  `json:"state"`
	Matched       bool   `json:"matched"`
	RegionBytes   int    `json:"region_bytes"`
	RegionPreview string `json:"region_preview"`
}

type Result struct {
	Agent          string      `json:"agent"`
	Matched        bool        `json:"matched"`
	State          State       `json:"state"`
	RuleID         string      `json:"rule_id"`
	Priority       int         `json:"priority"`
	Region         string      `json:"region"`
	Skip           bool        `json:"skip_state_update"`
	VisibleIdle    bool        `json:"visible_idle"`
	VisibleBlocker bool        `json:"visible_blocker"`
	VisibleWorking bool        `json:"visible_working"`
	Evaluated      []Evaluated `json:"evaluated"`
}

const previewMax = 240

func preview(s string) string {
	r := []rune(s)
	if len(r) <= previewMax {
		return s
	}
	return string(r[:previewMax]) + "..."
}

// Evaluate runs every rule in file order and keeps the highest priority match.
// A tie keeps the earlier rule, exactly as Herdr does: several bundled
// manifests rely on file order as the tiebreak.
func (c *Compiled) Evaluate(in Input) Result {
	// StateUnknown is the default so a no-match Result never reports the
	// zero value "": a caller that reads r.State without also checking
	// r.Matched should see "unknown", not an empty string that isn't one of
	// the four declared State constants. The matched path below overwrites
	// this with the winning rule's real state.
	res := Result{Agent: c.Manifest.ID, State: StateUnknown}
	best := -1
	for i, rule := range c.Manifest.Rules {
		text := Region(in, rule.Region)
		matched := gateMatches(&c.gates[i], text, strings.ToLower(text))
		res.Evaluated = append(res.Evaluated, Evaluated{
			ID: rule.ID, Priority: rule.Priority, Region: rule.Region,
			State: rule.EffectiveState(), Matched: matched,
			RegionBytes: len(text), RegionPreview: preview(text),
		})
		if !matched {
			continue
		}
		if best >= 0 && c.Manifest.Rules[best].Priority >= rule.Priority {
			continue
		}
		best = i
	}
	if best < 0 {
		return res
	}
	r := c.Manifest.Rules[best]
	st := r.EffectiveState()
	res.Matched = true
	res.State = st
	res.RuleID = r.ID
	res.Priority = r.Priority
	res.Region = r.Region
	res.Skip = r.SkipStateUpdate
	res.VisibleIdle = r.VisibleIdle && st == StateIdle
	res.VisibleBlocker = r.VisibleBlocker && st == StateBlocked
	res.VisibleWorking = r.VisibleWorking && st == StateWorking
	return res
}

func gateMatches(g *compiledGate, text, lower string) bool {
	for _, needle := range g.contains {
		if !strings.Contains(lower, needle) {
			return false
		}
	}
	for _, re := range g.regex {
		if !re.MatchString(text) {
			return false
		}
	}
	if len(g.lineRegex) > 0 {
		ls := strings.Split(text, "\n")
		for _, re := range g.lineRegex {
			hit := false
			for _, l := range ls {
				if re.MatchString(l) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		}
	}
	for i := range g.all {
		if !gateMatches(&g.all[i], text, lower) {
			return false
		}
	}
	if len(g.any) > 0 {
		hit := false
		for i := range g.any {
			if gateMatches(&g.any[i], text, lower) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for i := range g.not {
		if gateMatches(&g.not[i], text, lower) {
			return false
		}
	}
	return true
}
