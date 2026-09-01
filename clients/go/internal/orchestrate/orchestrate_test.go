package orchestrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// fakeClaude is a client on the claude-code backend whose `claude`
// answers every call with reply as the result text.
func fakeClaude(t *testing.T, reply string) *llm.Client {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	env, _ := json.Marshal(map[string]any{"type": "result", "is_error": false, "result": reply})
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\ncat >/dev/null\ncat <<'EOF'\n" + string(env) + "\nEOF\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"OPENDAISUGI_LLM_BACKEND": "claude-code"}
	return llm.New(llm.Env{Getenv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok }, Home: dir,
		LookPath: func(string) (string, error) { return script, nil }})
}

func envelope(t *testing.T) verify.Envelope {
	t.Helper()
	e, err := verify.ParseEnvelope(json.RawMessage(`{"id": "env_00000001", "generated_by": "t", "task": "t",
		"permissions": {"file_read": [], "file_write": [], "network": false, "shell": false, "shell_allowlist": []}}`))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const oneTask = `{"steps": [{"id": "a", "type": "task", "prompt": "x"}]}`

func TestDecompose(t *testing.T) {
	plan, err := Decompose(fakeClaude(t, oneTask), "p", DefaultDecomposeModel, envelope(t), 500)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Value("source") != "decomposer" || len(plan.Value("steps").([]any)) != 1 {
		t.Fatal(plan)
	}
}

// A Z3 check that does not finish when the decomposed plan is verified
// fails the decomposition (fail closed), where the oracle would warn.
func TestDecomposeZ3UnknownFailsClosed(t *testing.T) {
	old := verifyPlan
	defer func() { verifyPlan = old }()
	verifyPlan = func(verify.ActionPlan, verify.Envelope, verify.VerifyOptions) verify.VerifyResultGo {
		return verify.VerifyResultGo{OK: true, Timeouts: []string{"Z3 returned unknown"}}
	}
	_, err := Decompose(fakeClaude(t, oneTask), "p", DefaultDecomposeModel, envelope(t), 500)
	var de *DecompositionError
	if !errors.As(err, &de) || de.Msg != "decomposed plan failed verify against envelope (out of policy): [z3] Z3 returned unknown" {
		t.Fatal(err)
	}
}

func TestDecomposeRefusals(t *testing.T) {
	for reply, want := range map[string]string{
		`{"steps": []}`: "decomposition produced no steps",
		`{"steps": [{"id": "a", "type": "task", "prompt": "x", "depends_on": ["a"]}]}`:     "decomposed plan is not a valid DAG: ",
		`{"steps": [{"id": "a", "type": "shell"}]}`:                                        "step 'a' (type 'shell') is missing required fields: 1 validation error for ShellStep",
		`{"steps": [{"id": "a", "type": "shell", "command": "ls"}]}`:                       "decomposed plan failed verify against envelope (out of policy): [permissions]",
		`{"steps": [{"id": "a", "type": "task", "prompt": "x", "arguments": {"n": NaN}}]}`: "decomposition LLM call failed: ",
	} {
		_, err := Decompose(fakeClaude(t, reply), "p", DefaultDecomposeModel, envelope(t), 500)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v", reply, err)
		}
	}
}

func step(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, verr := pmodel.ValidateJSON("TaskStep", pmodel.StepTypes["task"], text)
	if verr != nil {
		t.Fatal(verr)
	}
	return v.(*pyjson.Object)
}

// Sizing: the step type's floor, the prompt's signals, fan-in, and the
// budget's downgrade.
func TestSizing(t *testing.T) {
	l := BuildLadder("")
	easy := step(t, `{"id": "a", "type": "task", "prompt": "x", "depends_on": ["b"]}`)
	if s := SizeStep(easy, l, nil, ""); s.Difficulty != 0.35 || s.Tier != "cheap" || !s.Affordable {
		t.Fatalf("%+v", s)
	}
	hard := step(t, `{"id": "h", "type": "task", "prompt": "design the security architecture"}`)
	if s := SizeStep(hard, l, nil, ""); s.Tier != "frontier" {
		t.Fatalf("%+v", s)
	}
	total := int64(3000)
	if s := SizeStep(hard, l, &Tracker{Total: &total}, ""); s.Tier != "cheap" || !s.Downgraded {
		t.Fatalf("%+v", s)
	}
	total = 10
	if s := SizeStep(hard, l, &Tracker{Total: &total}, ""); s.Affordable || !s.Downgraded {
		t.Fatalf("%+v", s)
	}
}

func TestBudgetReport(t *testing.T) {
	tr := &Tracker{}
	if r := tr.Report().Dump(); pyjson.Dumps(r, true) !=
		`{"total": null, "spent": 0, "remaining": null, "step_count": 0, "by_model": {}, "approx_cost_usd": 0, "measured_cost_usd": null}` {
		t.Fatal(pyjson.Dumps(r, true))
	}
	total := int64(100)
	tr = &Tracker{Total: &total, Strict: true}
	if err := tr.Record("a", "claude-haiku-4-5", 101, nil); err == nil {
		t.Fatal("strict overrun not raised")
	}
	if !tr.Exhausted() || tr.Report().Spent != 101 {
		t.Fatal("the spend was not counted")
	}
}

// A Z3 check that does not finish in the verify just before the run
// rejects the run (fail closed): nothing runs, and it is journaled.
func TestRunZ3UnknownBeforeTheRunFailsClosed(t *testing.T) {
	old := verifyPlan
	defer func() { verifyPlan = old }()
	calls := 0
	verifyPlan = func(p verify.ActionPlan, e verify.Envelope, o verify.VerifyOptions) verify.VerifyResultGo {
		calls++
		if calls == 1 {
			return old(p, e, o)
		}
		return verify.VerifyResultGo{OK: true, Timeouts: []string{"Z3 returned unknown"}}
	}
	envText := `{"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": {}}`
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, envText)
	if verr != nil {
		t.Fatal(verr)
	}
	dir := t.TempDir()
	j, err := tracejournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	r, err := Run(Options{LLM: fakeClaude(t, oneTask), Prompt: "p", Env: v.(*pyjson.Object), VEnv: envelope(t),
		Journal: j, DecomposeModel: DefaultDecomposeModel, Z3TimeoutMs: 500, StepTimeoutS: 30, Ladder: BuildLadder("")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Session.Status != "rejected" || len(r.Session.Steps) != 0 || r.Session.TraceID == nil {
		t.Fatalf("%+v", r.Session)
	}
	body, _ := os.ReadFile(j.TracePath(*r.Session.TraceID))
	if !strings.Contains(string(body), "stage: z3") {
		t.Fatal(string(body))
	}
}
