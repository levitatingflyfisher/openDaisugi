package orchestrate

import (
	"errors"
	"fmt"
	"time"

	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// ErrUnported is a run this binary cannot finish the oracle's way.
var ErrUnported = errors.New("is not in this binary yet")

// Options are Orchestrator's settings and orchestrate's arguments, as
// the facade and the CLI pass them.
type Options struct {
	LLM    *llm.Client
	Prompt string
	// Env is the envelope's model_dump(); VEnv the verifier's reading.
	Env          *pyjson.Object
	VEnv         verify.Envelope
	Budget       *int64
	StrictBudget bool
	// MaxParallel is max_parallel (1: sequential).
	MaxParallel int
	SynthLLM    bool
	// Store, MatcherKey, Potion and Threshold are Tier-0 reuse.
	Store          *pathways.Store
	MatcherKey     string
	Potion         potion.Env
	Threshold      float64
	Journal        *tracejournal.Journal
	DecomposeModel string
	Z3TimeoutMs    int
	StepTimeoutS   int
	Ladder         Ladder
	// Fallback is the envelope's fallback handler; nil halts.
	Fallback supervise.Fallback
	// Environ is the shell steps' environment (with --llm's setting).
	Environ []string
	// Warn prints the store's stale-embeddings warning (once per process);
	// nil prints nothing.
	Warn func(string)
	// Agentic runs agentic steps (a reused delegated pathway has them);
	// nil leaves them with no executor.
	Agentic supervise.Executor
}

// Result is orchestrator.OrchestrationResult.
type Result struct {
	Prompt   string
	Plan     *pyjson.Object
	Session  *supervise.Session
	Answer   string
	Sizings  []Sizing
	Budget   Report
	Reused   bool
	UsedLLM  bool
	LogError error
}

// maybeReuse is Orchestrator._maybe_reuse: a distilled pathway's plan for
// the prompt, bound when it is typed, or nil.
func (o *Options) maybeReuse() *pyjson.Object {
	if o.Store == nil {
		return nil
	}
	r, err := o.Store.Find(o.Prompt, o.MatcherKey, o.Potion, o.Threshold)
	if o.Warn != nil {
		o.Warn(r.Warning)
	}
	if err != nil || r.Match == nil {
		return nil
	}
	p := r.Match.Pathway
	if params, ok := p.Obj.Value("parameters").([]any); ok && len(params) > 0 {
		return envgen.Bind(o.LLM, p.Obj, o.Prompt, o.VEnv, o.DecomposeModel, o.Z3TimeoutMs)
	}
	return envgen.DeepCopy(p.Obj.Value("plan_template").(*pyjson.Object))
}

// Run is Orchestrator.orchestrate.
func Run(o Options) (*Result, error) {
	tracker := &Tracker{Total: o.Budget, Strict: o.StrictBudget}
	reused := false
	plan := o.maybeReuse()
	if plan != nil {
		if envgen.PlanVerifies(plan, o.VEnv, o.Z3TimeoutMs) {
			reused = true
		} else {
			plan = nil
		}
	}
	if !reused {
		var err error
		if plan, err = Decompose(o.LLM, o.Prompt, o.DecomposeModel, o.VEnv, o.Z3TimeoutMs); err != nil {
			return nil, err
		}
	}
	steps := tracejournal.Steps(plan)
	planned := SizePlan(steps, o.Ladder)
	for i, s := range steps {
		if str(s, "type") == "task" {
			s.Set("preferred_model", planned[i].Model)
		}
	}
	task := &TaskExecutor{LLM: o.LLM, Tracker: tracker, Ladder: o.Ladder}
	executors := supervise.DefaultExecutors()
	executors["shell"] = supervise.Shell{Environ: o.Environ}
	executors["task"] = task
	executors["skill"] = SkillExecutor{}
	executors["mcp"] = MCPExecutor{}
	if o.Agentic != nil {
		executors["agentic"] = o.Agentic
	}
	vp, err := verify.ParsePlan([]byte(pathways.DumpJSON(plan)))
	if err != nil {
		return nil, fmt.Errorf("the plan does not read: %v: %w", err, ErrUnported)
	}
	t0 := time.Now()
	vr := verifyPlan(vp, o.VEnv, verify.VerifyOptions{Z3TimeoutMs: o.Z3TimeoutMs})
	d := float64(time.Since(t0).Nanoseconds()) / 1e6
	if len(vr.Timeouts) > 0 && vr.OK {
		vr.OK = false
		vr.Violations = append(vr.Violations, verify.V("z3", vr.Timeouts[0]).With(pyjson.NewObject(), nil))
	}
	verification, why := supervise.VerificationDump(vr, o.Env.Value("id"), plan.Value("id"), d)
	if why != "" {
		return nil, fmt.Errorf("%s: %w", why, ErrUnported)
	}
	sup := &supervise.Supervisor{Executors: executors, Approval: supervise.Always{}, Journal: o.Journal,
		Z3TimeoutMs: o.Z3TimeoutMs, StepTimeoutS: o.StepTimeoutS, MaxOutputBytes: 10 * 1024 * 1024,
		Fallback: o.Fallback, MaxParallel: o.MaxParallel}
	sess := sup.Run(plan, o.Env, o.VEnv, verification)
	realized := map[string]Sizing{}
	for _, s := range task.Live {
		realized[s.StepID] = s
	}
	sizings := make([]Sizing, len(planned))
	for i, s := range planned {
		if r, ok := realized[s.StepID]; ok {
			sizings[i] = r
		} else {
			sizings[i] = s
		}
	}
	useLLM := o.SynthLLM && !tracker.Exhausted()
	answer, used := Synthesize(o.LLM, o.Prompt, sess, steps, useLLM)
	return &Result{Prompt: o.Prompt, Plan: plan, Session: sess, Answer: answer, Sizings: sizings,
		Budget: tracker.Report(), Reused: reused, UsedLLM: used, LogError: sup.LogErr}, nil
}
