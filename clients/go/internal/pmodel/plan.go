package pmodel

import (
	"fmt"
	"strings"

	"daisugi-verify/internal/pystr"

	"daisugi-verify/internal/pyjson"
)

// This file holds the oracle's plan and pathway models: ActionPlan with
// its registered step types (opendaisugi/models.py), and CompiledPathway
// with PathwayParameter (opendaisugi/pathway.py). Field order is the
// order pydantic dumps them in: StepBase's fields, then the subclass's.

// FloatRange is float with ge and le bounds.
type FloatRange struct{ Lo, Hi float64 }

func (r FloatRange) validate(v any, loc []any, mode Mode) (any, []Err) {
	out, errs := Float{}.validate(v, loc, mode)
	if errs != nil {
		return nil, errs
	}
	f := out.(float64)
	if !(f >= r.Lo) {
		return nil, one(loc, "greater_than_equal", fmt.Sprintf("Input should be greater than or equal to %v", r.Lo), v)
	}
	if !(f <= r.Hi) {
		return nil, one(loc, "less_than_equal", fmt.Sprintf("Input should be less than or equal to %v", r.Hi), v)
	}
	return f, nil
}

// StepTypes is models.STEP_TYPE_REGISTRY as the CLI process holds it.
var StepTypes = map[string]*Model{}

// StepTypeOrder is the registry's keys in registration order.
var StepTypeOrder []string

func init() {
	float3 := Tuple{Items: []Schema{Float{}, Float{}, Float{}}}
	float4 := Tuple{Items: []Schema{Float{}, Float{}, Float{}, Float{}}}
	req := func(name string, s Schema) Field { return Field{Name: name, Schema: s, Required: true} }
	def := func(name string, s Schema, d func() any) Field { return Field{Name: name, Schema: s, Default: d} }
	unit := FloatRange{0, 1}
	defs := []struct {
		tag, name string
		fields    []Field
	}{
		{"shell", "ShellStep", []Field{req("command", Str{})}},
		{"file_read", "FileReadStep", []Field{req("path", Str{})}},
		{"file_write", "FileWriteStep", []Field{req("path", Str{}), req("content", Str{})}},
		{"network", "NetworkStep", []Field{req("url", Str{}), def("method", Literal{[]string{"GET"}}, constant("GET")),
			def("headers", Dict{Str{}}, emptyDict)}},
		{"joint_move", "JointMoveStep", []Field{req("joint_targets", Dict{Float{}}), def("duration_s", Float{}, constant(1.0)),
			def("velocity_scale", unit, constant(1.0))}},
		{"cartesian_move", "CartesianMoveStep", []Field{req("target_position", float3),
			def("target_orientation", Nullable{float4}, constant(nil)), def("duration_s", Float{}, constant(1.0)),
			def("velocity_scale", unit, constant(1.0))}},
		{"gripper", "GripperStep", []Field{req("action", Literal{[]string{"open", "close"}}), def("hold_s", Float{}, constant(0.2))}},
		{"sim_reset", "SimulationResetStep", []Field{def("seed", Nullable{Int{}}, constant(nil))}},
		{"vla", "VLAStep", []Field{req("task", Str{}), def("target_pose", Nullable{float3}, constant(nil)),
			def("max_actions", Int{}, constant(pyjson.Int{Text: "50"})), def("timeout_s", Float{}, constant(5.0))}},
		{"task", "TaskStep", []Field{req("prompt", Str{})}},
		{"agentic", "AgenticStep", []Field{req("prompt", Str{}), req("workspace", Str{}), def("tools", List{Str{}}, emptyList),
			def("max_turns", Nullable{Int{}}, constant(nil))}},
		{"skill", "SkillStep", []Field{req("skill_id", Str{}), def("skill_input", Dict{Any{}}, emptyDict),
			def("contract_envelope", Nullable{Envelope}, constant(nil))}},
		{"mcp", "MCPStep", []Field{req("server", Str{}), req("tool", Str{}), def("arguments", Dict{Any{}}, emptyDict)}},
	}
	for _, d := range defs {
		fields := []Field{
			req("id", Str{}),
			def("depends_on", List{Str{}}, emptyList),
			def("metadata", Dict{Any{}}, emptyDict),
			def("postcondition", Nullable{Postcondition}, constant(nil)),
			def("preferred_model", Nullable{Str{}}, constant(nil)),
			def("type", Literal{[]string{d.tag}}, constant(d.tag)),
		}
		StepTypes[d.tag] = &Model{Name: d.name, Fields: append(fields, d.fields...)}
		StepTypeOrder = append(StepTypeOrder, d.tag)
	}
}

// UnreadableStep is the error type of a step this port does not read.
const UnreadableStep = "unreadable_step"

// Steps is ActionPlan.steps: list[Any] with coerce_step before and the
// StepBase check after. A dict whose "type" is registered validates as
// that step model in Python mode (subclass.model_validate). A str item is
// decode_dict_text's case; callers refuse plans that hold one.
type Steps struct {
	// Decode reads a str item as coerce_step does (decode_dict_text), for
	// a model's reply. Unset, a str item is UnreadableStep.
	Decode bool
}

func (st Steps) validate(v any, loc []any, mode Mode) (any, []Err) {
	xs, ok := v.([]any)
	if !ok {
		msg := "Input should be a valid list"
		if mode == JSON {
			msg = "Input should be a valid array"
		}
		return nil, one(loc, "list_type", msg, v)
	}
	if st.Decode {
		return validateReplySteps(xs, loc, v)
	}
	return validateSteps(xs, loc, v)
}

// validateSteps is ActionPlan.steps read from stored input (Decode
// unset). Each item is checked in turn, and the first that fails is
// the error: a str is UnreadableStep, an item with no registered type
// is the after-validator's error, and a registered step's own field
// errors take the field's place, with no item index. The oracle's
// coerce_step goes past an item with no registered type, so for such
// an item before an invalid step the oracle names the invalid step and
// this names the item. Rust's validate_steps checks every item's type
// before any fields, so it names the item in both orders. GD-R-6
// covers replies only.
func validateSteps(xs []any, loc []any, v any) (any, []Err) {
	out := make([]any, 0, len(xs))
	for _, x := range xs {
		if _, isStr := x.(string); isStr {
			// coerce_step decodes such a string as JSON or as a Python
			// literal; the store reader does not, and says so.
			return nil, one(loc, UnreadableStep, "a plan step is a string", v)
		}
		o, isObj := x.(*pyjson.Object)
		var m *Model
		if isObj {
			if tag, has := o.Get("type"); has {
				if s, isStr := tag.(string); isStr {
					m = StepTypes[s]
				}
			}
		}
		if m == nil {
			// The after-validator's ValueError, for the whole field.
			return nil, one(loc, "value_error", fmt.Sprintf("Value error, ActionPlan.steps item %s is not a StepBase subclass. "+
				"If authoring a custom step type, register it with @opendaisugi.step_type.", Repr(x)), v)
		}
		y, errs := m.fields(o, loc, Python)
		if errs != nil {
			// model_validate raised inside the validator: its errors
			// take the field's place, with no item index.
			return nil, errs
		}
		out = append(out, y)
	}
	return out, nil
}

// validateReplySteps is ActionPlan.steps as a model's reply is read
// (Decode set). coerce_step runs over every item first: a str that
// decodes to a dict of a registered type is that dict, and a dict of a
// registered type validates as that step right away (its errors take the
// field's place, with no item index); an item with no registered type
// is only noted, not yet an error. Once every item is checked this way,
// the after-validator names the first noted item (GD-R-6): an invalid
// registered step can be the error even after an item that is no step.
func validateReplySteps(xs []any, loc []any, v any) (any, []Err) {
	type outcome struct {
		val    any
		item   any // the original item, when it is not a step
		isStep bool
	}
	outs := make([]outcome, 0, len(xs))
	for _, x := range xs {
		item := x
		if text, isStr := x.(string); isStr {
			if d, ok := DecodeDictText(text); ok {
				if tag, has := d.Get("type"); has {
					if s, isStr := tag.(string); isStr && StepTypes[s] != nil {
						item = d
					}
				}
			}
		}
		o, isObj := item.(*pyjson.Object)
		var m *Model
		if isObj {
			if tag, has := o.Get("type"); has {
				if s, isStr := tag.(string); isStr {
					m = StepTypes[s]
				}
			}
		}
		if m != nil && isObj {
			y, errs := m.fields(o, loc, Python)
			if errs != nil {
				// model_validate raised inside the validator: its errors
				// take the field's place, with no item index.
				return nil, errs
			}
			outs = append(outs, outcome{val: y, isStep: true})
			continue
		}
		outs = append(outs, outcome{item: item})
	}
	out := make([]any, 0, len(outs))
	for _, r := range outs {
		if r.isStep {
			out = append(out, r.val)
			continue
		}
		// The after-validator's ValueError, for the whole field: the
		// first item that is no step, once every item has been through
		// coerce_step.
		return nil, one(loc, "value_error", fmt.Sprintf("Value error, ActionPlan.steps item %s is not a StepBase subclass. "+
			"If authoring a custom step type, register it with @opendaisugi.step_type.", Repr(r.item)), v)
	}
	return out, nil
}

// ActionPlan is models.ActionPlan. An absent id is a fresh plan_<hex8>.
var ActionPlan = &Model{Name: "ActionPlan", Finite: true, Fields: []Field{
	{Name: "id", Schema: Str{}, Default: func() any { return "plan_" + newEnvelopeID().(string)[4:] }},
	{Name: "source", Schema: Str{}, Required: true},
	{Name: "task", Schema: Str{}, Required: true},
	{Name: "steps", Schema: Steps{}, Required: true},
}}

// ActionPlanReply is ActionPlan as a model's reply is read: a step given
// as a string is decoded as coerce_step decodes it.
var ActionPlanReply = &Model{Name: "ActionPlan", Finite: true, Fields: []Field{
	ActionPlan.Fields[0], ActionPlan.Fields[1], ActionPlan.Fields[2],
	{Name: "steps", Schema: Steps{Decode: true}, Required: true},
}}

// DecodeDictText is models.decode_dict_text: text holding one dict, as
// JSON or as a Python literal (the subset pyjson.LiteralEval reads).
func DecodeDictText(text string) (*pyjson.Object, bool) {
	if pystr.Len(text) > 65_536 {
		return nil, false
	}
	body := pystr.Strip(text)
	if !strings.HasPrefix(body, "{") {
		return nil, false
	}
	if v, err := pyjson.LoadsPy(body, 900); err == nil {
		if o, isObj := v.(*pyjson.Object); isObj {
			return o, true
		}
	}
	if v, ok := pyjson.LiteralEval(body); ok {
		if o, isObj := v.(*pyjson.Object); isObj {
			return o, true
		}
	}
	return nil, false
}

// PathwayParameter is pathway.PathwayParameter.
var PathwayParameter = &Model{Name: "PathwayParameter", Fields: []Field{
	{Name: "name", Schema: Str{}, Required: true},
	{Name: "step_index", Schema: Int{}, Required: true},
	{Name: "step_id", Schema: Str{}, Required: true},
	{Name: "field", Schema: Str{}, Required: true},
	{Name: "head", Schema: Str{}, Required: true},
	{Name: "observed", Schema: List{Str{}}, Default: emptyList},
}}

// CompiledPathway is pathway.CompiledPathway.
var CompiledPathway = &Model{Name: "CompiledPathway", Fields: []Field{
	{Name: "id", Schema: Str{}, Required: true},
	{Name: "task_description", Schema: Str{}, Required: true},
	{Name: "task_embedding", Schema: List{Float{}}, Required: true},
	{Name: "embedding_model", Schema: Str{}, Default: constant("")},
	{Name: "embedding_model_version", Schema: Str{}, Default: constant("")},
	{Name: "envelope", Schema: Envelope, Required: true},
	{Name: "plan_template", Schema: ActionPlan, Required: true},
	{Name: "source_trace_ids", Schema: List{Str{}}, Required: true},
	{Name: "version", Schema: Int{}, Default: constant(pyjson.Int{Text: "1"})},
	{Name: "hit_count", Schema: Int{}, Default: constant(pyjson.Int{Text: "0"})},
	{Name: "distilled_at", Schema: Float{}, Required: true},
	{Name: "last_activation_at", Schema: Float{}, Default: constant(0.0)},
	{Name: "failure_count", Schema: Int{}, Default: constant(pyjson.Int{Text: "0"})},
	{Name: "activation_count", Schema: Int{}, Default: constant(pyjson.Int{Text: "0"})},
	{Name: "structure_signature", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "parameters", Schema: List{PathwayParameter}, Default: emptyList},
}}

// ValidateObject validates o against m's fields, as model_validate does once the
// input is known to be a dict.
func (m *Model) ValidateObject(o *pyjson.Object, mode Mode) (*pyjson.Object, *ValidationError) {
	out, errs := m.fields(o, nil, mode)
	if len(errs) > 0 {
		return nil, &ValidationError{Title: m.Name, Errs: errs}
	}
	return out.(*pyjson.Object), nil
}
