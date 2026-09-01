package transcript

import (
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// stepUnion is models.ActionStep: the registered step types as a union
// discriminated on "type", in the union's order.
var stepUnion = &pmodel.Tagged{Key: "type", Tags: []string{"shell", "file_read", "file_write", "network",
	"joint_move", "cartesian_move", "gripper", "sim_reset", "vla", "task", "agentic", "skill", "mcp"},
	Models: pmodel.StepTypes}

// Episode is parsers.Episode.
var Episode = &pmodel.Model{Name: "Episode", Fields: []pmodel.Field{
	{Name: "id", Schema: pmodel.Str{}, Required: true},
	{Name: "task", Schema: pmodel.Str{}, Required: true},
	{Name: "context", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: func() any { return nil }},
	{Name: "steps", Schema: pmodel.List{Elem: stepUnion}, Required: true},
	{Name: "source_range", Schema: pmodel.Dict{Val: pmodel.Any{}}, Required: true},
}}

// ParseResult is parsers.ParseResult.
var ParseResult = &pmodel.Model{Name: "ParseResult", Fields: []pmodel.Field{
	{Name: "source", Schema: pmodel.Str{}, Required: true},
	{Name: "source_file", Schema: pmodel.Str{}, Required: true},
	{Name: "parsed_at", Schema: pmodel.Str{}, Required: true},
	{Name: "episodes", Schema: pmodel.List{Elem: Episode}, Required: true},
}}

// Validate is ParseResult(**raw) on a mapping: the validated dump, or
// pydantic's error.
func Validate(raw *pyjson.Object) (*pyjson.Object, *pmodel.ValidationError) {
	return ParseResult.ValidateObject(raw, pmodel.Python)
}
