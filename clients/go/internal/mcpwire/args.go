package mcpwire

import (
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// preParseDepth is well under the nesting where json.loads would raise
// RecursionError.
const preParseDepth = 900

func constant(v any) func() any { return func() any { return v } }

var (
	str     = pmodel.Str{}
	optStrS = pmodel.Nullable{Inner: pmodel.Str{}}
	dict    = pmodel.Dict{Val: pmodel.Any{}}
)

// Args is each tool's argument model (FastMCP's <tool>Arguments), with
// the fields whose annotation is exactly str: those are never pre-parsed.
var Args = map[string]*pmodel.Model{
	"envelope_for": {Name: "envelope_forArguments", Fields: []pmodel.Field{
		{Name: "task", Schema: str, Required: true},
		{Name: "stakes", Schema: str, Default: constant("medium")},
		{Name: "context", Schema: optStrS, Default: constant(nil)},
	}},
	"find_pathway": {Name: "find_pathwayArguments", Fields: []pmodel.Field{
		{Name: "task", Schema: str, Required: true},
	}},
	"recall": {Name: "recallArguments", Fields: []pmodel.Field{
		{Name: "task", Schema: str, Required: true},
		{Name: "envelope", Schema: dict, Required: true},
		{Name: "z3_timeout_ms", Schema: pmodel.Int{}, Default: constant(pyjson.Int{Text: "500"})},
	}},
	"recall_answer": {Name: "recall_answerArguments", Fields: []pmodel.Field{
		{Name: "task", Schema: str, Required: true},
		{Name: "current_ground_hash", Schema: optStrS, Default: constant(nil)},
		{Name: "max_age_seconds", Schema: pmodel.Float{}, Default: constant(604800.0)},
	}},
	"verify_plan": {Name: "verify_planArguments", Fields: []pmodel.Field{
		{Name: "plan", Schema: dict, Required: true},
		{Name: "envelope", Schema: dict, Required: true},
	}},
	"verify_completed_step": {Name: "verify_completed_stepArguments", Fields: []pmodel.Field{
		{Name: "step", Schema: dict, Required: true},
		{Name: "envelope", Schema: dict, Required: true},
	}},
	"list_pathways": {Name: "list_pathwaysArguments"},
	"pathway_stats": {Name: "pathway_statsArguments"},
	"run_plan": {Name: "run_planArguments", Fields: []pmodel.Field{
		{Name: "plan", Schema: dict, Required: true},
		{Name: "envelope", Schema: dict, Required: true},
		{Name: "dry_run", Schema: pmodel.Bool{}, Default: constant(true)},
	}},
	"receipts_for_run": {Name: "receipts_for_runArguments", Fields: []pmodel.Field{
		{Name: "run_id", Schema: str, Required: true},
	}},
	"recent_runs": {Name: "recent_runsArguments", Fields: []pmodel.Field{
		{Name: "limit", Schema: pmodel.Int{}, Default: constant(pyjson.Int{Text: "20"})},
	}},
	"delegate": {Name: "delegateArguments", Fields: []pmodel.Field{
		{Name: "path", Schema: str, Required: true},
		{Name: "question", Schema: str, Required: true},
		{Name: "mode", Schema: str, Default: constant("bulk_read")},
	}},
}

// strOnly are the fields annotated exactly str: pre_parse_json leaves
// their values alone.
var strOnly = map[string]bool{"task": true, "stakes": true, "run_id": true, "path": true, "question": true,
	"mode": true}

// PreParse is FuncMetadata.pre_parse_json: a str value given for a field
// not annotated str is read with json.loads, and kept when that gives a
// list, a dict or None. ok is false when a value holds text json.loads
// reads into a form this binary does not take (nesting deeper than it
// reads).
func PreParse(m *pmodel.Model, args *pyjson.Object) (*pyjson.Object, bool) {
	out := pyjson.NewObjectCap(args.Len())
	for _, k := range args.Keys() {
		v := args.Value(k)
		out.Set(k, v)
		s, isStr := v.(string)
		if !isStr || strOnly[k] || !hasField(m, k) {
			continue
		}
		parsed, err := pyjson.LoadsPy(s, preParseDepth)
		if err != nil {
			if err.TooDeep {
				return nil, false
			}
			continue
		}
		switch parsed.(type) {
		case string, pyjson.Int, pyjson.Float, bool:
			continue
		}
		out.Set(k, parsed)
	}
	return out, true
}

func hasField(m *pmodel.Model, k string) bool {
	for _, f := range m.Fields {
		if f.Name == k {
			return true
		}
	}
	return false
}
