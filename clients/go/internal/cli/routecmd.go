package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// thresholds is each carried backend's reuse threshold, as
// _search.active_threshold gives it.
var thresholds = map[string]float64{"potion": 0.59, "all-MiniLM-L6-v2": 0.55, "lexical": 0.25, "int8": 0.55}

// route is `daisugi route`: RouteAdvisor.advise for one task.
func (e *Env) route(args []string) error {
	const cmd = "route"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory (pathway store)."},
		{names: []string{"--cheap-model"}, value: true, metavar: "TEXT", help: "Model recommended for easy tasks."},
		{names: []string{"--frontier-model"}, value: true, metavar: "TEXT", help: "Model recommended for hard tasks."},
		{names: []string{"--threshold"}, value: true, metavar: "FLOAT", help: "Pathway-match threshold (0-1); default: active backend's."},
		{names: []string{"--harness"}, value: true, metavar: "TEXT", help: "Host harness: claude-code, codex, ollama/local, hermes, openclaw."},
		{names: []string{"--knn"}, help: "Also show the knn router's choice and its nearest past turns (reads <data-dir>/gateway/routing.db)."},
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
	p, err := parseArgs(args, opts, 1<<20)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " TASK", "Recommend the cheapest viable model/tier for a task.", opts)
	}
	if len(p.args) == 0 {
		return e.usage(cmd, &usageError{"Missing argument 'task'."})
	}
	task := p.args[0]
	if task == "feedback" {
		return e.routeFeedback(cmd, p)
	}
	if len(p.args) > 1 {
		return e.usage(cmd, &usageError{fmt.Sprintf("Invalid value: unexpected extra argument %s", pystr.Repr(p.args[1]))})
	}
	hasThreshold := p.has("--threshold")
	threshold, err := clickFloat(p, "--threshold", 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	cheap := p.str("--cheap-model", gateway.DefaultCheapModel)
	frontier := p.str("--frontier-model", gateway.DefaultFrontierModel)
	harness := strings.ToLower(pystr.Strip(p.str("--harness", "claude-code")))
	advisorTool := harness == "claude-code" || harness == "claude" || harness == "anthropic"
	db := filepath.Join(dataDir, "pathways.db")
	_, statErr := os.Stat(db)
	hasStore := statErr == nil
	cfg, err := config.Load(filepath.Join(e.dataHome(), "config.yaml"))
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("the config file is not one this binary reads: %w", err))
	}
	if !hasThreshold {
		t, ok := thresholds[cfg.MatcherModel]
		if !ok {
			e.errf("matcher_model=%s is not a built embedder.\n", pystr.Repr(cfg.MatcherModel))
			return exit(1)
		}
		threshold = t
	}
	d := gateway.EstimateDifficulty(task)
	type advice struct {
		tier, model, reason, pathway string
		pairing                      bool
	}
	var a *advice
	if hasStore {
		m, notBuilt, err := e.matcher()
		if err != nil {
			return e.refuse(cmd, err)
		}
		if notBuilt != "" {
			e.errf("matcher_model=%s is not a built embedder.\n", pystr.Repr(notBuilt))
			return exit(1)
		}
		s, err := pathways.Open(db)
		if err != nil {
			return e.storeErr(cmd, err)
		}
		r, ferr := s.Find(task, m.Key, e.potionEnv(), threshold)
		s.Close()
		if ferr != nil && errors.Is(ferr, pathways.ErrUnreadable) {
			return e.refuse(cmd, ferr)
		}
		// The stale-embeddings UserWarning find prints, as Python's
		// warnings module prints it under PYTHONWARNINGS; a filter not
		// modelled is refused (route writes nothing).
		if ferr == nil && r.Warning != "" {
			if err := e.staleOnce(r.Warning); err != nil {
				return e.refuse(cmd, err)
			}
		}
		// A lookup that fails is no match: routing never breaks on the store.
		if ferr == nil && r.Match != nil {
			id := r.Match.Pathway.ID()
			a = &advice{tier: "tier0-pathway", pathway: id, reason: fmt.Sprintf("reuse distilled pathway %s (similarity %s); "+
				"re-verified against its envelope — near-zero LLM cost, provably in policy", id, gateway.FormatF(r.Match.Similarity, 2))}
		}
	}
	if a == nil {
		switch {
		case d < 0.5:
			a = &advice{tier: "tier1-cheap", model: cheap,
				reason: fmt.Sprintf("novel but low-difficulty (%s); cheap model suffices", gateway.FormatF(d, 2))}
		case !advisorTool:
			a = &advice{tier: "tier2-frontier", model: frontier,
				reason: fmt.Sprintf("novel and high-difficulty (%s); use the frontier model", gateway.FormatF(d, 2))}
		default:
			a = &advice{tier: "tier2-frontier", model: frontier, pairing: true,
				reason: fmt.Sprintf("novel and high-difficulty (%s); use the frontier — or pair a cheap executor with an "+
					"Opus advisor (Anthropic advisor tool, beta advisor-tool-2026-03-01) for comparable quality at lower cost",
					gateway.FormatF(d, 2))}
		}
	}
	var view *gateway.KnnView
	if p.flag("--knn") {
		v, err := e.knnView(task, dataDir, frontier, cheap)
		if err != nil {
			return e.refuse(cmd, err)
		}
		view = &v
	}
	if p.flag("--json") {
		o := pyjson.NewObject().Set("tier", a.tier).Set("model", a.model).Set("reason", a.reason).Set("difficulty", d)
		if a.pathway != "" {
			o.Set("pathway_id", a.pathway)
		} else {
			o.Set("pathway_id", nil)
		}
		o.Set("advisor_pairing", a.pairing)
		if view != nil {
			o.Set("knn", knnViewObject(*view))
		}
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	line := "route: " + a.tier
	if a.model != "" {
		line += "  →  " + a.model
	}
	e.out("%s\n", line)
	e.out("  difficulty: %s\n", gateway.FormatF(d, 2))
	if a.pathway != "" {
		e.out("  pathway:    %s\n", a.pathway)
	}
	e.out("  why:        %s\n", a.reason)
	if view != nil {
		for _, ln := range gateway.KnnViewLines(*view) {
			e.out("%s\n", ln)
		}
	}
	return nil
}

// knnView is knn_view: the knn router's choice for a task, read only.
func (e *Env) knnView(task, dataDir, requested, cheap string) (gateway.KnnView, error) {
	k := e.knnRouter(dataDir)
	vec, identity, err := k.Embed(task)
	if err != nil {
		return gateway.KnnView{Err: "could not embed the task"}, nil
	}
	return gateway.KnnViewOf(vec, identity, k.Store, requested, cheap, gateway.DefaultPrices(), k.Threshold)
}

// knnViewObject is the knn key of route --json.
func knnViewObject(v gateway.KnnView) any {
	if v.Err != "" {
		return pyjson.NewObject().Set("error", v.Err)
	}
	ns := make([]any, len(v.Neighbours))
	for i, n := range v.Neighbours {
		ns[i] = pyjson.NewObject().Set("turn_id", n.TurnID).Set("similarity", pyjson.Float(n.Similarity)).
			Set("model", n.Model).Set("outcome", n.Outcome)
	}
	ss := make([]any, len(v.Scores))
	for i, s := range v.Scores {
		ss[i] = pyjson.NewObject().Set("model", s.Model).Set("score", pyjson.Float(s.Score))
	}
	return pyjson.NewObject().Set("model", v.Model).Set("fallback", v.Fallback).Set("reason", v.Reason).
		Set("known", pyjson.Int{Text: fmt.Sprint(v.Known)}).Set("threshold", pyjson.Float(v.Threshold)).
		Set("neighbours", ns).Set("scores", ss)
}

// routeFeedback is `daisugi route feedback <turn-id> good|bad`.
func (e *Env) routeFeedback(cmd string, p *parsed) error {
	rest := p.args[1:]
	if len(rest) != 2 {
		return e.usage(cmd, &usageError{"Invalid value: usage: daisugi route feedback <turn-id> good|bad"})
	}
	turnID, outcome := rest[0], rest[1]
	if outcome != gateway.KnnGood && outcome != gateway.KnnBad {
		return e.usage(cmd, &usageError{fmt.Sprintf("Invalid value: the outcome must be good or bad, not %s", pystr.Repr(outcome))})
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	store := &gateway.OutcomeStore{Path: filepath.Join(dataDir, "gateway", "routing.db")}
	ok, err := store.SetOutcome(turnID, outcome, gateway.SourceHuman)
	if err != nil {
		return e.fail(cmd, err)
	}
	if !ok {
		e.errf("no booked turn %s.\nthe knn router books turns in <data-dir>/gateway/routing.db.\n"+
			"see the turn ids with: daisugi router status\n", pystr.Repr(turnID))
		return exit(1)
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.Dumps(pyjson.NewObject().Set("turn_id", turnID).Set("outcome", outcome), true))
	} else {
		e.out("turn %s: outcome set to %s\n", turnID, outcome)
	}
	return nil
}
