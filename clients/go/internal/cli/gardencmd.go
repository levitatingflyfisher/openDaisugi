package cli

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/garden"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

var gardenerHelp = `Usage: daisugi gardener [OPTIONS] COMMAND [ARGS]...

  Lifecycle management for compiled pathways (prune, merge, status).

Commands:
  prune   Evict stale / failure-dominated pathways.
  merge   Collapse near-duplicate pathways.
  run     Run the full gardener pipeline (prune + merge).
  watch   Cron-friendly one-shot gardener.
  status  Report current store size, pathway activation stats, failure ratios.
`

func (e *Env) gardener(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", gardenerHelp)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "prune":
		return e.gardenerPrune(rest)
	case "merge":
		return e.gardenerMerge(rest)
	case "run":
		return e.gardenerRun(rest)
	case "watch":
		return e.gardenerWatch(rest)
	case "status":
		return e.gardenerStatus(rest)
	}
	e.errf("Usage: daisugi gardener [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi gardener --help' for help.\n\nError: No such command '%s'.\n", sub)
	return exit(2)
}

// nowSeconds is time.time().
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// clickFloat reads a FLOAT option: Python's float() of the text.
func clickFloat(p *parsed, name string, def float64) (float64, error) {
	if !p.has(name) {
		return def, nil
	}
	raw := p.str(name, "")
	f, ok := pmodel.FloatFromString(raw)
	if !ok {
		return 0, &usageError{fmt.Sprintf("Invalid value for '%s': %s is not a valid float.", name, pystr.Repr(raw))}
	}
	return f, nil
}

// clickInt reads an INT option: Python's int() of the text.
func clickInt(p *parsed, name string, def int64) (int64, error) {
	if !p.has(name) {
		return def, nil
	}
	raw := p.str(name, "")
	n, ok := pyInt(raw)
	if !ok {
		return 0, &usageError{fmt.Sprintf("Invalid value for '%s': %s is not a valid int.", name, pystr.Repr(raw))}
	}
	return n, nil
}

func dryRunOpt() opt { return opt{names: []string{"--dry-run"}} }

func (e *Env) gardenStoreErr(cmd string, err error) error {
	var inv *pathways.Invalid
	if errors.As(err, &inv) {
		return e.fail(cmd, err)
	}
	return e.storeErr(cmd, err)
}

func pruneJSON(r garden.PruneReport, withDry bool, dry bool) *pyjson.Object {
	o := pyjson.NewObject().Set("removed_ids", strAny(r.RemovedIDs)).Set("kept_count", r.KeptCount)
	reasons := pyjson.NewObject()
	for _, id := range r.RemovedIDs {
		reasons.Set(id, r.Reasons[id])
	}
	o.Set("reasons", reasons)
	if withDry {
		o.Set("dry_run", dry)
	}
	return o
}

func pairsAny(pairs [][2]string) []any {
	out := make([]any, len(pairs))
	for i, p := range pairs {
		out[i] = []any{p[0], p[1]}
	}
	return out
}

func strAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (e *Env) gardenerPrune(args []string) error {
	const cmd = "gardener prune"
	opts := []opt{dataDirOpt,
		{names: []string{"--max-idle-days"}, value: true, metavar: "FLOAT"},
		{names: []string{"--max-failure-ratio"}, value: true, metavar: "FLOAT"},
		{names: []string{"--min-activations"}, value: true, metavar: "INTEGER"},
		dryRunOpt(), {names: []string{"--json"}}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Evict stale / failure-dominated pathways.", opts)
	}
	cfg := garden.DefaultPrune()
	if cfg.MaxIdleDays, err = clickFloat(p, "--max-idle-days", 30); err != nil {
		return e.usage(cmd, err)
	}
	if cfg.MaxFailureRatio, err = clickFloat(p, "--max-failure-ratio", 0.5); err != nil {
		return e.usage(cmd, err)
	}
	n, err := clickInt(p, "--min-activations", 5)
	if err != nil {
		return e.usage(cmd, err)
	}
	cfg.MinActivations = big.NewInt(n)
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dry := p.flag("--dry-run")
	rep, err := garden.Prune(s, cfg, dry, nowSeconds())
	if err != nil {
		return e.gardenStoreErr(cmd, err)
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(pruneJSON(rep, true, dry), 2, true))
		return nil
	}
	verb := "removed"
	if dry {
		verb = "would remove"
	}
	e.out("%s: %d (kept: %d)\n", verb, len(rep.RemovedIDs), rep.KeptCount)
	for _, id := range rep.RemovedIDs {
		e.out("  %s — %s\n", id, rep.Reasons[id])
	}
	return nil
}

func (e *Env) gardenerMerge(args []string) error {
	const cmd = "gardener merge"
	opts := []opt{dataDirOpt, {names: []string{"--similarity"}, value: true, metavar: "FLOAT"},
		dryRunOpt(), {names: []string{"--json"}}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Collapse near-duplicate pathways.", opts)
	}
	cfg := garden.DefaultMerge()
	if cfg.SimilarityThreshold, err = clickFloat(p, "--similarity", 0.92); err != nil {
		return e.usage(cmd, err)
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dry := p.flag("--dry-run")
	rep, err := garden.Merge(s, cfg, dry)
	if err != nil {
		return e.gardenStoreErr(cmd, err)
	}
	if p.flag("--json") {
		o := pyjson.NewObject().Set("merged_pairs", pairsAny(rep.MergedPairs)).
			Set("kept_ids", strAny(rep.KeptIDs)).Set("removed_ids", strAny(rep.RemovedIDs)).Set("dry_run", dry)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	verb := "merged"
	if dry {
		verb = "would merge"
	}
	e.out("%s: %d pair(s)\n", verb, len(rep.MergedPairs))
	for _, pr := range rep.MergedPairs {
		e.out("  %s  <-  %s\n", pr[0], pr[1])
	}
	return nil
}

// runGardener is gardener.run_gardener: prune, then merge.
func runGardener(s *pathways.Store, dry bool) (garden.PruneReport, garden.MergeReport, error) {
	pr, err := garden.Prune(s, garden.DefaultPrune(), dry, nowSeconds())
	if err != nil {
		return pr, garden.MergeReport{}, err
	}
	mr, err := garden.Merge(s, garden.DefaultMerge(), dry)
	return pr, mr, err
}

func (e *Env) gardenerRun(args []string) error {
	const cmd = "gardener run"
	opts := []opt{dataDirOpt, dryRunOpt(), {names: []string{"--json"}}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Run the full gardener pipeline (prune + merge).", opts)
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dry := p.flag("--dry-run")
	pr, mr, err := runGardener(s, dry)
	if err != nil {
		return e.gardenStoreErr(cmd, err)
	}
	if p.flag("--json") {
		o := pyjson.NewObject().
			Set("prune", pruneJSON(pr, false, false)).
			Set("merge", pyjson.NewObject().Set("merged_pairs", pairsAny(mr.MergedPairs)).Set("kept_ids", strAny(mr.KeptIDs))).
			Set("dry_run", dry)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	verb := ""
	if dry {
		verb = "would"
	}
	e.out("prune: %s removed %d, kept %d\n", verb, len(pr.RemovedIDs), pr.KeptCount)
	e.out("merge: %s merged %d pair(s)\n", verb, len(mr.MergedPairs))
	return nil
}

// readStamp is the last-run stamp as the oracle reads it: a float, or 0
// when the file is absent or its text is not one.
func readStamp(path string) (float64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !utf8.Valid(raw) {
		return 0, errors.New("UnicodeDecodeError: the stamp file is not UTF-8")
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
	f, ok := pmodel.FloatFromString(pystr.Strip(text))
	if !ok {
		return 0, nil
	}
	return f, nil
}

func (e *Env) gardenerWatch(args []string) error {
	const cmd = "gardener watch"
	opts := []opt{dataDirOpt,
		{names: []string{"--min-interval"}, value: true, metavar: "INTEGER", help: "Skip if the last run is newer than this many seconds."},
		{names: []string{"--force"}, help: "Ignore the min-interval check."}, dryRunOpt()}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Cron-friendly one-shot gardener. Skips if last run is within --min-interval.", opts)
	}
	minInterval, err := clickInt(p, "--min-interval", 3600)
	if err != nil {
		return e.usage(cmd, err)
	}
	dir := p.str("--data-dir", filepath.Join(e.home, ".opendaisugi"))
	stamp := filepath.Join(dir, ".gardener-last-run")
	now := nowSeconds()
	last, err := readStamp(stamp)
	if err != nil {
		return e.failPy(cmd, err)
	}
	elapsed := now - last
	if !p.flag("--force") && elapsed < float64(minInterval) {
		o := pyjson.NewObject().Set("skipped", true).Set("reason", "min_interval_not_elapsed").
			Set("elapsed_s", pyjson.Round(elapsed, 1)).Set("min_interval_s", pyjson.Int{Text: fmt.Sprint(minInterval)})
		e.out("%s\n", pyjson.Dumps(o, true))
		return nil
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dry := p.flag("--dry-run")
	pr, mr, err := runGardener(s, dry)
	if err != nil {
		return e.gardenStoreErr(cmd, err)
	}
	if !dry {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return e.failPy(cmd, err)
		}
		if err := os.WriteFile(stamp, []byte(garden.FormatF(now, 3)), 0o666); err != nil {
			return e.failPy(cmd, err)
		}
	}
	o := pyjson.NewObject().Set("skipped", false).Set("ran_at", now).Set("dry_run", dry).
		Set("prune", pyjson.NewObject().Set("removed", len(pr.RemovedIDs)).Set("kept", pr.KeptCount)).
		Set("merge", pyjson.NewObject().Set("merged", len(mr.MergedPairs)))
	e.out("%s\n", pyjson.Dumps(o, true))
	return nil
}

func (e *Env) gardenerStatus(args []string) error {
	const cmd = "gardener status"
	opts := []opt{dataDirOpt, {names: []string{"--json"}}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Report current store size, pathway activation stats, failure ratios.", opts)
	}
	s, err := e.openStore(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	all, err := s.ReadAll(func(string) bool { return false })
	if err != nil {
		return e.gardenStoreErr(cmd, err)
	}
	if p.flag("--json") {
		items := []any{}
		for _, pw := range all {
			o := pw.Obj
			items = append(items, pyjson.NewObject().Set("id", o.Value("id")).
				Set("hit_count", o.Value("hit_count")).Set("failure_count", o.Value("failure_count")).
				Set("last_activation_at", o.Value("last_activation_at")))
		}
		o := pyjson.NewObject().Set("count", len(all)).Set("pathways", items)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	e.out("count: %d\n", len(all))
	for _, pw := range all {
		o := pw.Obj
		h, _ := new(big.Int).SetString(o.Value("hit_count").(pyjson.Int).Text, 10)
		f, _ := new(big.Int).SetString(o.Value("failure_count").(pyjson.Int).Text, 10)
		total := new(big.Int).Add(h, f)
		ratio := 0.0
		if total.Sign() != 0 {
			ratio, _ = new(big.Rat).SetFrac(f, total).Float64()
		}
		e.out("  %s  hits=%s  fails=%s  fail_ratio=%s\n", pw.ID(), h, f, garden.FormatF(ratio, 2))
	}
	return nil
}
