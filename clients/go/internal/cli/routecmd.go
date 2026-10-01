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
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
	p, err := parseArgs(args, opts, 1)
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
	if p.flag("--json") {
		o := pyjson.NewObject().Set("tier", a.tier).Set("model", a.model).Set("reason", a.reason).Set("difficulty", d)
		if a.pathway != "" {
			o.Set("pathway_id", a.pathway)
		} else {
			o.Set("pathway_id", nil)
		}
		o.Set("advisor_pairing", a.pairing)
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
	return nil
}
