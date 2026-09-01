package orchestrate

import (
	"encoding/json"
	"fmt"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// DefaultDecomposeModel is decomposer._DEFAULT_MODEL.
const DefaultDecomposeModel = "anthropic/claude-sonnet-4-20250514"

// decomposerSystem is decomposer.DECOMPOSER_SYSTEM_PROMPT.
const decomposerSystem = `You are a planning decomposer. Given a task, break it into the smallest useful
sequence of typed steps and return them as a DAG.

Step types:
- "task": a natural-language subtask for an LLM to reason about. Field: prompt.
  Use this for analysis, drafting, summarizing, deciding — anything that is
  thinking rather than acting.
- "skill": invoke a reusable named skill/pathway. Field: skill_id (+ optional
  skill_input). Use when a distilled capability already covers the sub-goal.
- "mcp": call an external tool over MCP. Fields: server, tool (+ optional
  arguments).
- "shell": run one shell command (no pipes/;/&&). Field: command.
- "file_read"/"file_write": read/write one path. Fields: path (+ content).
- "network": one HTTP GET. Field: url.

Rules:
- Give every step a short unique id.
- Use depends_on (list of step ids) to encode ordering; independent steps may
  have no dependencies and will run in parallel-eligible order.
- Prefer "task" steps for reasoning and keep each step focused on one thing.
- Do not invent shell pipelines or chained commands; emit separate steps.
`

// verifyPlan is verify.Verify; a test swaps it to make a Z3 check answer
// unknown.
var verifyPlan = verify.Verify

func constant(v any) func() any { return func() any { return v } }

// typeFields is decomposer._TYPE_FIELDS.
var typeFields = []string{"prompt", "skill_id", "skill_input", "server", "tool", "arguments", "command",
	"path", "content", "url"}

// DecomposedStep and DecomposedPlan are the decomposer's reply models.
var DecomposedStep = func() *pmodel.Model {
	opt := func(name string, s pmodel.Schema) pmodel.Field {
		return pmodel.Field{Name: name, Schema: pmodel.Nullable{Inner: s}, Default: constant(nil)}
	}
	return &pmodel.Model{Name: "DecomposedStep", Fields: []pmodel.Field{
		{Name: "id", Schema: pmodel.Str{}, Required: true},
		{Name: "type", Schema: pmodel.Literal{Choices: []string{"task", "skill", "mcp", "shell", "file_read", "file_write", "network"}}, Required: true},
		{Name: "depends_on", Schema: pmodel.List{Elem: pmodel.Str{}}, Default: func() any { return []any{} }},
		opt("prompt", pmodel.Str{}), opt("skill_id", pmodel.Str{}), opt("skill_input", pmodel.Dict{Val: pmodel.Any{}}),
		opt("server", pmodel.Str{}), opt("tool", pmodel.Str{}), opt("arguments", pmodel.Dict{Val: pmodel.Any{}}),
		opt("command", pmodel.Str{}), opt("path", pmodel.Str{}), opt("content", pmodel.Str{}), opt("url", pmodel.Str{}),
	}}
}()

var DecomposedPlan = &pmodel.Model{Name: "DecomposedPlan", Fields: []pmodel.Field{
	{Name: "steps", Schema: pmodel.List{Elem: DecomposedStep}, Required: true},
}}

// DecompositionError is exceptions.DecompositionError; NoSteps marks
// NoStepsError.
type DecompositionError struct {
	Msg     string
	NoSteps bool
}

func (e *DecompositionError) Error() string { return e.Msg }

// NotConfigured is LLMNotConfigured, which the decomposer's client raises
// before any call.
type NotConfigured struct{ Msg string }

func (e *NotConfigured) Error() string { return e.Msg }

// inventory is decomposer._inventory_block for the orchestrator's call:
// no skill handlers and no declared MCP tools.
const inventory = "No skills or MCP tools are available in this environment. Decompose " +
	"using ONLY 'task' steps (natural-language subtasks); do NOT emit " +
	"'skill' or 'mcp' steps.\n\n"

// Decompose is decomposer.decompose with the orchestrator's arguments: a
// plan verified against env, or an error.
func Decompose(c *llm.Client, prompt, model string, env verify.Envelope, z3ms int) (*pyjson.Object, error) {
	if e := c.Preflight(model); e != nil {
		return nil, &NotConfigured{e.Msg}
	}
	reply, err := c.Structured(llm.Call{Model: model, System: decomposerSystem, User: inventory + prompt,
		Response: llm.Schema{Name: "DecomposedPlan", Model: DecomposedPlan}, MaxRetries: 2})
	if err != nil {
		return nil, &DecompositionError{Msg: "decomposition LLM call failed: " + err.Error()}
	}
	items, _ := reply.Value("steps").([]any)
	if len(items) == 0 {
		return nil, &DecompositionError{Msg: "decomposition produced no steps", NoSteps: true}
	}
	steps := make([]any, 0, len(items))
	for _, it := range items {
		s := it.(*pyjson.Object)
		payload := pyjson.NewObject().Set("type", s.Value("type")).Set("id", s.Value("id")).
			Set("depends_on", s.Value("depends_on"))
		for _, f := range typeFields {
			if v := s.Value(f); v != nil {
				payload.Set(f, v)
			}
		}
		kind := str(s, "type")
		m := pmodel.StepTypes[kind]
		out, verr := pmodel.Validate(m.Name, m, payload, pmodel.Python)
		if verr != nil {
			return nil, &DecompositionError{Msg: fmt.Sprintf("step %s (type %s) is missing required fields: %s",
				pystr.Repr(str(s, "id")), pystr.Repr(kind), verr.String())}
		}
		steps = append(steps, out)
	}
	planV, verr := pmodel.Validate("ActionPlan", pmodel.ActionPlan,
		pyjson.NewObject().Set("source", "decomposer").Set("task", prompt).Set("steps", steps), pmodel.Python)
	if verr != nil {
		return nil, &DecompositionError{Msg: "the decomposed plan does not validate: " + verr.String()}
	}
	plan := planV.(*pyjson.Object)
	vp, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
	if err != nil {
		return nil, fmt.Errorf("the decomposed plan does not read: %w", err)
	}
	if vs := verify.CheckDAG(vp); len(vs) > 0 {
		return nil, &DecompositionError{Msg: "decomposed plan is not a valid DAG: " + vs[0].Message}
	}
	r := verifyPlan(vp, env, verify.VerifyOptions{Z3TimeoutMs: z3ms})
	if len(r.Timeouts) > 0 && r.OK {
		// Fail closed where the oracle's lenient verify would warn (K2-2).
		r.OK = false
		r.Violations = append(r.Violations, verify.V("z3", r.Timeouts[0]))
	}
	if !r.OK {
		v := r.Violations[0]
		return nil, &DecompositionError{Msg: fmt.Sprintf("decomposed plan failed verify against envelope "+
			"(out of policy): [%s] %s", v.Stage, v.Message)}
	}
	return plan, nil
}
