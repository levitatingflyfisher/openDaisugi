package cli

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

const journalHelp = `Usage: daisugi journal [OPTIONS] COMMAND [ARGS]...

  Inspect and query the trace journal.

Commands:
  search  Search journal traces by task (the lexical and potion matchers).
  replay  Re-run verify() on a stored trace and report drift.
  stats   Print aggregate stats from the journal index.
  parse   Parse an agent transcript into episodes.
  ingest  Ingest parsed episodes into the journal.
`

var journalDataDirOpt = opt{names: []string{"--data-dir"}, value: true, metavar: "PATH",
	help: "Root data directory containing journal/"}

func (e *Env) journal(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", journalHelp)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "stats":
		return e.journalStats(rest)
	case "replay":
		return e.journalReplay(rest)
	case "search":
		return e.journalSearch(rest)
	case "parse":
		return e.journalParse(rest)
	case "ingest":
		return e.journalIngest(rest)
	}
	e.errf("Usage: daisugi journal [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi journal --help' for help.\n\nError: No such command '%s'.\n", sub)
	return exit(2)
}

// openJournal is Journal(data_dir=...): the directories and the index made.
func (e *Env) openJournal(cmd, dataDir string) (*tracejournal.Journal, error) {
	j, err := tracejournal.Open(dataDir)
	if err != nil {
		return nil, e.failPy(cmd, err)
	}
	return j, nil
}

func (e *Env) journalStats(args []string) error {
	const cmd = "journal stats"
	opts := []opt{journalDataDirOpt, {names: []string{"--json"}, help: "Emit JournalStats as JSON."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Print aggregate stats from the journal index.", opts)
	}
	j, err := e.openJournal(cmd, e.dataDir(p))
	if err != nil {
		return err
	}
	defer j.Close()
	total, passed, failed, avg, err := j.Stats()
	if err != nil {
		return e.failPy(cmd, err)
	}
	a := 0.0
	if avg != nil {
		a = *avg
	}
	if p.flag("--json") {
		o := pyjson.NewObject().Set("total", pyjson.Int{Text: itoa64(total)}).
			Set("passed", pyjson.Int{Text: itoa64(passed)}).Set("failed", pyjson.Int{Text: itoa64(failed)}).
			Set("avg_duration_ms", a)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	e.out("total: %d\npassed: %d\nfailed: %d\navg duration (ms): %s\n", total, passed, failed, pyFixed(a, 2))
	return nil
}

// loadErr ends a command whose load_trace raised: exit 1 naming the
// exception, or the refusal for a body this binary does not read.
func (e *Env) loadErr(cmd string, err error) error {
	var le *tracejournal.LoadError
	if errors.As(err, &le) {
		e.errf("daisugi %s: %s: %s\n", cmd, le.Type, le.Msg)
		return exit(1)
	}
	if errors.Is(err, tracejournal.ErrUnreadable) {
		return e.refuse(cmd, err)
	}
	return e.failPy(cmd, err)
}

// replayed is Journal.replay's verify() of a stored trace, as a
// VerificationResult dump.
func replayVerify(rec *tracejournal.Record) (*verify.VerifyResultGo, *pyjson.Object, error) {
	p, err := verify.ParsePlan([]byte(pathways.DumpJSON(rec.Plan)))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the stored plan: %v", tracejournal.ErrUnreadable, err)
	}
	en, err := verify.ParseEnvelope([]byte(pathways.DumpJSON(rec.Envelope)))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the stored envelope: %v", tracejournal.ErrUnreadable, err)
	}
	t0 := time.Now()
	res := verify.Verify(p, en, verify.VerifyOptions{Z3TimeoutMs: 500})
	ms := float64(time.Since(t0).Nanoseconds()) / 1e6
	violations := []any{}
	for _, v := range res.Violations {
		var rem any
		if v.Remediation != nil {
			rem = *v.Remediation
		}
		violations = append(violations, pyjson.NewObject().Set("stage", v.Stage).Set("message", v.Message).
			Set("detail", v.Detail).Set("suggested_remediation", rem))
	}
	warnings := []any{}
	for _, w := range res.Warnings {
		warnings = append(warnings, w)
	}
	dump := pyjson.NewObject().Set("ok", res.OK).Set("violations", violations).Set("warnings", warnings).
		Set("envelope_id", rec.Envelope.Value("id")).Set("plan_id", rec.Plan.Value("id")).Set("duration_ms", ms).
		Set("client", "python").Set("fallback", nil).Set("client_verdict", nil)
	return &res, dump, nil
}

func (e *Env) journalReplay(args []string) error {
	const cmd = "journal replay"
	opts := []opt{journalDataDirOpt, {names: []string{"--json"}, help: "Emit ReplayResult as JSON."}}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " TRACE_ID", "Re-run verify() on a stored trace and report drift.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "TRACE_ID", "TRACE_ID")
	}
	id := p.args[0]
	j, err := e.openJournal(cmd, e.dataDir(p))
	if err != nil {
		return err
	}
	defer j.Close()
	rec, err := j.LoadTrace(id)
	var le *tracejournal.LoadError
	if errors.As(err, &le) && le.Type == "FileNotFoundError" {
		e.out("Trace not found: %s\n", id)
		return exit(2)
	}
	if err != nil {
		return e.loadErr(cmd, err)
	}
	res, dump, err := replayVerify(rec)
	if err != nil {
		return e.refuse(cmd, err)
	}
	origOK := rec.Result.Value("ok") == true
	drift := origOK != res.OK
	traceID, printable := pyStr(rec.ID)
	if p.flag("--json") {
		unworded := res.WarningsUnmodeled
		for _, v := range res.Violations {
			unworded = unworded || !v.Known
		}
		if unworded {
			// A violation's detail outside the permissions and DAG stages
			// is not worded by this binary's verifier (C-14).
			return e.refuse(cmd, errors.New("the replayed result holds a violation whose detail this binary does not write yet"))
		}
		o := pyjson.NewObject().Set("trace_id", tracejournal.JSONMode(rec.ID)).Set("original_ok", origOK).
			Set("replayed_ok", res.OK).Set("drift", drift).
			Set("original_result", tracejournal.JSONMode(rec.Result)).Set("replayed_result", dump)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
	} else {
		if !printable {
			return e.refuse(cmd, errors.New("the trace's id is not a string"))
		}
		if drift {
			e.out("%s: DRIFT detected\n", traceID)
			e.out("  original: ok=%s\n", pyBool(origOK))
			e.out("  replayed: ok=%s\n", pyBool(res.OK))
			if len(res.Violations) > 0 {
				e.out("  new violations:\n")
				for _, v := range res.Violations {
					e.out("    - [%s] %s\n", v.Stage, v.Message)
				}
			}
		} else {
			e.out("%s: no drift (ok=%s)\n", traceID, pyBool(origOK))
		}
	}
	if drift {
		return exit(1)
	}
	return nil
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func (e *Env) journalSearch(args []string) error {
	const cmd = "journal search"
	opts := []opt{journalDataDirOpt,
		{names: []string{"--limit"}, value: true, metavar: "INTEGER", help: "Maximum number of results."},
		{names: []string{"--json"}, help: "Emit result rows as JSON array."}}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " QUERY", "Search journal traces by task (the lexical and potion matchers).", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "QUERY", "QUERY")
	}
	limit, err := clickInt(p, "--limit", 10)
	if err != nil {
		return e.usage(cmd, err)
	}
	query := p.args[0]
	// The matcher is the one ~/.opendaisugi/config.yaml names. A matcher
	// this binary does not carry is refused before Journal() makes
	// anything; an error Python raises comes after it, as in Python.
	cfg, cerr := config.Load(gateroot.Join(e.dataHome(), "config.yaml"))
	if cerr != nil && !errors.Is(cerr, config.ErrInvalid) {
		return e.configLoadErr(cmd, cerr)
	}
	var m *pathways.Matcher
	var emb pathways.Embedder
	if cerr == nil {
		if m, err = pathways.SelectMatcher(cfg.MatcherModel, e.potionEnv()); err != nil {
			return e.refuse(cmd, err)
		}
		if m != nil {
			// A potion model that cannot be had is refused here, before
			// Journal() makes anything; Python raises after it.
			if emb, err = m.Embedder(); err != nil {
				if errors.Is(err, potion.ErrNotAvailable) {
					return e.refuse(cmd, err)
				}
				return e.failPy(cmd, err)
			}
		}
	}
	j, err := e.openJournal(cmd, e.dataDir(p))
	if err != nil {
		return err
	}
	defer j.Close()
	if cerr != nil {
		return e.configLoadErr(cmd, cerr)
	}
	if m == nil {
		e.errf("matcher_model=%s is not a built embedder. Built: 'all-MiniLM-L6-v2' with torch, 'potion' with no "+
			"torch, 'lexical' with no model, 'int8' with onnx and no torch.\n", pystr.Repr(cfg.MatcherModel))
		return exit(1)
	}
	rows, err := j.ListRecent(10000)
	if err != nil {
		return e.failPy(cmd, err)
	}
	traces := make([]*pyjson.Object, 0, len(rows))
	tasks := make([]string, 0, len(rows))
	for _, r := range rows {
		t, task, err := traceRow(r)
		if err != nil {
			return e.refuse(cmd, err)
		}
		traces = append(traces, t)
		tasks = append(tasks, task)
	}
	var results []*pyjson.Object
	if len(traces) > 0 {
		q := emb.Encode(query)
		qn := math.Sqrt(dot(q, q))
		scores := make([]float64, len(traces))
		for i, task := range tasks {
			c := emb.Encode(task)
			denom := math.Sqrt(pairwise(c)) * qn
			if denom == 0 {
				denom = 1e-9
			}
			scores[i] = math.Max(-1, math.Min(1, dot(c, q)/denom))
		}
		order := make([]int, len(scores))
		for i := range order {
			order[i] = i
		}
		// argsort(-scores): equal scores keep their order here; numpy's
		// SIMD sort may order them otherwise (C-15).
		sort.SliceStable(order, func(a, b int) bool { return -scores[order[a]] < -scores[order[b]] })
		for _, i := range pySlice(len(order), limit) {
			results = append(results, traces[order[i]])
		}
	}
	if p.flag("--json") {
		out := make([]any, len(results))
		for i, t := range results {
			out[i] = t
		}
		e.out("%s\n", pyjson.DumpsIndent(out, 2, true))
		return nil
	}
	if len(results) == 0 {
		e.out("(no matching traces)\n")
		return nil
	}
	for _, t := range results {
		status := "FAIL"
		if t.Value("ok") == true {
			status = "ok"
		}
		e.out("%s  [%s]  %s\n", t.Value("id"), status, t.Value("task"))
	}
	return nil
}

// pySlice is the indexes of seq[:limit] for a sequence of n items.
func pySlice(n int, limit int64) []int {
	end := int64(n)
	switch {
	case limit < 0:
		end = int64(n) + limit
		if end < 0 {
			end = 0
		}
	case limit < end:
		end = limit
	}
	out := make([]int, end)
	for i := range out {
		out[i] = i
	}
	return out
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// pairwise is numpy's pairwise sum of the squares of v, as
// np.linalg.norm(c, axis=1) sums a row.
func pairwise(v []float64) float64 {
	n := len(v)
	switch {
	case n < 8:
		s := 0.0
		for _, x := range v {
			s += x * x
		}
		return s
	case n <= 128:
		var r [8]float64
		i := 0
		for ; i < n-n%8; i += 8 {
			for k := 0; k < 8; k++ {
				r[k] += v[i+k] * v[i+k]
			}
		}
		s := ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]))
		for ; i < n; i++ {
			s += v[i] * v[i]
		}
		return s
	}
	n2 := n / 2
	n2 -= n2 % 8
	return pairwise(v[:n2]) + pairwise(v[n2:])
}

// traceRow is Trace(...) of one list_recent row, dumped in JSON mode,
// and its task.
func traceRow(r tracejournal.Recent) (*pyjson.Object, string, error) {
	strs := []any{r.ID, r.CreatedAt, r.Task, r.PlanID, r.EnvelopeID}
	for _, v := range strs {
		if _, ok := v.(string); !ok {
			return nil, "", errors.New("a journal row holds a value that is not text")
		}
	}
	var ok bool
	switch x := r.OK.(type) {
	case int64:
		ok = x != 0
	default:
		return nil, "", errors.New("a journal row's ok is not an integer")
	}
	var dur float64
	switch x := r.DurationMs.(type) {
	case float64:
		dur = x
	case int64:
		dur = float64(x)
	default:
		return nil, "", errors.New("a journal row's duration is not a number")
	}
	text, isStr := r.Violations.(string)
	if !isStr {
		return nil, "", errors.New("a journal row's violations are not text")
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		return nil, "", errors.New("a journal row's violations are not JSON this binary reads")
	}
	list, isList := v.([]any)
	if !isList {
		return nil, "", errors.New("a journal row's violations are not a list")
	}
	violations := make([]any, 0, len(list))
	for _, x := range list {
		o, isObj := x.(*pyjson.Object)
		if !isObj {
			return nil, "", errors.New("a journal row's violation is not an object")
		}
		out, verr := pmodel.Violation.ValidateObject(o, pmodel.Python)
		if verr != nil {
			return nil, "", errors.New("a journal row's violation does not validate")
		}
		violations = append(violations, tracejournal.JSONMode(out))
	}
	t := pyjson.NewObject().Set("id", r.ID).Set("created_at", r.CreatedAt).Set("task", r.Task).
		Set("plan_id", r.PlanID).Set("envelope_id", r.EnvelopeID).Set("ok", ok).Set("duration_ms", dur).
		Set("violations", violations)
	return t, r.Task.(string), nil
}
