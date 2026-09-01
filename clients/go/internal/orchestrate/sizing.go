// Package orchestrate is the oracle's orchestrator
// (opendaisugi/orchestrator.py, decomposer.py, model_sizer.py, budget.py,
// synthesizer.py and delegating_executor.py): a prompt decomposed by a
// model into a verified plan, each step sized to the cheapest capable
// model within a token budget, the plan run under the supervisor, and the
// step outputs put together into one answer.
package orchestrate

import (
	"math"

	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pyjson"
)

// Rung is model_sizer.ModelRung.
type Rung struct {
	Name          string
	Model         string
	MaxDifficulty float64
	EstTokens     int64
}

// Ladder is model_sizer.ModelLadder.
type Ladder []Rung

// BuildLadder is model_sizer.build_ladder: a local rung only when a local
// model is named, then cheap and frontier.
func BuildLadder(local string) Ladder {
	var l Ladder
	if local != "" {
		l = append(l, Rung{"local", local, 0.35, 1200})
	}
	return append(l, Rung{"cheap", gateway.DefaultCheapModel, 0.65, 2000},
		Rung{"frontier", gateway.DefaultFrontierModel, 1.0, 4500})
}

func (l Ladder) forDifficulty(d float64) Rung {
	for _, r := range l {
		if d <= r.MaxDifficulty {
			return r
		}
	}
	return l[len(l)-1]
}

func (l Ladder) forModel(model string) (Rung, bool) {
	for _, r := range l {
		if r.Model == model {
			return r, true
		}
	}
	return Rung{}, false
}

func (l Ladder) cheapestAffordable(remaining float64) (Rung, bool) {
	for _, r := range l {
		if float64(r.EstTokens) <= remaining {
			return r, true
		}
	}
	return Rung{}, false
}

// Sizing is model_sizer.StepSizing.
type Sizing struct {
	StepID     string
	Difficulty float64
	Tier       string
	Model      string
	EstTokens  int64
	Downgraded bool
	Affordable bool
}

// Dump is dataclasses.asdict(sizing).
func (s Sizing) Dump() *pyjson.Object {
	return pyjson.NewObject().Set("step_id", s.StepID).Set("difficulty", s.Difficulty).Set("tier", s.Tier).
		Set("model", s.Model).Set("est_tokens", pyjson.Int{Text: itoa(s.EstTokens)}).
		Set("downgraded", s.Downgraded).Set("affordable", s.Affordable)
}

var stepTypeBase = map[string]float64{
	"task": 0.3, "shell": 0.1, "file_read": 0.05, "file_write": 0.1, "network": 0.1, "skill": 0.0, "mcp": 0.1,
}

func pyMin(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

func pyMax(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

// StepDifficulty is model_sizer.estimate_step_difficulty.
func StepDifficulty(step *pyjson.Object) float64 {
	kind, _ := step.Value("type").(string)
	base, ok := stepTypeBase[kind]
	if !ok {
		base = 0.3
	}
	if kind == "task" {
		p, _ := step.Value("prompt").(string)
		base = pyMax(base, gateway.EstimateDifficulty(p))
	}
	deps, _ := step.Value("depends_on").([]any)
	base += pyMin(float64(len(deps))*0.05, 0.2)
	return pyMin(base, 1.0)
}

// Remaining is what a budget has left: +Inf when unlimited.
type Remaining interface{ Remaining() float64 }

// SizeStep is model_sizer.size_step.
func SizeStep(step *pyjson.Object, l Ladder, budget Remaining, target string) Sizing {
	d := StepDifficulty(step)
	chosen, ok := Rung{}, false
	if target != "" {
		chosen, ok = l.forModel(target)
	}
	if !ok {
		chosen = l.forDifficulty(d)
	}
	downgraded, affordable := false, true
	if budget != nil {
		rem := budget.Remaining()
		if float64(chosen.EstTokens) > rem {
			if cheaper, ok := l.cheapestAffordable(rem); ok {
				chosen, downgraded = cheaper, true
			} else {
				cheapest := l[0]
				downgraded = chosen != cheapest
				chosen, affordable = cheapest, false
			}
		}
	}
	id, isStr := step.Value("id").(string)
	if !isStr {
		id = "?"
	}
	return Sizing{id, d, chosen.Name, chosen.Model, chosen.EstTokens, downgraded, affordable}
}

// SizePlan is model_sizer.size_plan with no budget.
func SizePlan(steps []*pyjson.Object, l Ladder) []Sizing {
	out := make([]Sizing, len(steps))
	for i, s := range steps {
		out[i] = SizeStep(s, l, nil, "")
	}
	return out
}

var inf = math.Inf(1)
