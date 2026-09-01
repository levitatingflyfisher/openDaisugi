package pmodel

import "daisugi-verify/internal/pyjson"

// Violation is models.Violation.
var Violation = &Model{Name: "Violation", Fields: []Field{
	{Name: "stage", Schema: Str{}, Required: true},
	{Name: "message", Schema: Str{}, Required: true},
	{Name: "detail", Schema: Dict{Any{}}, Default: emptyDict},
	{Name: "suggested_remediation", Schema: Nullable{Str{}}, Default: constant(nil)},
}}

// VerificationResult is models.VerificationResult.
var VerificationResult = &Model{Name: "VerificationResult", Fields: []Field{
	{Name: "ok", Schema: Bool{}, Required: true},
	{Name: "violations", Schema: List{Violation}, Default: emptyList},
	{Name: "warnings", Schema: List{Str{}}, Default: emptyList},
	{Name: "envelope_id", Schema: Str{}, Required: true},
	{Name: "plan_id", Schema: Str{}, Required: true},
	{Name: "duration_ms", Schema: Float{}, Required: true},
	{Name: "client", Schema: Str{}, Default: constant("python")},
	{Name: "fallback", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "client_verdict", Schema: Nullable{Bool{}}, Default: constant(nil)},
}}

// StepField is a step held as Any with coerce_step before it and the
// StepBase check after (RefinementRecord.step): a dict of a registered
// type validates as that step. A string, another shape, or a step that
// fails its own validation is UnreadableStep: this port does not word
// those errors, and callers refuse.
type StepField struct{ Nullable bool }

func (s StepField) validate(v any, loc []any, mode Mode) (any, []Err) {
	if v == nil && s.Nullable {
		return nil, nil
	}
	if o, ok := v.(*pyjson.Object); ok {
		if tag, has := o.Get("type"); has {
			if name, isStr := tag.(string); isStr {
				if m := StepTypes[name]; m != nil {
					out, errs := m.fields(o, nil, Python)
					if errs == nil {
						return out, nil
					}
				}
			}
		}
	}
	return nil, one(loc, UnreadableStep, "a step this binary does not read", v)
}
