package envgen

import (
	"strings"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
)

// This file is compose: a frozen pathway as a skill a plan's SkillStep
// calls. Safety needs nothing new: a SkillStep that names a pathway
// carries the pathway's envelope as its contract_envelope, and verify
// proves the caller's envelope subsumes it.

// Executor runs one step (orchestration_executors' executor.run): its
// stdout, or the error it raises.
type Executor interface {
	Run(step *pyjson.Object, timeoutS, maxOutputBytes int) (string, error)
}

// Handler is a SkillHandler: a skill step in, its output out.
type Handler func(step *pyjson.Object) (string, error)

// SkillHandler is pathway_skill_handler: the pathway's plan run step by
// step in topological order, each output tagged with its step.
func SkillHandler(pathway *pyjson.Object, executors map[string]Executor, timeoutS, maxOutputBytes int) Handler {
	return func(*pyjson.Object) (string, error) {
		steps, err := tracejournal.TopoOrderErr(obj(pathway.Value("plan_template")))
		if err != nil {
			return "", err
		}
		var outputs []string
		for _, s := range steps {
			t := str(s.Value("type"))
			ex := executors[t]
			if ex == nil {
				return "", &PyError{"RuntimeError", "no executor for composed step type " + pystr.Repr(t)}
			}
			out, err := ex.Run(s, timeoutS, maxOutputBytes)
			if err != nil {
				return "", err
			}
			outputs = append(outputs, "["+str(s.Value("id"))+"·"+t+"] "+out)
		}
		return strings.Join(outputs, "\n"), nil
	}
}

// HandlersFor is pathway_skill_handlers_for: a handler for each named
// pathway that exists and is frozen. A typed pathway is not composed.
func HandlersFor(store *pathways.Store, skillIDs []string, executors map[string]Executor) (map[string]Handler, error) {
	out := map[string]Handler{}
	for _, id := range skillIDs {
		row, err := store.Get(id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			continue
		}
		p, err := pathways.FromRow(row)
		if err != nil {
			return nil, err
		}
		if len(listOf(p.Obj.Value("parameters"))) > 0 {
			continue
		}
		out[id] = SkillHandler(p.Obj, executors, 30, 65_536)
	}
	return out, nil
}

// ContractEnvelopes is pathway_contract_envelopes: each frozen pathway's
// envelope by its id.
func ContractEnvelopes(store *pathways.Store) (map[string]*pyjson.Object, error) {
	ps, err := store.ReadAll(nil)
	if err != nil {
		return nil, err
	}
	out := map[string]*pyjson.Object{}
	for _, p := range ps {
		if len(listOf(p.Obj.Value("parameters"))) == 0 {
			out[p.ID()] = obj(p.Obj.Value("envelope"))
		}
	}
	return out, nil
}
