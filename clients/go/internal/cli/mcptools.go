package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/delegate"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/recall"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// builtMatcher is the matcher in effect, or a refusal when it is not one
// this binary carries.
func (s *mcpServer) builtMatcher() (*pathways.Matcher, error) {
	m, notBuilt, err := s.e.matcher()
	if err != nil {
		return nil, refusef("%v", err)
	}
	if notBuilt != "" {
		return nil, refusef("matcher_model %s, which is not a built embedder here", pystr.Repr(notBuilt))
	}
	return m, nil
}

func (s *mcpServer) envelopeFor(a *pyjson.Object) (any, error) {
	task := a.Value("task").(string)
	stakes := a.Value("stakes").(string)
	if stakes != "low" && stakes != "medium" && stakes != "high" {
		return nil, &toolError{"stakes must be low|medium|high, got " + pystr.Repr(stakes)}
	}
	m, err := s.builtMatcher()
	if err != nil {
		return nil, err
	}
	c := s.e.llmClient()
	if stakes != "low" {
		if err := c.Check(s.model); err != nil {
			return nil, refusef("%v", err)
		}
	}
	// generate_envelope's arguments: the pathway store and the journal
	// are made before it runs.
	store, err := s.pathwayStore()
	if err != nil {
		return nil, err
	}
	j, err := s.theJournal()
	if err != nil {
		return nil, err
	}
	o := envgen.Options{Task: task, Models: []string{s.model}, Single: true, Stakes: stakes, Thinking: "standard",
		Cache: s.cache, Store: store, MatcherKey: m.Key, Potion: s.e.potionEnv(), Journal: j,
		MaxRetries: 3, MaxTaskChars: 4000, LLM: c}
	if ctx, ok := a.Value("context").(string); ok {
		o.Context = &ctx
	}
	r, err := envgen.Generate(o)
	if err != nil {
		var pe *envgen.PyError
		if errors.As(err, &pe) {
			return nil, &toolError{pe.Msg}
		}
		return nil, refusef("%s", short(err))
	}
	return tracejournal.JSONMode(r.Envelope), nil
}

func (s *mcpServer) findPathway(a *pyjson.Object) (any, error) {
	m, err := s.builtMatcher()
	if err != nil {
		return nil, err
	}
	store, err := s.pathwayStore()
	if err != nil {
		return nil, err
	}
	r, err := store.Find(a.Value("task").(string), m.Key, s.e.potionEnv(), -1)
	if err != nil {
		return nil, refusef("a pathway store this binary does not read (%v)", short(err))
	}
	if r.Match == nil {
		return nil, nil
	}
	return pyjson.NewObject().Set("similarity", r.Match.Similarity).
		Set("pathway", tracejournal.JSONMode(r.Match.Pathway.Full().Obj)), nil
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *mcpServer) recall(a *pyjson.Object) (any, error) {
	store, err := s.pathwayStore()
	if err != nil {
		return nil, err
	}
	env, err := validate("Envelope", pmodel.Envelope, a.Value("envelope"))
	if err != nil {
		return nil, err
	}
	z3, ok := intArg(a, "z3_timeout_ms")
	if !ok || z3 <= 0 || z3 > math.MaxInt32 {
		// Z3 reads a timeout of 0 or less as none, and the oracle passes
		// what it is given (MCP-6).
		return nil, refusef("a z3_timeout_ms that is not a positive 32-bit count")
	}
	m, err := s.builtMatcher()
	if err != nil {
		return nil, err
	}
	r, err := recall.Recall(store, m.Key, s.e.potionEnv(), a.Value("task").(string),
		json.RawMessage(pathways.DumpJSON(env)), int(z3), s.e.llmClient(), s.model)
	if err != nil {
		return nil, refusef("%s", short(err))
	}
	var prov any
	if p := r.Provenance; p != nil {
		prov = pyjson.NewObject().Set("pathway_id", p.PathwayID).Set("similarity", p.Similarity).
			Set("tier", p.Tier).Set("source_trace_count", p.SourceTraceCount).Set("distilled_at", p.DistilledAt).
			Set("hit_count", p.HitCount)
	}
	return pyjson.NewObject().Set("hit", r.Hit).Set("reason", strOrNil(r.Reason)).
		Set("plan", tracejournal.JSONMode(r.Plan)).Set("provenance", prov), nil
}

func (s *mcpServer) recallAnswer(a *pyjson.Object) (any, error) {
	maxAge := a.Value("max_age_seconds").(float64)
	if math.IsNaN(maxAge) || math.IsInf(maxAge, 0) {
		return nil, &toolError{"max_age_seconds must be a finite number, got " + pmodel.FloatRepr(maxAge)}
	}
	entries, err := gateway.LoadAnswers(filepath.Join(s.dataDir, "gateway", "answers.jsonl"))
	if err != nil {
		return nil, refusef("an answer store this binary does not read (%v)", short(err))
	}
	var ground *string
	if g, ok := a.Value("current_ground_hash").(string); ok {
		ground = &g
	}
	var emb pathways.Embedder
	threshold := 0.0
	for _, e := range entries {
		if pyjson.Truthy(e.Signature) && pyjson.Truthy(e.Answer) {
			m, err := s.builtMatcher()
			if err != nil {
				return nil, err
			}
			if emb, err = m.Embedder(); err != nil {
				return nil, refusef("%s", short(err))
			}
			threshold = m.Threshold
			break
		}
	}
	now := float64(time.Now().UnixNano()) / 1e9
	r, err := recall.RecallAnswer(a.Value("task").(string), entries, now, emb, threshold, maxAge, ground)
	if err != nil {
		return nil, refusef("%s", short(err))
	}
	var prov any
	if p := r.Provenance; p != nil {
		prov = pyjson.NewObject().Set("similarity", p.Similarity).Set("age_seconds", p.AgeSeconds).
			Set("created_at", p.CreatedAt).Set("ground_hash", p.GroundHash)
	}
	return pyjson.NewObject().Set("hit", r.Hit).Set("reason", strOrNil(r.Reason)).Set("answer", r.Answer).
		Set("provenance", prov), nil
}

// verifyWhole is Daisugi.verify: the whole-plan verify, failing closed on
// a Z3 check that did not finish (MCP-5).
func verifyWhole(plan, env *pyjson.Object) (*pyjson.Object, error) {
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		return nil, refusef("an envelope the verifier does not read (%v)", short(err))
	}
	if why := verify.Stage2Refusal(venv); why != "" {
		return nil, refusef("%s", why)
	}
	vplan, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
	if err != nil {
		return nil, refusef("a plan the verifier does not read (%v)", short(err))
	}
	t0 := time.Now()
	r := verifyPlan(vplan, venv, verify.VerifyOptions{Z3TimeoutMs: 500})
	d := float64(time.Since(t0).Nanoseconds()) / 1e6
	if len(r.Timeouts) > 0 {
		r.OK = false
		r.Violations = append(r.Violations, verify.V("z3", r.Timeouts[0]).With(pyjson.NewObject(), nil))
	}
	dump, why := supervise.VerificationDump(r, env.Value("id"), plan.Value("id"), d)
	if why != "" {
		return nil, refusef("%s", why)
	}
	return dump, nil
}

func (s *mcpServer) verifyPlan(a *pyjson.Object) (any, error) {
	plan, err := validate("ActionPlan", pmodel.ActionPlan, a.Value("plan"))
	if err != nil {
		return nil, err
	}
	env, err := validate("Envelope", pmodel.Envelope, a.Value("envelope"))
	if err != nil {
		return nil, err
	}
	return verifyWhole(plan, env)
}

const missingPermissions = "envelope dict is missing 'permissions'; pass an explicit " +
	"Permission block (all-default is not a safe fallback)"

func (s *mcpServer) verifyCompletedStep(a *pyjson.Object) (any, error) {
	raw := a.Value("envelope").(*pyjson.Object)
	if _, has := raw.Get("permissions"); !has {
		return nil, &toolError{missingPermissions}
	}
	var task any = ""
	if t, has := raw.Get("task"); has {
		task = t
	}
	plan, err := validate("ActionPlan", pmodel.ActionPlan, pyjson.NewObject().Set("source", "stage2-mcp").
		Set("task", task).Set("steps", []any{a.Value("step")}))
	if err != nil {
		return nil, err
	}
	env, err := validate("Envelope", pmodel.Envelope, raw)
	if err != nil {
		return nil, err
	}
	vs, err := stage2Violations(plan.Value("steps").([]any)[0].(*pyjson.Object), env)
	if err != nil {
		return nil, err
	}
	return pyjson.NewObject().Set("violations", vs), nil
}

func violation(message string, detail *pyjson.Object) *pyjson.Object {
	return pyjson.NewObject().Set("stage", "stage2").Set("message", message).Set("detail", detail).
		Set("suggested_remediation", nil)
}

// pyEqualsInt is Python's rc == expected for a JSON value and an int.
func pyEqualsInt(v any, want pyjson.Int) bool {
	w, ok := new(big.Rat).SetString(want.Text)
	if !ok {
		return false
	}
	switch x := v.(type) {
	case bool:
		if x {
			return w.Cmp(big.NewRat(1, 1)) == 0
		}
		return w.Sign() == 0
	case pyjson.Int:
		r, ok := new(big.Rat).SetString(x.Text)
		return ok && r.Cmp(w) == 0
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
		return new(big.Rat).SetFloat64(f).Cmp(w) == 0
	}
	return false
}

// stage2Violations is stage2.verify_completed_step over one validated
// step, each violation as model_dump(mode="json") gives it. An expr it
// does not evaluate the oracle's way is refused.
func stage2Violations(step, env *pyjson.Object) ([]any, error) {
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		return nil, refusef("an envelope the verifier does not read (%v)", short(err))
	}
	pseudo := pyjson.NewObject().Set("id", "plan_00000000").Set("source", "stage2").Set("task", env.Value("task")).
		Set("steps", []any{step})
	vplan, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(pseudo)))
	if err != nil || len(vplan.Steps) != 1 {
		return nil, refusef("a step the verifier does not read")
	}
	strict := verify.ResolveStrict(nil, venv)
	stepID := step.Value("id")
	out := []any{}
	pcs, _ := env.Value("postconditions").([]any)
	for i, x := range pcs {
		pc := x.(*pyjson.Object)
		if pc.Value("enforce") != true {
			continue
		}
		typ := pc.Value("type").(string)
		expr := pc.Value("expr")
		if expr == nil {
			switch typ {
			case "exit_code", "file_exists", "file_size_range":
				ok, detail, err := stage2Handler(typ, pc, step)
				if err != nil {
					return nil, err
				}
				if !ok {
					d := pyjson.NewObject().Set("postcondition", typ).Set("step_id", stepID)
					for _, k := range detail.Keys() {
						d.Set(k, detail.Value(k))
					}
					out = append(out, violation(fmt.Sprintf("postcondition '%s' violated on completed step %s",
						typ, stepID), d))
				}
				continue
			}
			if strict {
				out = append(out, violation(fmt.Sprintf("postcondition '%s' declares a safety property with no "+
					"verifiable expr; cannot be discharged under strict mode", typ),
					pyjson.NewObject().Set("postcondition", typ).Set("reason", "opaque_unrecognized").
						Set("step_id", stepID).Set("suggested_remediation", "add an `expr` to make it verifiable, "+
						"or set enforce=False to keep it as documentation")))
			}
			continue
		}
		if _, isDict := expr.(*pyjson.Object); !isDict {
			return nil, refusef("postcondition '%s' has an expr that is not a dict", typ)
		}
		text := pathways.DumpJSON(expr)
		if strings.Contains(text, `"llm_check"`) || strings.Contains(text, `"alias"`) {
			return nil, refusef("postcondition '%s' asks a model or names an alias", typ)
		}
		pexpr, err := verify.ParseExpression(venv.Postconditions[i].Expr)
		if err != nil {
			return nil, refusef("postcondition '%s' has an expr this binary does not read", typ)
		}
		ok, err := verify.EvaluatePredicate(pexpr, vplan, venv)
		if err != nil {
			return nil, refusef("postcondition '%s' meets an evaluation error this binary does not word", typ)
		}
		if !ok {
			out = append(out, violation(fmt.Sprintf("postcondition '%s' violated on completed step %s", typ, stepID),
				pyjson.NewObject().Set("postcondition", typ).Set("description", pc.Value("description")).
					Set("step_id", stepID)))
		}
	}
	return out, nil
}

// stage2Handler is one of _OPAQUE_POSTCONDITION_HANDLERS: whether the
// postcondition is discharged, and its detail.
func stage2Handler(typ string, pc, step *pyjson.Object) (bool, *pyjson.Object, error) {
	reason := func(r string) (bool, *pyjson.Object, error) {
		return false, pyjson.NewObject().Set("reason", r), nil
	}
	path, _ := pc.Value("path").(string)
	if strings.ContainsRune(path, 0) {
		return false, nil, refusef("a postcondition path with a NUL, whose handler error this binary does not word")
	}
	switch typ {
	case "exit_code":
		md, _ := step.Value("metadata").(*pyjson.Object)
		var rc any
		if md != nil {
			rc = md.Value("rc")
		}
		if rc == nil {
			return reason("step metadata missing rc")
		}
		want, ok := pc.Value("expected").(pyjson.Int)
		if !ok {
			return reason("exit_code postcondition missing `expected`")
		}
		return pyEqualsInt(rc, want), pyjson.NewObject().Set("observed_rc", rc).Set("expected", want), nil
	case "file_exists":
		if path == "" {
			return reason("file_exists postcondition missing `path`")
		}
		_, err := os.Stat(path)
		exists := err == nil
		return exists, pyjson.NewObject().Set("path", path).Set("exists", exists), nil
	}
	// file_size_range
	if path == "" {
		return reason("file_size_range postcondition missing `path`")
	}
	lo, hasLo := pc.Value("min").(pyjson.Int)
	hi, hasHi := pc.Value("max").(pyjson.Int)
	if !hasLo && !hasHi {
		return reason("file_size_range postcondition needs at least one of `min` / `max`; otherwise it constrains nothing")
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, pyjson.NewObject().Set("path", path).Set("reason", "file does not exist"), nil
	}
	size := big.NewInt(st.Size())
	var loV any = pyjson.Int{Text: "0"}
	ok := true
	if hasLo {
		loV = lo
		l, _ := new(big.Int).SetString(lo.Text, 10)
		ok = l.Cmp(size) <= 0
	}
	var hiV any // inf: None in the JSON dump
	if hasHi {
		hiV = hi
		h, _ := new(big.Int).SetString(hi.Text, 10)
		ok = ok && size.Cmp(h) <= 0
	}
	return ok, pyjson.NewObject().Set("path", path).Set("size", pyjson.Int{Text: size.String()}).
		Set("min", loV).Set("max", hiV), nil
}

func (s *mcpServer) runPlan(a *pyjson.Object) (any, error) {
	plan, err := validate("ActionPlan", pmodel.ActionPlan, a.Value("plan"))
	if err != nil {
		return nil, err
	}
	env, err := validate("Envelope", pmodel.Envelope, a.Value("envelope"))
	if err != nil {
		return nil, err
	}
	if _, set := s.e.env["OPENDAISUGI_MCP_RUN_TIMEOUT"]; set {
		return nil, refusef("OPENDAISUGI_MCP_RUN_TIMEOUT set")
	}
	dry, _ := a.Value("dry_run").(bool)
	pre, why := prepare(plan, env)
	if why != "" {
		return nil, refusef("%s", why)
	}
	j, err := s.theJournal()
	if err != nil {
		return nil, err
	}
	var fallback supervise.Fallback
	if fs, _ := env.Value("fallback").(*pyjson.Object); fs != nil && fs.Value("strategy") == "tier2_recompute" {
		fallback = supervise.Recompute(s.e.llmClient(), env, pre.venv, 500)
	}
	var executors map[string]supervise.Executor
	var approval supervise.Approver
	if dry {
		executors = map[string]supervise.Executor{}
		for _, x := range plan.Value("steps").([]any) {
			executors[x.(*pyjson.Object).Value("type").(string)] = supervise.DryRun{}
		}
		approval = supervise.Always{}
	}
	if len(executors) == 0 {
		executors = supervise.DefaultExecutors()
		executors["shell"] = supervise.Shell{Environ: s.e.Environ}
	}
	if !dry {
		// The real approval gate; stdin is the MCP stream, never a
		// terminal, so no prompt is read from it.
		approval = supervise.Default{Getenv: s.e.lookup, Stdin: strings.NewReader(""), Stdout: io.Discard,
			Terminal: func() bool { return false }}
	}
	sup := &supervise.Supervisor{Executors: executors, Journal: j, Z3TimeoutMs: 500, StepTimeoutS: 30,
		MaxOutputBytes: 10 * 1024 * 1024, Fallback: fallback, Approval: approval}
	sess := sup.Run(pre.plan, pre.env, pre.venv, pre.verification)
	if sup.LogErr != nil {
		return nil, refusef("a journal write this binary does not make the oracle's way (%v)", short(sup.LogErr))
	}
	rows, err := j.Receipts(sess.ID)
	if err != nil {
		return nil, refusef("a journal this binary does not read (%v)", short(err))
	}
	receipts := []any{}
	for _, r := range rows {
		receipts = append(receipts, pyjson.NewObject().Set("step_id", r.StepID).Set("timestamp", r.Timestamp).
			Set("evidence_hash", r.EvidenceHash).Set("verify_result", r.VerifyResult).Set("model_id", r.ModelID))
	}
	var integrity, failed any
	if sess.IntegrityPassed != nil {
		integrity = *sess.IntegrityPassed
	}
	if sess.FailedStepID != nil {
		failed = *sess.FailedStepID
	}
	return pyjson.NewObject().Set("run_id", sess.ID).Set("status", sess.Status).Set("integrity_passed", integrity).
		Set("failed_step_id", failed).Set("receipts", receipts), nil
}

// delegate is the delegate tool: the router's worker reads
// the file and answers; each quote is checked against the file.
func (s *mcpServer) delegate(a *pyjson.Object) (any, error) {
	c := s.e.llmClient()
	out, err := delegate.Run(a.Value("path").(string), a.Value("question").(string), a.Value("mode").(string),
		s.dataDir, s.e.lookup, c.CompleteAt)
	if err != nil {
		return nil, refusef("%v", err)
	}
	return out, nil
}
