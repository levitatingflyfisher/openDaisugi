package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/orchestrate"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// orchInt is click.INT on an option: the value, or click's usage error.
func orchInt(name, v string) (int64, error) {
	n, ok := pyInt(v)
	if !ok {
		return 0, &usageError{fmt.Sprintf("Invalid value for %s: %s is not a valid integer.", name, pystr.Repr(v))}
	}
	return n, nil
}

// orchestrateCmd is `daisugi orchestrate PROMPT`: decompose, size,
// supervised execute, synthesize.
func (e *Env) orchestrateCmd(args []string) error {
	const cmd = "orchestrate"
	opts := []opt{
		{names: []string{"--envelope", "-e"}, value: true, metavar: "PATH", help: "Envelope YAML (authorization boundary). If omitted, one is generated for the prompt."},
		{names: []string{"--budget", "-b"}, value: true, metavar: "INTEGER", help: "Approximate token budget for the run (gates routing during execution). Omit for unbudgeted."},
		{names: []string{"--strict-budget"}, help: "Stop when the budget is exhausted instead of downgrading to a cheaper model."},
		{names: []string{"--deterministic-synthesis"}, help: "Assemble the final answer from step outputs deterministically instead of with an LLM."},
		{names: []string{"--max-parallel"}, value: true, metavar: "INTEGER", help: "Run independent steps in the same dependency level concurrently, up to this many at once (1 = sequential, the default)."},
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "Model used to decompose the prompt (and generate the envelope if none is given)."},
		{names: []string{"--llm"}, value: true, metavar: "TEXT", help: "LLM backend: api | claude-code. Default: auto-detect."},
		{names: []string{"--stakes"}, value: true, metavar: "TEXT", help: "Stakes for a generated envelope: low|medium|high."},
		{names: []string{"--cost"}, help: "Show a cost figure for the run."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data dir (pathway store + journal)."},
		{names: []string{"--json"}, help: "Emit the orchestration result as JSON."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "PROMPT", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PROMPT", "Run PROMPT end to end: decompose → size → supervised execute → synthesize.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "PROMPT", &usageError{"Missing argument 'PROMPT'."})
	}
	prompt := p.args[0]
	var budget *int64
	if p.has("--budget") {
		n, err := orchInt("'--budget' / '-b'", p.str("--budget", ""))
		if err != nil {
			return e.usageArgs(cmd, "PROMPT", err)
		}
		budget = &n
	}
	parallel := int64(1)
	if p.has("--max-parallel") {
		if parallel, err = orchInt("'--max-parallel'", p.str("--max-parallel", "")); err != nil {
			return e.usageArgs(cmd, "PROMPT", err)
		}
	}
	stakes := p.str("--stakes", "medium")
	if stakes != "low" && stakes != "medium" && stakes != "high" {
		e.errf("Invalid --stakes %s; choose from ['high', 'low', 'medium'].\n", pystr.Repr(stakes))
		return exit(2)
	}
	if p.has("--llm") {
		v := p.str("--llm", "")
		if err := e.checkLLMFlag(v); err != nil {
			return err
		}
		e.env["OPENDAISUGI_LLM_BACKEND"] = v
		e.Environ = append(e.Environ, "OPENDAISUGI_LLM_BACKEND="+v)
	}
	if parallel > 1 {
		return e.notYet("daisugi orchestrate --max-parallel above 1")
	}
	model := p.str("--model", orchestrate.DefaultDecomposeModel)
	dataDir := gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
	if err := e.renamedBackendAt(cmd, dataDir); err != nil {
		return err
	}
	c := e.llmClient()
	if err := c.Check(model); err != nil {
		return e.refuse(cmd, err)
	}
	m, notBuilt, err := e.matcher()
	if err != nil {
		return e.refuse(cmd, err)
	}
	if notBuilt != "" {
		return e.refuse(cmd, fmt.Errorf("matcher_model %s is not a built embedder", pystr.Repr(notBuilt)))
	}
	// Read the envelope before anything is said, so a refusal changes
	// nothing and says one line.
	var env *pyjson.Object
	var envMissing bool
	var envErr string
	envPath := p.str("--envelope", "")
	if p.has("--envelope") {
		if _, err := os.Stat(envPath); err != nil {
			envMissing = true
		} else {
			var refusal string
			env, envErr, refusal = loadModelYAML(envPath, "Envelope", pmodel.Envelope)
			if refusal != "" {
				return e.refuse(cmd, fmt.Errorf("%s", refusal))
			}
		}
	}
	if env != nil {
		ve, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
		if err != nil {
			return e.refuse(cmd, err)
		}
		if why := verify.Stage2Refusal(ve); why != "" {
			return e.refuse(cmd, fmt.Errorf("%s", why))
		}
	}
	// A stored pathway embedded under another model makes find warn
	// (a UserWarning this binary does not print): refused here, before
	// anything is written.
	if warn, err := findWarning(filepath.Join(dataDir, "pathways.db"), prompt, m.Key, e); err != nil {
		return e.refuse(cmd, err)
	} else if warn {
		return e.refuse(cmd, fmt.Errorf("the pathway store warns of stale embeddings, a warning this binary does not print yet"))
	}
	if err := e.echoResolvedAt(dataDir); err != nil {
		return e.refuse(cmd, err)
	}
	if envMissing {
		return e.fail3("Tried to read the envelope "+envPath+".", "The file does not exist.", "Check the path.", 2)
	}
	if envErr != "" {
		return e.fail3("Tried to read the envelope "+envPath+".", "It did not parse: "+envErr, "Fix the file and run again.", 2)
	}
	// Daisugi(model=..., data_dir=...): the envelope cache, then the
	// pathway store and the journal the run reads and writes.
	cache, err := envgen.OpenCache(filepath.Join(dataDir, "envelope_cache.db"))
	if err != nil {
		return e.fail(cmd, err)
	}
	store, err := pathways.Open(filepath.Join(dataDir, "pathways.db"))
	if err != nil {
		return e.storeErr(cmd, err)
	}
	defer store.Close()
	j, err := e.openJournal(cmd, dataDir)
	if err != nil {
		return err
	}
	defer j.Close()
	if env == nil {
		o := envgen.Options{Task: prompt, Models: []string{model}, Single: true, Stakes: stakes, Thinking: "standard",
			Cache: cache, Store: store, MatcherKey: m.Key, Potion: e.potionEnv(), Journal: j,
			MaxRetries: 3, MaxTaskChars: 4000, LLM: c}
		r, err := envgen.Generate(o)
		if err != nil {
			return e.orchestrateErr(cmd, err)
		}
		env = r.Envelope
	}
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		return e.orchestrateErr(cmd, fmt.Errorf("the generated envelope does not read: %v: %w", err, orchestrate.ErrUnported))
	}
	if why := verify.Stage2Refusal(venv); why != "" {
		// A generated envelope: the data directory is set up (K2-6).
		return e.orchestrateErr(cmd, fmt.Errorf("%s: %w", why, orchestrate.ErrUnported))
	}
	var fallback supervise.Fallback
	if fs, _ := env.Value("fallback").(*pyjson.Object); fs != nil && fs.Value("strategy") == "tier2_recompute" {
		fallback = supervise.Recompute(c, env, venv, 500)
	}
	res, err := orchestrate.Run(orchestrate.Options{LLM: c, Prompt: prompt, Env: env, VEnv: venv, Budget: budget,
		StrictBudget: p.flag("--strict-budget"), SynthLLM: !p.flag("--deterministic-synthesis"), Store: store,
		MatcherKey: m.Key, Potion: e.potionEnv(), Threshold: -1, Journal: j, DecomposeModel: model, Environ: e.Environ,
		Z3TimeoutMs: 500, StepTimeoutS: 180, Ladder: orchestrate.BuildLadder(""), Fallback: fallback})
	if err != nil {
		var de *orchestrate.DecompositionError
		if errors.As(err, &de) && de.NoSteps {
			if p.flag("--json") {
				e.out("%s\n", pyjson.Dumps(pyjson.NewObject().Set("status", "no-steps").Set("prompt", prompt), false))
			} else {
				e.out("This prompt has no steps to run.\n")
				e.out("daisugi orchestrate runs multi-step tasks under a verified envelope. " +
					"For a plain question, ask your agent directly.\n")
			}
			return nil
		}
		return e.orchestrateErr(cmd, err)
	}
	if res.LogError != nil {
		return e.fail(cmd, res.LogError)
	}
	status := res.Session.Status
	if p.flag("--json") {
		sizings := make([]any, len(res.Sizings))
		for i, s := range res.Sizings {
			sizings[i] = s.Dump()
		}
		steps := make([]any, len(res.Session.Steps))
		for i, o := range res.Session.Steps {
			var rc any
			if o.RC != nil {
				rc = supervise.PyInt(*o.RC)
			}
			var errText any
			if o.Error != nil {
				errText = *o.Error
			}
			steps[i] = pyjson.NewObject().Set("step_id", o.StepID).Set("status", o.Status).Set("rc", rc).Set("error", errText)
		}
		payload := pyjson.NewObject().Set("prompt", prompt).Set("status", status).Set("final_answer", res.Answer).
			Set("reused_pathway", res.Reused).Set("used_llm_synthesis", res.UsedLLM).Set("budget", res.Budget.Dump()).
			Set("sizings", sizings).Set("steps", steps).Set("plan", tracejournal.JSONMode(res.Plan))
		e.out("%s\n", pyjson.DumpsIndent(payload, 2, true))
	} else {
		e.echo("%s\n\n", res.Answer)
		reused := ""
		if res.Reused {
			reused = ", reused pathway"
		}
		e.echo("— orchestration (%s%s) —\n", status, reused)
		if status != supervise.Succeeded {
			for _, o := range res.Session.Steps {
				if (o.Status == supervise.Failed || o.Status == supervise.Aborted || o.Status == "rejected_halted") &&
					o.Error != nil && *o.Error != "" {
					e.echoErr("  %s: %s — %s\n", o.StepID, o.Status, *o.Error)
				}
			}
		}
		for _, s := range res.Sizings {
			down := ""
			if s.Downgraded {
				down = "  [downgraded]"
			}
			e.echo("  %s: difficulty=%s → %s (%s)%s\n", s.StepID, strconv.FormatFloat(s.Difficulty, 'f', 2, 64),
				s.Tier, s.Model, down)
		}
		b := res.Budget
		spent := strconv.FormatInt(b.Spent, 10)
		if b.Total != nil {
			spent += "/" + strconv.FormatInt(*b.Total, 10)
		}
		e.echo("  budget: %s tokens across %d model call(s)\n", spent, b.StepCount)
		if p.flag("--cost") {
			if b.Measured != nil {
				e.echo("  cost:   $%s (exact — Claude Code accounting)\n", strconv.FormatFloat(*b.Measured, 'f', 4, 64))
			} else {
				e.echo("  cost:   ~$%s (estimated)\n", strconv.FormatFloat(b.ApproxF, 'f', 4, 64))
			}
		}
	}
	if status != supervise.Succeeded {
		return exit(1)
	}
	return nil
}

// orchestrateErr is orchestrate_cmd's exception handling.
func (e *Env) orchestrateErr(cmd string, err error) error {
	var de *orchestrate.DecompositionError
	var nc *orchestrate.NotConfigured
	var pe *envgen.PyError
	switch {
	case errors.As(err, &de):
		return e.fail3("Tried to decompose the prompt into a plan.", de.Msg,
			"Rephrase the prompt, or check the backend with `daisugi config`.", 1)
	case errors.As(err, &nc):
		e.errf("%s\n", pystr.Strip(nc.Msg))
		return exit(1)
	case errors.As(err, &pe):
		switch pe.Class {
		case "EnvelopeGenerationError", "ModelCallError", "ModelLadderExhausted":
			return e.fail3("Tried to generate the envelope.", pe.Msg, "Check the backend with `daisugi config`.", 1)
		case "LLMNotConfigured", "LowStakesNotConfigured", "TaskTooLongError":
			e.errf("%s\n", pystr.Strip(pe.Msg))
			return exit(1)
		}
		return e.fail3("Tried to orchestrate the prompt.", pe.Class+": "+pe.Msg,
			"Run again with DAISUGI_DEBUG=1 and file the traceback as a bug.", 1)
	case errors.Is(err, orchestrate.ErrUnported), errors.Is(err, envgen.ErrUnported):
		// The data directory is set up by now (K2-6): say so, not
		// "Nothing was changed."
		e.errf("daisugi %s: %v. The envelope cache, pathway store and journal were made; no step ran.\n", cmd, err)
		return exit(2)
	}
	return e.fail(cmd, err)
}

// findWarning reports whether PathwayStore.find(prompt) would warn of
// stale embeddings, read without writing. A store that is not there, or
// has no table, does not warn.
func findWarning(db, prompt, key string, e *Env) (bool, error) {
	if _, err := os.Stat(db); err != nil {
		return false, nil
	}
	s, err := pathways.OpenReadOnly(db)
	if err != nil {
		return false, err
	}
	defer s.Close()
	r, err := s.Find(prompt, key, e.potionEnv(), -1)
	if err != nil {
		if errors.Is(err, pathways.ErrUnreadable) || errors.Is(err, pathways.ErrNotCarried) {
			return false, err
		}
		return false, nil
	}
	return r.Warning != "", nil
}
