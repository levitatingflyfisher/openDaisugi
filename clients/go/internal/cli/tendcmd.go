package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/distill"
	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
)

func (e *Env) lookup(k string) (string, bool) {
	v, ok := e.env[k]
	return v, ok
}

func (e *Env) potionEnv() potion.Env {
	return potion.Env{Lookup: e.lookup, Home: e.home, Notice: func(s string) { e.errf("%s\n", s) }}
}

func (e *Env) llmClient() *llm.Client {
	return llm.New(llm.Env{Getenv: e.lookup, Home: e.home, Stdout: e.Stdout, Stderr: e.Stderr,
		LookPath: func(name string) (string, error) { return lookPath(name, e.env["PATH"]) }})
}

// lookPath is shutil.which over a PATH value.
func lookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// matcher is the configured matcher, or the reason tend cannot run.
// refuse is set for a matcher this binary does not carry (PW-1);
// notBuilt for a key nothing builds, which Python raises on.
func (e *Env) matcher() (m *pathways.Matcher, notBuilt string, err error) {
	cfg, err := config.Load(filepath.Join(e.home, ".opendaisugi", "config.yaml"))
	if err != nil {
		return nil, "", fmt.Errorf("the config file is not one this binary reads: %w", err)
	}
	m, err = pathways.SelectMatcher(cfg.MatcherModel, e.potionEnv())
	if err != nil {
		return nil, "", err
	}
	if m == nil {
		return nil, cfg.MatcherModel, nil
	}
	return m, "", nil
}

func (e *Env) tend(args []string) error {
	const cmd = "tend"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "Model used for template generalization + improvement."},
		{names: []string{"--min-traces"}, value: true, metavar: "INTEGER", help: "Minimum cluster size to distill."},
		{names: []string{"--lookback-days"}, value: true, metavar: "INTEGER", help: "How far back to scan the journal."},
		{names: []string{"--dry-run"}, help: "Run the pipeline but do not store pathways."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Run the distiller. Scans successful traces and produces compiled pathways.", opts)
	}
	minTraces, err := clickInt(p, "--min-traces", 3)
	if err != nil {
		return e.usage(cmd, err)
	}
	lookback, err := clickInt(p, "--lookback-days", 30)
	if err != nil {
		return e.usage(cmd, err)
	}
	model := p.str("--model", "anthropic/claude-sonnet-4-20250514")
	dataDir := p.str("--data-dir", filepath.Join(e.home, ".opendaisugi"))
	dry := p.flag("--dry-run")
	rep, err := e.runTend(dataDir, model, minTraces, lookback, dry)
	if err != nil {
		return err
	}
	e.out("tend complete: created=%d updated=%d skipped=%d in %.1fs\n", rep.Created, rep.Updated, rep.Skipped, rep.seconds)
	if len(rep.Pathways) > 0 {
		e.out("  %d pathway(s): %s\n", len(rep.Pathways), strings.Join(rep.Pathways, ", "))
	}
	for _, w := range rep.Warnings {
		e.out("  warning: %s\n", w)
	}
	return nil
}

// raisedError is an error Daisugi() raises before tend runs: the CLI
// prints its message and exits 1, and auto-tend does not catch it.
type raisedError struct{ msg string }

func (r *raisedError) Error() string { return r.msg }

type tendReport struct {
	distill.Report
	seconds float64
}

// tendState is what the read checks of a tend found, before it writes.
type tendState struct {
	m        *pathways.Matcher // nil for a matcher the binary does not carry
	identity string
	client   *llm.Client
	since    float64
	traces   []tracejournal.Distillable
}

// identityOf is active_model_name for a matcher key.
func identityOf(m *pathways.Matcher, key string) string {
	if m != nil {
		return m.Identity
	}
	if key == "int8" {
		return "all-MiniLM-L6-v2-int8"
	}
	return key
}

// tendCheck is every refusal of a tend run, made before anything is
// written: the matcher, the model, the journal's traces and refinements
// and the store's rows. extra is how many traces the caller will add
// first (auto-tend's conversions). A matcher the binary does not carry is
// refused only when the run would load its embedder: a stale row to
// re-embed, or enough traces to cluster.
func (e *Env) tendCheck(dataDir, model string, minTraces, lookback int64, dry bool, extra int, started float64) (*tendState, error) {
	const cmd = "tend"
	cfg, cerr := config.Load(filepath.Join(e.home, ".opendaisugi", "config.yaml"))
	if cerr != nil {
		return nil, e.refuse(cmd, fmt.Errorf("the config file is not one this binary reads: %w", cerr))
	}
	m, notCarried := pathways.SelectMatcher(cfg.MatcherModel, e.potionEnv())
	if notCarried == nil && m == nil {
		// Daisugi() resolves the matcher's threshold before it makes
		// anything, and raises for a key nothing builds; the CLI prints
		// the error's message and exits 1.
		return nil, &raisedError{"matcher_model=" + pystr.Repr(cfg.MatcherModel) + " is not a built embedder."}
	}
	st := &tendState{m: m, identity: identityOf(m, cfg.MatcherModel), client: e.llmClient(),
		since: started - float64(lookback)*86400}
	if err := st.client.Check(model); err != nil {
		return nil, e.refuse(cmd, err)
	}
	if fi, err := os.Stat(dataDir); err == nil && !fi.IsDir() {
		e.errf("daisugi %s: FileExistsError: %s exists and is not a directory\n", cmd, dataDir)
		return nil, exit(1)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "journal", "index.db")); err == nil {
		// Read only: an old journal is migrated when tend runs, after
		// every refusal.
		j, err := tracejournal.OpenReadOnly(dataDir)
		if err != nil {
			return nil, e.failPy(cmd, err)
		}
		defer j.Close()
		if st.traces, err = j.ListSuccessful(&st.since); err != nil {
			return nil, e.tendErr(err)
		}
		if err := (&distill.Distiller{J: j}).Prepare(st.traces); err != nil {
			return nil, e.tendErr(err)
		}
	}
	stale := false
	if !dry {
		if _, err := os.Stat(filepath.Join(dataDir, "pathways.db")); err == nil {
			// Read only, as the journal: the store is migrated after
			// every refusal.
			s, err := pathways.OpenReadOnly(filepath.Join(dataDir, "pathways.db"))
			if err != nil {
				return nil, e.storeErr(cmd, err)
			}
			_, rerr := s.ReadAll(func(string) bool { return false })
			rows, aerr := s.All()
			s.Close()
			if errors.Is(rerr, pathways.ErrUnreadable) {
				return nil, e.refuse(cmd, rerr)
			}
			if aerr != nil {
				return nil, e.storeErr(cmd, aerr)
			}
			for _, r := range rows {
				if r["embedding_model"] != st.identity || r["embedding_model_version"] != pathways.EmbeddingModelVersion {
					stale = true
				}
			}
		}
	}
	if notCarried != nil && (stale || int64(len(st.traces)+extra) >= minTraces) {
		return nil, e.refuse(cmd, notCarried)
	}
	return st, nil
}

// runTend is Daisugi(...).tend(min_traces, lookback_days): the checks
// first, with nothing written; then the envelope cache, the journal and
// the store opened (and made), and the distiller run.
func (e *Env) runTend(dataDir, model string, minTraces, lookback int64, dry bool) (tendReport, error) {
	const cmd = "tend"
	started := nowSeconds()
	st, err := e.tendCheck(dataDir, model, minTraces, lookback, dry, 0, started)
	var raised *raisedError
	if errors.As(err, &raised) {
		e.errf("%s\n", raised.msg)
		e.raised = true
		return tendReport{}, exit(1)
	}
	if err != nil {
		return tendReport{}, err
	}
	// From here on the run writes, so input this binary cannot read stops
	// it without the refusal's "Nothing was changed.".
	if err := tracejournal.EnsureEnvelopeCache(filepath.Join(dataDir, "envelope_cache.db")); err != nil {
		return tendReport{}, e.afterWrites(cmd, err)
	}
	j, err := tracejournal.Open(dataDir)
	if err != nil {
		return tendReport{}, e.afterWrites(cmd, err)
	}
	defer j.Close()
	d := &distill.Distiller{J: j, M: st.m, LLM: st.client,
		Opt: distill.Options{Model: model, MinTraces: minTraces, LookbackDays: lookback, ValidationSplit: 0.6,
			StructureWeight: 0.5, Now: nowSeconds, Z3TimeoutMs: 500, Identity: st.identity}}
	if st.m != nil {
		d.Opt.Threshold = st.m.Threshold
	}
	if err := d.Prepare(st.traces); err != nil {
		return tendReport{}, e.afterWrites(cmd, err)
	}
	if dry {
		d.S = &distill.MemStore{}
	} else {
		s, err := pathways.Open(filepath.Join(dataDir, "pathways.db"))
		if err != nil {
			return tendReport{}, e.afterWrites(cmd, err)
		}
		defer s.Close()
		d.S = distill.DiskStore{Store: s}
	}
	rep, err := d.Tend(st.traces)
	if err != nil {
		return tendReport{}, e.afterWrites(cmd, err)
	}
	return tendReport{Report: rep, seconds: nowSeconds() - started}, nil
}

func (e *Env) tendErr(err error) error {
	if errors.Is(err, tracejournal.ErrUnreadable) || errors.Is(err, pathways.ErrUnreadable) {
		return e.refuse("tend", err)
	}
	return e.failPy("tend", err)
}

// afterWrites is tendErr, except that once a run has started writing,
// input this binary cannot read stops it (exit 2) with no claim that
// nothing changed. The checks before the first write read the same
// input, so no case reaches this.
func (e *Env) afterWrites(cmd string, err error) error {
	if errors.Is(err, tracejournal.ErrUnreadable) || errors.Is(err, pathways.ErrUnreadable) {
		e.errf("daisugi %s: %v. The run stopped after its first write.\n", cmd, err)
		return exit(2)
	}
	return e.failPy(cmd, err)
}

func (e *Env) distillRepeats(args []string) error {
	const cmd = "distill-repeats"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory (reads <data-dir>/gateway/turns.jsonl)."},
		{names: []string{"--top"}, value: true, metavar: "INTEGER", help: "Maximum number of rows to print."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Rank repeated gateway asks into a reuse worklist.", opts)
	}
	top, err := clickInt(p, "--top", 20)
	if err != nil {
		return e.usage(cmd, err)
	}
	dataDir := p.str("--data-dir", filepath.Join(e.home, ".opendaisugi"))
	path := filepath.Join(dataDir, "gateway", "turns.jsonl")
	// Skipped lines are logged on the opendaisugi logger, which prints
	// nothing by default.
	turns, _, err := distill.LoadTurns(path)
	if err != nil {
		return e.tendErrAs(cmd, err)
	}
	if len(turns) == 0 {
		e.out("no turns recorded yet — run `daisugi gateway` to start journaling.\n")
		return nil
	}
	signed := false
	for _, t := range turns {
		if t.Signature != "" {
			signed = true
		}
	}
	var cands []distill.Candidate
	if signed {
		m, notBuilt, err := e.matcher()
		if err != nil {
			return e.refuse(cmd, err)
		}
		if notBuilt != "" {
			e.errf("matcher_model=%s is not a built embedder.\n", pystr.Repr(notBuilt))
			return exit(1)
		}
		emb, err := m.Embedder()
		if err != nil {
			e.errf("%v\n", err)
			return exit(2)
		}
		var find func(string) bool
		db := filepath.Join(dataDir, "pathways.db")
		if _, err := os.Stat(db); err == nil {
			s, err := pathways.Open(db)
			if err != nil {
				return e.storeErr(cmd, err)
			}
			defer s.Close()
			key := m.Key
			find = func(task string) bool {
				r, err := s.Find(task, key, e.potionEnv(), -1)
				return err == nil && r.Match != nil
			}
		}
		cands = distill.RankReuse(turns, emb, m.Threshold, find)
	}
	if len(cands) == 0 {
		e.out("no repeated asks found yet.\n")
		return nil
	}
	e.out("%4s  %4s  %10s  %9s  reusable?  task\n", "rank", "occ", "tokens", "dollars")
	// candidates[:top], a negative top counting from the end.
	n := int64(len(cands))
	if top < 0 {
		n = max(0, n+top)
	} else if top < n {
		n = top
	}
	for i, c := range cands[:n] {
		e.out("%s\n", c.Row(i+1))
	}
	return nil
}

func (e *Env) tendErrAs(cmd string, err error) error {
	if errors.Is(err, tracejournal.ErrUnreadable) || errors.Is(err, pathways.ErrUnreadable) {
		return e.refuse(cmd, err)
	}
	return e.failPy(cmd, err)
}
