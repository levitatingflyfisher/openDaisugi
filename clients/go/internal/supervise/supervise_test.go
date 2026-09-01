package supervise

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

func model(t *testing.T, title string, m *pmodel.Model, text string) *pyjson.Object {
	t.Helper()
	v, verr := pmodel.ValidateJSON(title, m, text)
	if verr != nil {
		t.Fatal(verr)
	}
	return v.(*pyjson.Object)
}

type setup struct {
	plan, env *pyjson.Object
	venv      verify.Envelope
	j         *tracejournal.Journal
	dir       string
	ran       *[]string
}

// recorder is an executor that notes each step it runs.
type recorder struct{ ran *[]string }

func (r recorder) Run(step *pyjson.Object, _, _ int) (ExecResult, error) {
	*r.ran = append(*r.ran, str(step, "id"))
	return ExecResult{RC: 0, Stdout: "ok"}, nil
}

func newSetup(t *testing.T, steps string) *setup {
	t.Helper()
	env := model(t, "Envelope", pmodel.Envelope, `{"id": "env_00000001", "generated_by": "t", "task": "t",
		"permissions": {"shell": true, "shell_allowlist": ["echo"]}}`)
	plan := model(t, "ActionPlan", pmodel.ActionPlan, `{"id": "plan_00000001", "source": "t", "task": "t", "steps": `+steps+`}`)
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	j, err := tracejournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return &setup{plan: plan, env: env, venv: venv, j: j, dir: dir, ran: &[]string{}}
}

func (s *setup) supervisor(fallback Fallback, vs func(verify.ActionPlan, verify.Envelope, int) verify.VerifyResultGo) *Supervisor {
	return &Supervisor{Executors: map[string]Executor{"shell": recorder{s.ran}}, Approval: Always{}, Journal: s.j,
		Z3TimeoutMs: 500, StepTimeoutS: 30, MaxOutputBytes: 1 << 20, Fallback: fallback, VerifyStep: vs}
}

func okVerification(t *testing.T, s *setup) *pyjson.Object {
	d, why := VerificationDump(verify.VerifyResultGo{OK: true}, s.env.Value("id"), s.plan.Value("id"), 1)
	if why != "" {
		t.Fatal(why)
	}
	return d
}

func count(t *testing.T, dir, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "journal", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const oneStep = `[{"id": "s1", "type": "shell", "command": "echo hi"}]`

func rejectAll(verify.ActionPlan, verify.Envelope, int) verify.VerifyResultGo {
	return verify.VerifyResultGo{Violations: []verify.Violation{verify.V("permissions", "no")}}
}

// A step the per-step check rejects halts the run: nothing runs, one
// refinement is written, and the run is journaled.
func TestPerStepRejectionHalts(t *testing.T) {
	s := newSetup(t, oneStep)
	sess := s.supervisor(nil, rejectAll).Run(s.plan, s.env, s.venv, okVerification(t, s))
	if sess.Status != Halted || len(sess.Steps) != 1 || sess.Steps[0].Status != "rejected_halted" ||
		*sess.Steps[0].Error != "rejected: no" {
		t.Fatalf("%+v", sess)
	}
	if len(*s.ran) != 0 || count(t, s.dir, "refinement_log") != 1 || count(t, s.dir, "receipts") != 0 ||
		count(t, s.dir, "traces") != 1 {
		t.Fatalf("ran %v", *s.ran)
	}
}

// A Z3 check that does not finish in the per-step check fails closed.
func TestPerStepZ3UnknownFailsClosed(t *testing.T) {
	s := newSetup(t, oneStep)
	unknown := func(verify.ActionPlan, verify.Envelope, int) verify.VerifyResultGo {
		return verify.VerifyResultGo{OK: true, Timeouts: []string{"Z3 returned unknown"}}
	}
	sess := s.supervisor(nil, unknown).Run(s.plan, s.env, s.venv, okVerification(t, s))
	if sess.Status != Halted || len(*s.ran) != 0 {
		t.Fatalf("%+v ran %v", sess, *s.ran)
	}
}

// A recomputed step that passes the per-step check runs in place of the
// rejected one; one that fails it halts.
func TestRecomputedStep(t *testing.T) {
	s := newSetup(t, oneStep)
	replacement := model(t, "ShellStep", pmodel.StepTypes["shell"], `{"id": "s1b", "type": "shell", "command": "echo ok"}`)
	fb := func(*pyjson.Object, []verify.Violation) (*pyjson.Object, *pyjson.Object) {
		return replacement, pyjson.NewObject()
	}
	onlyFirst := func(p verify.ActionPlan, e verify.Envelope, z int) verify.VerifyResultGo {
		if p.Steps[0].ID == "s1" {
			return rejectAll(p, e, z)
		}
		return verify.VerifyResultGo{OK: true}
	}
	sess := s.supervisor(fb, onlyFirst).Run(s.plan, s.env, s.venv, okVerification(t, s))
	if sess.Status != Succeeded || len(sess.Steps) != 2 || sess.Steps[0].Status != "rejected_recomputed" ||
		strings.Join(*s.ran, ",") != "s1b" {
		t.Fatalf("%+v ran %v", sess, *s.ran)
	}
	s2 := newSetup(t, oneStep)
	sess = s2.supervisor(fb, rejectAll).Run(s2.plan, s2.env, s2.venv, okVerification(t, s2))
	if sess.Status != Halted || len(*s2.ran) != 0 || !strings.HasPrefix(*sess.Steps[1].Error, "recomputed step rejected: ") {
		t.Fatalf("%+v", sess)
	}
}

// Recompute keeps a replacement only when a one-step plan of it verifies;
// a Z3 check that does not finish there halts (fail closed).
func TestRecomputeVerifiesTheReplacement(t *testing.T) {
	s := newSetup(t, oneStep)
	oldS, oldV, oldP := structured, verifyPlan, preflight
	defer func() { structured, verifyPlan, preflight = oldS, oldV, oldP }()
	preflight = func(*llm.Client, string) bool { return true }
	structured = func(_ *llm.Client, call llm.Call) (*pyjson.Object, error) {
		if call.Response.Name != "ShellStep" || !strings.Contains(call.User, "Rejected step:") {
			t.Fatalf("%+v", call)
		}
		return model(t, "ShellStep", pmodel.StepTypes["shell"], `{"id": "s1", "type": "shell", "command": "echo ok"}`), nil
	}
	fb := Recompute(nil, s.env, s.venv, 500)
	step := tracejournal.Steps(s.plan)[0]
	if r, _ := fb(step, nil); r == nil {
		t.Fatal("a replacement that verifies was dropped")
	}
	verifyPlan = func(verify.ActionPlan, verify.Envelope, verify.VerifyOptions) verify.VerifyResultGo {
		return verify.VerifyResultGo{OK: true, Timeouts: []string{"Z3 returned unknown"}}
	}
	if r, _ := fb(step, nil); r != nil {
		t.Fatal("a replacement whose check did not finish was kept")
	}
}

// The receipt's hash is compute_evidence_hash's: the oracle's value for
// this evidence.
func TestEvidenceHash(t *testing.T) {
	ev := pyjson.NewObject().Set("rc", PyInt(0)).Set("stdout", "hé\n").Set("duration_ms", 1.5).Set("status", "succeeded")
	// compute_evidence_hash({"rc": 0, "stdout": "hé\n", "duration_ms": 1.5,
	// "status": "succeeded"}), from the oracle.
	const want = "77ee6301ac882338165aea615acf9539bfcc7cf02683c45fa735d823d76a03b2"
	if got := EvidenceHash(ev); got != want {
		t.Fatalf("%s, want %s", got, want)
	}
}

// The default approval: the allowlist, then DAISUGI_APPROVE, then no
// terminal means deny.
func TestDefaultApproval(t *testing.T) {
	s := newSetup(t, oneStep)
	env := map[string]string{}
	d := Default{Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	step := tracejournal.Steps(s.plan)[0]
	if got, _ := d.Decide(step, s.env); !got.Approved || got.ApprovedBy != "allowlist" {
		t.Fatalf("%+v", got)
	}
	step.Set("command", "echo hi > x")
	if got, _ := d.Decide(step, s.env); got.Approved || got.ApprovedBy != "denied" {
		t.Fatalf("%+v", got)
	}
	env["DAISUGI_APPROVE"] = " NEVER "
	if got, _ := d.Decide(step, s.env); got.Approved || got.ApprovedBy != "env" {
		t.Fatalf("%+v", got)
	}
	env["DAISUGI_APPROVE"] = "maybe"
	if _, err := d.Decide(step, s.env); err == nil || !strings.HasPrefix(err.Error(), "ValueError: DAISUGI_APPROVE='maybe'") {
		t.Fatal(err)
	}
}
