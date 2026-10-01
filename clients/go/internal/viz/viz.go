// Package viz is opendaisugi.viz: a plan, verified against an envelope,
// as a standalone execution-monitor page. The page is the oracle's own
// template (assets/viz_dag_template.html, a copy a test checks) with the
// view model in its data block.
package viz

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"

	"daisugi-verify/internal/orchestrate"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

//go:embed assets/viz_dag_template.html
var template string

// kindLabel is viz._KIND_LABEL.
var kindLabel = map[string]string{
	"shell": "Shell", "file_read": "Read", "file_write": "Write", "network": "Network",
	"task": "Sub-agent", "skill": "Skill", "mcp": "MCP",
}

// llmKinds is viz._LLM_KINDS.
var llmKinds = map[string]bool{"task": true, "agentic": true}

var stepRe = regexp.MustCompile(`[Ss]tep '([^']+)'`)

func str(o *pyjson.Object, k string) string {
	s, _ := o.Value(k).(string)
	return s
}

// stepLabel is viz._step_label.
func stepLabel(s *pyjson.Object) string {
	t := str(s, "type")
	switch t {
	case "shell":
		return str(s, "command")
	case "file_read", "file_write":
		return str(s, "path")
	case "network":
		return str(s, "url")
	case "task":
		return str(s, "prompt")
	case "skill":
		return str(s, "skill_id")
	case "mcp":
		return str(s, "server") + "/" + str(s, "tool")
	}
	return t
}

func strList(v any) []any {
	xs, _ := v.([]any)
	if xs == nil {
		return []any{}
	}
	return xs
}

// Data is viz.plan_to_viz_data with the default ladder: plan and env are
// model_dump(mode="json") objects. An error is a plan or envelope this
// package does not read, or the exception dependency_levels raises.
func Data(plan, env *pyjson.Object) (*pyjson.Object, error) {
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		return nil, err
	}
	vplan, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
	if err != nil {
		return nil, err
	}
	r := verify.Verify(vplan, venv, verify.VerifyOptions{Z3TimeoutMs: 500})
	steps := tracejournal.Steps(plan)
	sizings := map[string]orchestrate.Sizing{}
	for _, sz := range orchestrate.SizePlan(steps, orchestrate.BuildLadder("")) {
		sizings[sz.StepID] = sz
	}
	levels, err := tracejournal.Levels(plan)
	if err != nil {
		return nil, err
	}
	levelOf := map[string]int{}
	levelIDs := make([]any, len(levels))
	for i, l := range levels {
		ids := make([]any, len(l))
		for j, st := range l {
			id := str(st, "id")
			levelOf[id] = i
			ids[j] = id
		}
		levelIDs[i] = ids
	}
	violByStep := map[string][]any{}
	for _, v := range r.Violations {
		sid, ok := "", false
		if m := stepRe.FindStringSubmatch(v.Message); m != nil {
			sid, ok = m[1], true
		} else if v.HasStep {
			sid, ok = v.Step, true
		}
		if !ok {
			continue // keyed by None: no step has that id
		}
		violByStep[sid] = append(violByStep[sid], pyjson.NewObject().Set("stage", v.Stage).Set("message", v.Message))
	}
	out := make([]any, len(steps))
	for i, s := range steps {
		id := str(s, "id")
		t := str(s, "type")
		kind, ok := kindLabel[t]
		if !ok {
			kind = t
		}
		vs := violByStep[id]
		if vs == nil {
			vs = []any{}
		}
		o := pyjson.NewObject().Set("id", id).Set("type", t).Set("kind", kind).Set("label", stepLabel(s)).
			Set("depends_on", strList(s.Value("depends_on"))).Set("level", levelOf[id]).
			Set("blocked", len(vs) > 0).Set("violations", vs)
		if sz, ok := sizings[id]; ok {
			o.Set("model", sz.Model).Set("tier", sz.Tier).Set("difficulty", pyjson.Round(sz.Difficulty, 2)).
				Set("est_tokens", int(sz.EstTokens))
		} else {
			o.Set("model", nil).Set("tier", nil).Set("difficulty", nil).Set("est_tokens", nil)
		}
		o.Set("runs_llm", llmKinds[t])
		out[i] = o
	}
	perms, _ := env.Value("permissions").(*pyjson.Object)
	task := str(env, "task")
	if task == "" {
		task = str(plan, "task")
	}
	return pyjson.NewObject().
		Set("task", task).
		Set("envelope", pyjson.NewObject().
			Set("shell_allowlist", strList(perms.Value("shell_allowlist"))).
			Set("file_read", strList(perms.Value("file_read"))).
			Set("network", perms.Value("network")).
			Set("mcp_allowlist", strList(perms.Value("mcp_allowlist")))).
		Set("ok", r.OK).
		Set("n_violations", len(r.Violations)).
		Set("levels", levelIDs).
		Set("steps", out), nil
}

// Render is viz.render_dag_html: the template with the view model in its
// data block, `</` escaped so a label cannot close the script.
func Render(plan, env *pyjson.Object) (string, error) {
	d, err := Data(plan, env)
	if err != nil {
		return "", err
	}
	payload := strings.ReplaceAll(pyjson.Dumps(d, true), "</", `<\/`)
	return strings.ReplaceAll(template, "/*__DATA__*/ null", payload), nil
}
