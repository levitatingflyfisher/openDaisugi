package supervise

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// readOnlyKinds is supervisor._READ_ONLY_KINDS.
var readOnlyKinds = map[string]bool{"file_read": true, "network": true}

// Fallback is a fallback handler: given a rejected step and its
// violations, a replacement step (recomputed) or nil (halted).
type Fallback func(step *pyjson.Object, violations []verify.Violation) (replacement *pyjson.Object, verification *pyjson.Object)

// Supervisor is supervisor.Supervisor with its CLI settings.
type Supervisor struct {
	Executors      map[string]Executor
	Approval       Approver
	Journal        *tracejournal.Journal
	Z3TimeoutMs    int
	StepTimeoutS   int
	MaxOutputBytes int
	Strict         *bool
	// Fallback is the envelope's fallback handler; nil halts.
	Fallback Fallback
	// LogErr is the error log_run met, if any: the run is then not
	// journaled, and the caller fails as the oracle raises.
	LogErr error
	// VerifyStep is verify.VerifyStep; a test swaps it.
	VerifyStep func(plan verify.ActionPlan, env verify.Envelope, z3ms int) verify.VerifyResultGo
	// Hook is a runner's per-step hook (weave); nil changes nothing.
	Hook Hook
}

// Hook is supervisor.StepHook: a runner's hook into each step.
type Hook interface {
	// Prepare gets each step before its per-step verify. It returns the
	// step to verify and run, or an outcome to record in its place (skip),
	// or an outcome and a run status to stop the run there.
	Prepare(step *pyjson.Object) (next *pyjson.Object, skip *Outcome, stop *Outcome, status string)
	// Checked gets each step that passed its per-step verify; an outcome
	// it returns stops the run there with the status.
	Checked(step *pyjson.Object) (stop *Outcome, status string)
	// Started is called just before a step's executor runs; a reason it
	// returns stops the run there, and the step does not run.
	Started(step *pyjson.Object, runID string) string
	// Finish gets each executed step's outcome and returns the outcome to
	// record.
	Finish(step *pyjson.Object, out Outcome) Outcome
}

// NowISO is supervisor._now_iso.
func NowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

func hex8() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewRunID is f"run_{uuid4().hex[:8]}".
func NewRunID() string { return "run_" + hex8() }

// singleton is a one-step plan for the per-step checks, as the oracle
// builds it.
func singleton(source, task string, step *pyjson.Object) (verify.ActionPlan, error) {
	p := pyjson.NewObject().Set("id", "plan_"+hex8()).Set("source", source).Set("task", task).
		Set("steps", []any{step})
	return verify.ParsePlan(json.RawMessage(pathways.DumpJSON(p)))
}

func copyStep(step *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range step.Keys() {
		out.Set(k, step.Value(k))
	}
	return out
}

func isolated(step *pyjson.Object) *pyjson.Object {
	c := copyStep(step)
	c.Set("depends_on", []any{})
	return c
}

// VerificationDump is VerificationResult.model_dump(mode="json") of a
// whole-plan verify. ok is false when a violation's detail is one this
// binary does not word, or a warning may be missing: the caller refuses.
func VerificationDump(r verify.VerifyResultGo, envID, planID any, durationMs float64) (*pyjson.Object, string) {
	violations := []any{}
	for _, v := range r.Violations {
		if !v.Known {
			return nil, fmt.Sprintf("a violation (%s) whose detail this binary does not write yet", v.Message)
		}
		var rem any
		if v.Remediation != nil {
			rem = *v.Remediation
		}
		violations = append(violations, pyjson.NewObject().Set("stage", v.Stage).Set("message", v.Message).
			Set("detail", v.Detail).Set("suggested_remediation", rem))
	}
	if r.WarningsUnmodeled {
		return nil, "a verifier warning this binary does not word yet"
	}
	warnings := []any{}
	for _, w := range r.Warnings {
		warnings = append(warnings, w)
	}
	return pyjson.NewObject().Set("ok", r.OK).Set("violations", violations).Set("warnings", warnings).
		Set("envelope_id", envID).Set("plan_id", planID).Set("duration_ms", durationMs).
		Set("client", "python").Set("fallback", nil).Set("client_verdict", nil), ""
}

// Run is Supervisor.run with the whole-plan verification already made
// (verification is its JSON dump). plan and env are model_dump() values,
// venv the envelope as the verifier reads it.
func (s *Supervisor) Run(plan, env *pyjson.Object, venv verify.Envelope, verification *pyjson.Object) *Session {
	sess := &Session{ID: NewRunID(), EnvelopeID: str(env, "id"), PlanID: str(plan, "id"), Status: Pending,
		Verification: verification, StartedAt: NowISO()}
	task := str(plan, "task")
	if verification.Value("ok") != true {
		sess.Status = Rejected
		sess.EndedAt = strp(NowISO())
		s.journalSession(sess, plan, env, task)
		return sess
	}
	sess.Status = Running
	ordered, err := tracejournal.TopoOrderErr(plan)
	if err != nil {
		// A plan that verified has no cycle; fail closed if it has one.
		sess.Status = Failed
		sess.EndedAt = strp(NowISO())
		s.journalSession(sess, plan, env, task)
		return sess
	}
	for _, ex := range s.Executors {
		if c, ok := ex.(Configurable); ok {
			c.Configure(env)
		}
	}
	verifyStep := s.VerifyStep
	if verifyStep == nil {
		verifyStep = verify.VerifyStep
	}
	stepCheck := func(step *pyjson.Object) verify.VerifyResultGo {
		p, err := singleton("per-step-verify", str(env, "task"), isolated(step))
		if err != nil {
			return verify.VerifyResultGo{Violations: []verify.Violation{verify.V("permissions", "the step does not read: "+err.Error())}}
		}
		r := verifyStep(p, venv, s.Z3TimeoutMs)
		if len(r.Timeouts) > 0 && r.OK {
			// A check that did not finish is a failure here (fail closed).
			r.OK = false
			r.Violations = append(r.Violations, verify.V("z3", r.Timeouts[0]))
		}
		return r
	}
	completed := true
	for _, step := range ordered {
		if s.Hook != nil {
			next, skip, stop, status := s.Hook.Prepare(step)
			if stop != nil {
				sess.Steps = append(sess.Steps, *stop)
				sess.Status = status
				completed = false
				break
			}
			if skip != nil {
				sess.Steps = append(sess.Steps, *skip)
				continue
			}
			step = next
		}
		res := stepCheck(step)
		if !res.OK {
			replacement := s.onRejection(sess, step, res, env)
			if replacement == nil {
				msg := "rejected"
				if len(res.Violations) > 0 {
					msg = "rejected: " + res.Violations[0].Message
				}
				sess.Steps = append(sess.Steps, Outcome{StepID: str(step, "id"), Status: "rejected_halted",
					Stdout: "", StartedAt: NowISO(), Error: strp(msg)})
				sess.Status = Halted
				completed = false
				break
			}
			sess.Steps = append(sess.Steps, Outcome{StepID: str(step, "id"), Status: "rejected_recomputed",
				StartedAt: NowISO()})
			step = replacement
			if re := stepCheck(step); !re.OK {
				msg := "recomputed step rejected"
				if len(re.Violations) > 0 {
					msg = "recomputed step rejected: " + re.Violations[0].Message
				}
				sess.Steps = append(sess.Steps, Outcome{StepID: str(step, "id"), Status: "rejected_halted",
					StartedAt: NowISO(), Error: strp(msg)})
				sess.Status = Halted
				completed = false
				break
			}
		}
		if s.Hook != nil {
			if stop, status := s.Hook.Checked(step); stop != nil {
				sess.Steps = append(sess.Steps, *stop)
				sess.Status = status
				completed = false
				break
			}
		}
		decision, err := s.Approval.Decide(step, env)
		if err != nil {
			sess.Steps = append(sess.Steps, Outcome{StepID: str(step, "id"), Status: "aborted",
				StartedAt: NowISO(), Error: strp("approval error: " + err.Error())})
			sess.Status = Aborted
			completed = false
			break
		}
		started := NowISO()
		if !decision.Approved {
			sess.Steps = append(sess.Steps, Outcome{StepID: str(step, "id"), Status: "aborted",
				ApprovedBy: strp(decision.ApprovedBy), StartedAt: started, Error: strp("approval denied: " + decision.Reason)})
			sess.Status = Aborted
			completed = false
			break
		}
		if s.Hook != nil {
			if why := s.Hook.Started(step, sess.ID); why != "" {
				// The runner could not record that the step starts, so the
				// step does not run.
				sess.Steps = append(sess.Steps, Outcome{StepID: str(step, "id"), Status: Aborted,
					ApprovedBy: strp(decision.ApprovedBy), StartedAt: started, Error: strp(why)})
				sess.Status = Aborted
				completed = false
				break
			}
		}
		out := s.executeOne(step, started, decision)
		if out.Status == Succeeded {
			if msg := s.stage2(step, out, venv); msg != "" {
				out.Status = Failed
				out.Error = strp("stage2 rejection: " + msg)
			}
		}
		if s.Hook != nil {
			out = s.Hook.Finish(step, out)
		}
		sess.Steps = append(sess.Steps, out)
		s.writeReceipt(step, out, sess.ID)
		if out.Status == Failed {
			sess.Status = Failed
			sess.FailedStepID = strp(str(step, "id"))
			completed = false
			break
		}
	}
	if completed {
		sess.Status = Succeeded
	}
	sess.EndedAt = strp(NowISO())
	s.checkIntegrity(sess, plan)
	s.journalSession(sess, plan, env, task)
	return sess
}

// stage2 is the postcondition check on a completed step: the first
// violation's message, or "".
func (s *Supervisor) stage2(step *pyjson.Object, out Outcome, venv verify.Envelope) string {
	c := copyStep(step)
	md := pyjson.NewObject()
	if m, ok := step.Value("metadata").(*pyjson.Object); ok {
		for _, k := range m.Keys() {
			md.Set(k, m.Value(k))
		}
	}
	var rc any
	if out.RC != nil {
		rc = PyInt(*out.RC)
	}
	md.Set("output", out.Stdout).Set("rc", rc)
	c.Set("metadata", md)
	p, err := singleton("stage2", venv.Task, c)
	if err != nil || len(p.Steps) != 1 {
		return "the completed step does not read"
	}
	vs := verify.VerifyCompletedStep(p.Steps[0], venv, s.Strict)
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

func (s *Supervisor) onRejection(sess *Session, step *pyjson.Object, res verify.VerifyResultGo, env *pyjson.Object) *pyjson.Object {
	var replacement, rv *pyjson.Object
	if s.Fallback != nil {
		replacement, rv = s.Fallback(step, res.Violations)
	}
	if s.Journal != nil {
		action := "halted"
		var rstep, rver any
		if replacement != nil {
			action = "recomputed"
			rstep, rver = replacement, rv
		}
		violations := []any{}
		for _, v := range res.Violations {
			detail := v.Detail
			if detail == nil {
				detail = pyjson.NewObject()
			}
			var rem any
			if v.Remediation != nil {
				rem = *v.Remediation
			}
			violations = append(violations, pyjson.NewObject().Set("stage", v.Stage).Set("message", v.Message).
				Set("detail", detail).Set("suggested_remediation", rem))
		}
		rec := pyjson.NewObject().Set("step", step).Set("violations", violations).Set("z3_counterexample", nil).
			Set("envelope_id", env.Value("id")).Set("fallback_action", action).Set("recomputed_step", rstep).
			Set("recomputed_verification", rver).Set("timestamp", tracejournal.Now()).Set("cache_key", env.Value("cache_key"))
		s.Journal.WriteRefinement(sess.ID, pathways.DumpJSON(rec), env.Value("cache_key"))
	}
	return replacement
}

func (s *Supervisor) executeOne(step *pyjson.Object, started string, d Decision) Outcome {
	kind := str(step, "type")
	ex, ok := s.Executors[kind]
	var r ExecResult
	if !ok {
		r = ExecResult{RC: 1, Stdout: fmt.Sprintf("no executor for kind '%s'", kind)}
	} else {
		var err error
		r, err = ex.Run(step, s.StepTimeoutS, s.MaxOutputBytes)
		if err != nil {
			return Outcome{StepID: str(step, "id"), Status: Failed, ApprovedBy: strp(d.ApprovedBy),
				StartedAt: started, Error: strp("executor error: " + err.Error())}
		}
	}
	status := Failed
	if r.RC == 0 && !r.TimedOut {
		status = Succeeded
	}
	var errText *string
	switch {
	case r.TimedOut:
		errText = strp("timed out")
	case r.RC != 0:
		detail := pystr.Strip(r.Stdout)
		if detail != "" {
			errText = strp(fmt.Sprintf("exit %d: %s", r.RC, pystr.Slice(detail, 0, 500)))
		} else {
			errText = strp(fmt.Sprintf("exit %d", r.RC))
		}
	}
	rc := r.RC
	return Outcome{StepID: str(step, "id"), Status: status, ApprovedBy: strp(d.ApprovedBy), RC: &rc,
		Stdout: r.Stdout, DurationMs: r.DurationMs, StartedAt: started, Error: errText, ModelID: r.Model,
		Reversibility: r.Reversibility, Reversal: r.Reversal}
}

// writeReceipt is Supervisor._write_step_receipt.
func (s *Supervisor) writeReceipt(step *pyjson.Object, out Outcome, runID string) {
	if s.Journal == nil {
		return
	}
	var rc any
	if out.RC != nil {
		rc = PyInt(*out.RC)
	}
	evidence := pyjson.NewObject().Set("rc", rc).Set("stdout", out.Stdout).Set("duration_ms", out.DurationMs).
		Set("status", out.Status)
	if out.Error != nil && *out.Error != "" {
		evidence.Set("error", *out.Error)
	}
	ok := out.Status == Succeeded
	details := ""
	if pc, isObj := step.Value("postcondition").(*pyjson.Object); isObj && ok {
		if p, _ := pc.Value("path").(string); p != "" {
			if _, has := evidence.Get(p); has {
				details = "evidence." + p + " present"
			} else {
				ok, details = false, "postcondition expected evidence."+p
			}
		} else {
			details = "no structural check configured"
		}
	}
	kind := str(step, "type")
	rev := ""
	if out.Reversibility != nil {
		rev = *out.Reversibility
	} else if readOnlyKinds[kind] {
		rev = "none"
	} else {
		rev = "irreversible"
	}
	var revJSON *string
	if out.Reversal != nil {
		revJSON = strp(pathways.DumpJSON(out.Reversal))
	}
	// A receipt that cannot be written is left out; the integrity check
	// then fails the run's integrity, as the oracle's would.
	_ = s.Journal.AppendReceipt(tracejournal.Receipt{StepID: str(step, "id"), RunID: runID,
		Timestamp: tracejournal.Now(), Evidence: evidence, EvidenceHash: EvidenceHash(evidence),
		VerifyResult: ok, VerifyDetails: details, ModelID: out.ModelID, EffectClass: kind,
		Reversibility: rev, ReversalJSON: revJSON})
}

// checkIntegrity is Supervisor._check_run_integrity.
func (s *Supervisor) checkIntegrity(sess *Session, plan *pyjson.Object) {
	if s.Journal == nil || sess.Status == Rejected || sess.Status == Pending {
		return
	}
	got, err := s.Journal.ReceiptSteps(sess.ID)
	if err != nil {
		sess.IntegrityPassed = nil
		return
	}
	expected := map[string]bool{}
	switch {
	case sess.Status == Succeeded:
		for _, st := range tracejournal.Steps(plan) {
			expected[str(st, "id")] = true
		}
	case sess.Status == Failed && sess.FailedStepID != nil:
		ordered, err := tracejournal.TopoOrderErr(plan)
		if err != nil {
			ordered = tracejournal.Steps(plan)
		}
		for _, st := range ordered {
			expected[str(st, "id")] = true
			if str(st, "id") == *sess.FailedStepID {
				break
			}
		}
	case sess.Status == Aborted || sess.Status == Halted:
		for _, o := range sess.Steps {
			if o.Status == Succeeded || o.Status == Failed {
				expected[o.StepID] = true
			}
		}
	}
	// A step a runner skipped on its earlier receipt ran in another run.
	for _, o := range sess.Steps {
		if o.Status == "skipped" {
			delete(expected, o.StepID)
		}
	}
	pass := true
	for id := range expected {
		if !got[id] {
			pass = false
		}
	}
	sess.IntegrityPassed = &pass
}

func (s *Supervisor) journalSession(sess *Session, plan, env *pyjson.Object, task string) {
	if s.Journal == nil {
		return
	}
	var failed *string
	var durations []float64
	for _, o := range sess.Steps {
		if failed == nil && o.Status == Failed {
			failed = strp(o.StepID)
		}
		durations = append(durations, o.DurationMs)
	}
	created := NowISO()
	traceID := created[:10] + "-" + hex8()
	r := tracejournal.Run{Task: task, Env: env, Plan: plan, Verification: sess.Verification, Session: sess.Dump(),
		RunID: sess.ID, Status: sess.Status, FailedStepID: failed, TotalDurationMs: pyjson.SumFloats(durations)}
	if err := s.Journal.LogRun(r, traceID, created); err == nil {
		sess.TraceID = strp(traceID)
	} else {
		s.LogErr = err
	}
}
