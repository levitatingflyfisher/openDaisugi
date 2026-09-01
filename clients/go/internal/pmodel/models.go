package pmodel

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"daisugi-verify/internal/pyjson"
)

// This file holds the oracle's models the gate validates: Envelope and
// what it holds (opendaisugi/models.py), and the predicate Expression
// union (opendaisugi/predicate.py).

func constant(v any) func() any { return func() any { return v } }

func emptyList() any { return []any{} }

func emptyDict() any { return pyjson.NewObject() }

var float3 = Tuple{Items: []Schema{Float{}, Float{}, Float{}}}
var box = Tuple{Items: []Schema{float3, float3}}

// Permission is models.Permission.
var Permission = &Model{Name: "Permission", Fields: []Field{
	{Name: "file_read", Schema: List{Str{}}, Default: emptyList},
	{Name: "file_write", Schema: List{Str{}}, Default: emptyList},
	{Name: "network", Schema: Bool{}, Default: constant(false)},
	{Name: "network_hosts", Schema: List{Str{}}, Default: emptyList},
	{Name: "shell", Schema: Bool{}, Default: constant(false)},
	{Name: "shell_allowlist", Schema: List{Str{}}, Default: emptyList},
	{Name: "shell_allow_decomposition", Schema: Bool{}, Default: constant(false)},
	{Name: "mcp_allowlist", Schema: List{Str{}}, Default: emptyList},
	{Name: "custom_step_allowlist", Schema: List{Str{}}, Default: emptyList},
	{Name: "max_execution_time_s", Schema: Int{}, Default: constant(pyjson.Int{Text: "30"})},
	{Name: "max_output_size_mb", Schema: Int{}, Default: constant(pyjson.Int{Text: "10"})},
	{Name: "workspace_bounds", Schema: Nullable{box}, Default: constant(nil)},
	{Name: "obstacles", Schema: List{box}, Default: emptyList},
	{Name: "velocity_limit", Schema: Nullable{Float{}}, Default: constant(nil)},
	{Name: "joint_limits", Schema: Dict{Tuple{Items: []Schema{Float{}, Float{}}}}, Default: emptyDict},
	{Name: "torque_limit", Schema: Nullable{Float{}}, Default: constant(nil)},
}}

// Invariant is models.Invariant.
var Invariant = &Model{Name: "Invariant", Fields: []Field{
	{Name: "type", Schema: Str{}, Required: true},
	{Name: "target", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "scope", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "description", Schema: Str{}, Required: true},
	{Name: "expr", Schema: Any{}, Default: constant(nil)},
	{Name: "enforce", Schema: Bool{}, Default: constant(true)},
}}

// Postcondition is models.Postcondition.
var Postcondition = &Model{Name: "Postcondition", Fields: []Field{
	{Name: "type", Schema: Str{}, Required: true},
	{Name: "path", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "expected", Schema: Nullable{Int{}}, Default: constant(nil)},
	{Name: "min", Schema: Nullable{Int{}}, Default: constant(nil)},
	{Name: "max", Schema: Nullable{Int{}}, Default: constant(nil)},
	{Name: "description", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "expr", Schema: Any{}, Default: constant(nil)},
	{Name: "enforce", Schema: Bool{}, Default: constant(true)},
}}

// FallbackStrategy is models.FallbackStrategy.
var FallbackStrategy = &Model{Name: "FallbackStrategy", Fields: []Field{
	{Name: "strategy", Schema: Str{}, Default: constant("tier2_recompute")},
	{Name: "model", Schema: Str{}, Default: constant("anthropic/claude-sonnet-4-20250514")},
	{Name: "include_refinement", Schema: Bool{}, Default: constant(true)},
}}

func newEnvelopeID() any {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "env_" + hex.EncodeToString(b)
}

func fallbackDefault() any {
	out, _ := FallbackStrategy.fields(pyjson.NewObject(), nil, JSON)
	return out
}

// Envelope is models.Envelope.
var Envelope = &Model{Name: "Envelope", Finite: true, Fields: []Field{
	{Name: "id", Schema: Str{}, Default: newEnvelopeID},
	{Name: "generated_by", Schema: Str{}, Required: true},
	{Name: "task", Schema: Str{}, Required: true},
	{Name: "permissions", Schema: Permission, Required: true},
	{Name: "invariants", Schema: List{Invariant}, Default: emptyList},
	{Name: "postconditions", Schema: List{Postcondition}, Default: emptyList},
	{Name: "fallback", Schema: FallbackStrategy, Default: fallbackDefault},
	{Name: "parent_envelope", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "tightening_only", Schema: Bool{}, Default: constant(true)},
	{Name: "summary", Schema: Nullable{Str{MaxLen: 80}}, Default: constant(nil)},
	{Name: "cache_key", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "stakes", Schema: Literal{[]string{"low", "medium", "high", "physical"}}, Default: constant("low")},
	{Name: "shell_interpreter_policy", Schema: Literal{[]string{"surface", "strict", "allow"}}, Default: constant("surface")},
}}

// Tagged is a discriminated union of models on one key.
type Tagged struct {
	Key  string
	Tags []string
	// Models by tag; filled after the union is built, since it recurses.
	Models map[string]*Model
}

func (t *Tagged) validate(v any, loc []any, mode Mode) (any, []Err) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, one(loc, "model_attributes_type", "Input should be a valid dictionary or object to extract fields from", v)
	}
	tagV, present := o.Get(t.Key)
	if !present {
		return nil, one(loc, "union_tag_not_found", "Unable to extract tag using discriminator '"+t.Key+"'", v)
	}
	tag := Repr(tagV)
	if s, isStr := tagV.(string); isStr {
		tag = s
	}
	m, found := t.Models[tag]
	if _, isStr := tagV.(string); !found || !isStr {
		quoted := make([]string, len(t.Tags))
		for i, x := range t.Tags {
			quoted[i] = "'" + x + "'"
		}
		return nil, one(loc, "union_tag_invalid", "Input tag '"+tag+"' found using '"+t.Key+
			"' does not match any of the expected tags: "+strings.Join(quoted, ", "), v)
	}
	return m.fields(o, with(loc, tag), mode)
}

// Expression is predicate.Expression, validated in Python mode as
// parse_expression does.
var Expression = &Tagged{Key: "op"}

// ExpressionTitle is the title of parse_expression's ValidationError.
const ExpressionTitle = "tagged-union[Equals,NotEquals,InSet,NotInSet,Matches,NotMatches,NumericRange,LengthRange," +
	"Exists,IsEmpty,And,Or,Not,Implies,ForallSteps,ExistsStep,ForallOutputs,DependsOn,Before,AliasRef,LLMCheck]"

func init() {
	op := func(tag string) Field {
		return Field{Name: "op", Schema: Literal{[]string{tag}}, Default: constant(tag)}
	}
	req := func(name string, s Schema) Field { return Field{Name: name, Schema: s, Required: true} }
	path := req("path", Str{})
	defs := []struct {
		tag, name string
		fields    []Field
	}{
		{"equals", "Equals", []Field{path, req("value", Any{})}},
		{"not_equals", "NotEquals", []Field{path, req("value", Any{})}},
		{"in_set", "InSet", []Field{path, req("values", List{Any{}})}},
		{"not_in_set", "NotInSet", []Field{path, req("values", List{Any{}})}},
		{"matches", "Matches", []Field{path, req("regex", Str{})}},
		{"not_matches", "NotMatches", []Field{path, req("regex", Str{})}},
		{"numeric_range", "NumericRange", []Field{path, req("min", Float{}), req("max", Float{})}},
		{"length_range", "LengthRange", []Field{path, {Name: "min", Schema: Int{}, Default: constant(pyjson.Int{Text: "0"})},
			{Name: "max", Schema: Nullable{Int{}}, Default: constant(nil)}}},
		{"exists", "Exists", []Field{path}},
		{"is_empty", "IsEmpty", []Field{path}},
		{"and", "And", []Field{req("children", List{Expression})}},
		{"or", "Or", []Field{req("children", List{Expression})}},
		{"not", "Not", []Field{req("child", Expression)}},
		{"implies", "Implies", []Field{req("a", Expression), req("b", Expression)}},
		{"forall_steps", "ForallSteps", []Field{req("pred", Expression)}},
		{"exists_step", "ExistsStep", []Field{req("pred", Expression)}},
		{"forall_outputs", "ForallOutputs", []Field{req("pred", Expression)}},
		{"depends_on", "DependsOn", []Field{req("step_id_a", Str{}), req("step_id_b", Str{})}},
		{"before", "Before", []Field{req("step_id_a", Str{}), req("step_id_b", Str{})}},
		{"alias", "AliasRef", []Field{req("name", Str{}), {Name: "args", Schema: Dict{Any{}}, Default: emptyDict}}},
		{"llm_check", "LLMCheck", []Field{req("rule", Str{})}},
	}
	Expression.Models = map[string]*Model{}
	for _, d := range defs {
		Expression.Tags = append(Expression.Tags, d.tag)
		Expression.Models[d.tag] = &Model{Name: d.name, Fields: append([]Field{op(d.tag)}, d.fields...)}
	}
}
