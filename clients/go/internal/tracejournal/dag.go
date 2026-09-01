package tracejournal

import (
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Steps is a plan's steps (model_dump() objects).
func Steps(plan *pyjson.Object) []*pyjson.Object {
	xs := plan.Value("steps").([]any)
	out := make([]*pyjson.Object, len(xs))
	for i, x := range xs {
		out[i] = x.(*pyjson.Object)
	}
	return out
}

// TopoOrder is dag.topological_order: networkx's topological_sort of the
// plan's graph (nodes in step order, then each unknown dependency as its
// first edge adds it; each node's successors in the order their edges were
// added), taken generation by generation. ok is false where Python raises:
// a cycle (NetworkXUnfeasible) or a node that is not a step (KeyError).
func TopoOrder(plan *pyjson.Object) (order []*pyjson.Object, ok bool) {
	order, err := TopoOrderErr(plan)
	return order, err == nil
}

// PyError is an exception the oracle raises: its str().
type PyError struct{ Type, Msg string }

func (e *PyError) Error() string { return e.Msg }

// TopoOrderErr is TopoOrder with the exception topological_order raises:
// ValueError for a cycle, KeyError for a dependency that is not a step.
func TopoOrderErr(plan *pyjson.Object) ([]*pyjson.Object, error) {
	var order []*pyjson.Object
	steps := Steps(plan)
	byID := map[string]*pyjson.Object{}
	var nodes []string
	index := map[string]int{}
	addNode := func(n string) {
		if _, seen := index[n]; !seen {
			index[n] = len(nodes)
			nodes = append(nodes, n)
		}
	}
	for _, s := range steps {
		id := s.Value("id").(string)
		byID[id] = s
		addNode(id)
	}
	succ := map[string][]string{}
	hasEdge := map[[2]string]bool{}
	indeg := map[string]int{}
	for _, s := range steps {
		to := s.Value("id").(string)
		for _, d := range s.Value("depends_on").([]any) {
			from := d.(string)
			addNode(from)
			if hasEdge[[2]string{from, to}] {
				continue
			}
			hasEdge[[2]string{from, to}] = true
			succ[from] = append(succ[from], to)
			indeg[to]++
		}
	}
	var zero []string
	for _, n := range nodes {
		if indeg[n] == 0 {
			zero = append(zero, n)
		}
	}
	remaining := map[string]int{}
	for n, d := range indeg {
		if d > 0 {
			remaining[n] = d
		}
	}
	var ids []string
	for len(zero) > 0 {
		gen := zero
		zero = nil
		for _, n := range gen {
			for _, c := range succ[n] {
				remaining[c]--
				if remaining[c] == 0 {
					zero = append(zero, c)
					delete(remaining, c)
				}
			}
		}
		ids = append(ids, gen...)
	}
	if len(remaining) > 0 {
		return nil, &PyError{"ValueError", "Plan has a cycle; run verify(plan, envelope) before supervising"}
	}
	for _, id := range ids {
		s, isStep := byID[id]
		if !isStep {
			return nil, &PyError{"KeyError", pystr.Repr(id)}
		}
		order = append(order, s)
	}
	return order, nil
}

// StructureSignature is distiller.plan_structure_signature: the step
// types in topological order joined by an arrow. ok is false where Python
// raises (and its callers record None).
func StructureSignature(plan *pyjson.Object) (string, bool) {
	order, ok := TopoOrder(plan)
	if !ok {
		return "", false
	}
	types := make([]string, len(order))
	for i, s := range order {
		types[i] = s.Value("type").(string)
	}
	return strings.Join(types, "→"), true
}

// RefinementRecord is refinement.RefinementRecord, as far as this binary
// reads it: a step given as a dict of a registered type.
var RefinementRecord = &pmodel.Model{Name: "RefinementRecord", Fields: []pmodel.Field{
	{Name: "step", Schema: pmodel.StepField{}, Required: true},
	{Name: "violations", Schema: pmodel.List{Elem: pmodel.Violation}, Required: true},
	{Name: "z3_counterexample", Schema: pmodel.Nullable{Inner: pmodel.Dict{Val: pmodel.Any{}}}, Required: true},
	{Name: "envelope_id", Schema: pmodel.Str{}, Required: true},
	{Name: "fallback_action", Schema: pmodel.Literal{Choices: []string{"halted", "recomputed"}}, Required: true},
	{Name: "recomputed_step", Schema: pmodel.StepField{Nullable: true}, Default: func() any { return nil }},
	{Name: "recomputed_verification", Schema: pmodel.Nullable{Inner: pmodel.VerificationResult}, Default: func() any { return nil }},
	{Name: "timestamp", Schema: pmodel.Float{}, Required: true},
	{Name: "cache_key", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: func() any { return nil }},
}}
