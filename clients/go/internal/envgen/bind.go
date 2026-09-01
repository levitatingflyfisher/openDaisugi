package envgen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"daisugi-verify/internal/distill"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// This file is pathway_bind and pathway_params.apply_bindings: fill a typed
// pathway's holes for a new task with one model call, then verify the
// bound plan against the caller's envelope. Any failure gives the frozen
// template.

// Bindings is pathway_bind.Bindings: a value for each hole.
var Bindings = &pmodel.Model{Name: "Bindings", Fields: []pmodel.Field{
	{Name: "values", Schema: pmodel.Dict{Val: pmodel.Str{}}, Required: true},
}}

func params(pathway *pyjson.Object) []*pyjson.Object {
	var out []*pyjson.Object
	for _, p := range listOf(pathway.Value("parameters")) {
		out = append(out, obj(p))
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// bindPrompt is _user_prompt.
func bindPrompt(ps []*pyjson.Object, task string) string {
	holes := make([]string, len(ps))
	heads := map[string]bool{}
	for i, p := range ps {
		holes[i] = fmt.Sprintf("- %s: fills the `%s` of a step whose head is `%s`; past values: %s",
			str(p.Value("name")), str(p.Value("field")), str(p.Value("head")), pystr.ReprList(strList(p.Value("observed"))))
		heads[str(p.Value("head"))] = true
	}
	hs := make([]string, 0, len(heads))
	for h := range heads {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	return "Task: " + task + "\n\n" +
		"Holes to fill (keep each head `" + strings.Join(hs, "`, `") + "` exactly):\n" + strings.Join(holes, "\n") + "\n\n" +
		"Return {name: concrete value} for every hole."
}

// isStrField reports whether a step type declares field, and whether it
// holds a string (so a bound string keeps the plan well formed).
func isStrField(stepType, field string) (declared, isStr bool) {
	m := pmodel.StepTypes[stepType]
	if m == nil {
		return false, false
	}
	for _, f := range m.Fields {
		if f.Name != field {
			continue
		}
		switch s := f.Schema.(type) {
		case pmodel.Str:
			return true, true
		case pmodel.Nullable:
			_, ok := s.Inner.(pmodel.Str)
			return true, ok
		}
		return true, false
	}
	return false, false
}

// ApplyBindings is apply_bindings: the bound plan on a deep copy, or nil
// when a hole is unbound or a value would change its capability head. A
// binding into a field that does not hold a string (or that the step does
// not declare) raises ValueError, as the oracle does, and ends in the
// template (K1-4).
func ApplyBindings(template *pyjson.Object, ps []*pyjson.Object, values *pyjson.Object) (*pyjson.Object, error) {
	plan := DeepCopy(template)
	steps, err := tracejournal.TopoOrderErr(plan)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		name := str(p.Value("name"))
		v, has := values.Get(name)
		idx := int(bigInt(p.Value("step_index")).Int64())
		if !has || idx >= len(steps) {
			return nil, nil
		}
		if idx < 0 {
			// A negative index counts from the end, as a list does.
			idx += len(steps)
			if idx < 0 {
				return nil, &PyError{"IndexError", "list index out of range"}
			}
		}
		value := str(v)
		step := steps[idx]
		st := str(step.Value("type"))
		head, ok := distill.CapabilityHead(st, value)
		if !ok || head != str(p.Value("head")) {
			return nil, nil
		}
		field := str(p.Value("field"))
		declared, isStr := isStrField(st, field)
		if !declared || !isStr {
			return nil, &PyError{"ValueError", fmt.Sprintf("the %s step has no string field %s", st, pystr.Repr(field))}
		}
		step.Set(field, value)
	}
	return plan, nil
}

// verifyPlan is the verify PlanVerifies runs; a test swaps it to make a
// Z3 check answer unknown.
var verifyPlan = verify.Verify

// PlanVerifies is verify(plan, envelope).ok, failing closed: a Z3 check
// that did not finish is a failure here, where the oracle's lenient
// verify keeps it as a warning (GW-11, K1-3).
func PlanVerifies(plan *pyjson.Object, env verify.Envelope, z3ms int) bool {
	p, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
	if err != nil {
		return false
	}
	r := verifyPlan(p, env, verify.VerifyOptions{Z3TimeoutMs: z3ms})
	return r.OK && len(r.Timeouts) == 0
}

// Bind is bind_parameters: a verified concrete plan for task, or the
// frozen template. A nil client is no model, which gives the template.
func Bind(c *llm.Client, pathway *pyjson.Object, task string, env verify.Envelope, model string, z3ms int) *pyjson.Object {
	template := DeepCopy(obj(pathway.Value("plan_template")))
	ps := params(pathway)
	if len(ps) == 0 || c == nil {
		return template
	}
	if c.Preflight(model) != nil {
		return template
	}
	resp, err := c.Structured(llm.Call{Model: model, System: bindSystem, User: bindPrompt(ps, task),
		Response: llm.Schema{Name: "Bindings", Model: Bindings}, MaxRetries: 2})
	if err != nil {
		return template
	}
	values, ok := resp.Value("values").(*pyjson.Object)
	if !ok {
		return template
	}
	bound, err := ApplyBindings(template, ps, values)
	if err != nil || bound == nil {
		return template
	}
	if !PlanVerifies(bound, env, z3ms) {
		return template
	}
	return bound
}
