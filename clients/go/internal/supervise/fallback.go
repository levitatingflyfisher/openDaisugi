package supervise

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// recomputeSystem is fallback._RECOMPUTE_SYSTEM_PROMPT.
const recomputeSystem = "You are a step-repair agent. A step in an action plan was rejected by a " +
	"safety verifier. Your job is to produce a SINGLE replacement step that " +
	"accomplishes the same goal while respecting the envelope's constraints.\n\n" +
	"Output a JSON object matching the step schema. Do NOT output an envelope " +
	"or a full plan — just one step.\n"

// recomputePrompt is RecomputeHandler._build_prompt.
func recomputePrompt(step, env *pyjson.Object, violations []verify.Violation) string {
	perms, _ := env.Value("permissions").(*pyjson.Object)
	parts := []string{
		"Rejected step:\n" + pyjson.DumpsIndent(tracejournal.JSONMode(step), 2, true),
		"\nEnvelope permissions:\n" + pyjson.DumpsIndent(tracejournal.JSONMode(perms), 2, true),
	}
	include := false
	if fs, ok := env.Value("fallback").(*pyjson.Object); ok {
		include = fs.Value("include_refinement") == true
	}
	if include && len(violations) > 0 {
		lines := make([]string, len(violations))
		for i, v := range violations {
			lines[i] = fmt.Sprintf("- [%s] %s", v.Stage, v.Message)
		}
		parts = append(parts, "\nViolations:\n"+strings.Join(lines, "\n"))
	}
	parts = append(parts, "\nProduce a replacement step that accomplishes the same goal within these permissions.")
	return strings.Join(parts, "\n")
}

// verifyPlan and structured are verify.Verify and the model call; a test
// swaps them.
var (
	verifyPlan = verify.Verify
	structured = func(c *llm.Client, call llm.Call) (*pyjson.Object, error) { return c.Structured(call) }
	preflight  = func(c *llm.Client, model string) bool { return c.Preflight(model) == nil }
)

// Recompute is fallback.RecomputeHandler: one model call for a
// replacement of the step's own type, kept only when a one-step plan of it
// verifies against the envelope. Any failure halts.
func Recompute(c *llm.Client, env *pyjson.Object, venv verify.Envelope, z3ms int) Fallback {
	return func(step *pyjson.Object, violations []verify.Violation) (*pyjson.Object, *pyjson.Object) {
		fs, _ := env.Value("fallback").(*pyjson.Object)
		model, _ := fs.Value("model").(string)
		m := pmodel.StepTypes[str(step, "type")]
		if m == nil || !preflight(c, model) {
			return nil, nil
		}
		reply, err := structured(c, llm.Call{Model: model, System: recomputeSystem,
			User: recomputePrompt(step, env, violations), Response: llm.Schema{Name: m.Name, Model: m}, MaxRetries: 2})
		if err != nil {
			return nil, nil
		}
		plan := pyjson.NewObject().Set("id", "plan_"+hex8()).Set("source", "recompute").Set("task", venv.Task).
			Set("steps", []any{reply})
		vp, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
		if err != nil {
			return nil, nil
		}
		t0 := time.Now()
		r := verifyPlan(vp, venv, verify.VerifyOptions{Z3TimeoutMs: z3ms})
		if !r.OK || len(r.Timeouts) > 0 {
			return nil, nil
		}
		dump, why := VerificationDump(r, env.Value("id"), plan.Value("id"), float64(time.Since(t0).Nanoseconds())/1e6)
		if why != "" {
			return nil, nil
		}
		return reply, dump
	}
}
