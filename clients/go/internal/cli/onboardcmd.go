package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/sqlpath"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/transcript"
)

// splitPromptVersion is parsers.claude_code.SPLIT_PROMPT_VERSION.
const splitPromptVersion = "2026-08-03"

const splitCacheSchema = `
CREATE TABLE IF NOT EXISTS split_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    boundaries_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_split_prompt_version ON split_cache(prompt_version);
`

// splitCache is split_cache.SplitCache: rows read from the database when
// the run starts, and rows the run adds, written when the run writes.
type splitCache struct {
	path    string
	rows    map[string]string // cache key -> boundaries_json
	pending []splitRow
}

type splitRow struct {
	key, boundaries string
	at              float64
}

func splitKey(model, content string) string {
	return sha256Hex("version:" + splitPromptVersion + "\nmodel:" + model + "\ncontent:" + content)
}

// readSplitCache reads the rows of the current prompt version, without
// writing: a row of another version is evicted when the cache is made.
func readSplitCache(path string) (*splitCache, error) {
	c := &splitCache{path: path, rows: map[string]string{}}
	if _, err := os.Stat(path); err != nil {
		return c, nil
	}
	// sqlite3.connect takes the path as a plain file name.
	dsn, err := sqlpath.DSN(path, "mode=ro")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT cache_key, boundaries_json FROM split_cache WHERE prompt_version = ?", splitPromptVersion)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return c, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, b any
		if err := rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		ks, ok1 := k.(string)
		bs, ok2 := b.(string)
		if !ok1 || !ok2 {
			return nil, errors.New("a split cache row this binary does not read")
		}
		c.rows[ks] = bs
	}
	return c, rows.Err()
}

func (c *splitCache) get(model, content string) (any, bool, error) {
	text, ok := c.rows[splitKey(model, content)]
	if !ok {
		return nil, false, nil
	}
	v, derr := pyjson.LoadsPy(text, splitJSONDepth)
	if derr != nil {
		return nil, false, errors.New("a split cache row that does not read as JSON")
	}
	return v, true, nil
}

func (c *splitCache) put(v any, model, content string) {
	k := splitKey(model, content)
	text := pyjson.Dumps(v, true)
	c.rows[k] = text
	c.pending = append(c.pending, splitRow{k, text, nowSeconds()})
}

// write makes the cache (its schema, the stale rows evicted) and adds the
// run's rows.
func (c *splitCache) write() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o777); err != nil {
		return err
	}
	dsn, err := sqlpath.DSN(c.path, "")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(splitCacheSchema); err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM split_cache WHERE prompt_version != ?", splitPromptVersion); err != nil {
		return err
	}
	for _, r := range c.pending {
		if _, err := db.Exec("INSERT OR REPLACE INTO split_cache (cache_key, prompt_version, boundaries_json, "+
			"inserted_at) VALUES (?, ?, ?, ?)", r.key, splitPromptVersion, r.boundaries, r.at); err != nil {
			return err
		}
	}
	return nil
}

// discovered is onboarding.DiscoveredTranscript.
type discovered struct {
	path, harness string
	mtime         float64
}

type transcriptRoot struct{ harness, root string }

// expandUser is Path.expanduser for "~" and "~/...".
func (e *Env) expandUser(p string) string {
	if p == "~" {
		return e.home
	}
	if strings.HasPrefix(p, "~/") {
		return e.home + p[1:]
	}
	return p
}

// transcriptRoots is default_transcript_roots: the harness roots, in
// order, with OPENDAISUGI_TRANSCRIPT_ROOTS on top.
func (e *Env) transcriptRoots() ([]transcriptRoot, error) {
	roots := []transcriptRoot{
		{"claude-code", gateroot.Join(e.home, ".claude/projects")},
		{"codex", gateroot.Join(e.home, ".codex/sessions")},
	}
	set := func(h, r string) {
		for i := range roots {
			if roots[i].harness == h {
				roots[i].root = r
				return
			}
		}
		roots = append(roots, transcriptRoot{h, r})
	}
	env := pystr.Strip(e.env["OPENDAISUGI_TRANSCRIPT_ROOTS"])
	if env == "" {
		return roots, nil
	}
	for _, entry := range strings.Split(env, ":") {
		entry = pystr.Strip(entry)
		if entry == "" {
			continue
		}
		harness, path := "custom", entry
		if h, p, found := strings.Cut(entry, "="); found {
			harness, path = pystr.Strip(h), pystr.Strip(p)
			if harness == "" {
				harness = "custom"
			}
		}
		if strings.HasPrefix(path, "~") && path != "~" && !strings.HasPrefix(path, "~/") {
			return nil, errors.New("a transcript root under another user's home (~user)")
		}
		set(harness, gateroot.PathStr(e.expandUser(path)))
	}
	return roots, nil
}

// discoverTranscripts is discover_transcripts: every non-empty *.jsonl
// file under the roots (journal.jsonl aside), once each by its resolved
// path, newest first. Directories are walked without following links.
func discoverTranscripts(roots []transcriptRoot) ([]discovered, error) {
	seen := map[string]bool{}
	var found []discovered
	for _, r := range roots {
		fi, err := os.Stat(r.root)
		if err != nil || !fi.IsDir() {
			continue
		}
		var walkErr error
		_ = filepath.WalkDir(r.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == r.root {
					walkErr = err
				}
				return nil
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") || d.Name() == "journal.jsonl" {
				return nil
			}
			st, err := os.Stat(p)
			if err != nil || !st.Mode().IsRegular() {
				return nil
			}
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil
			}
			if abs, err := filepath.Abs(resolved); err == nil {
				resolved = abs
			}
			if seen[resolved] || st.Size() == 0 {
				return nil
			}
			seen[resolved] = true
			found = append(found, discovered{path: gateroot.PathStr(p), harness: r.harness,
				mtime: float64(st.ModTime().UnixNano()) / 1e9})
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].mtime > found[j].mtime })
	return found, nil
}

// onboardTranscript is one transcript's parse and ingest, worked out
// before anything is written.
type onboardTranscript struct {
	t        discovered
	episodes int
	items    []*ingestItem
	// warning is set when the transcript is skipped (no parser, or a
	// parse that raised).
	warning   string
	processed bool
}

func (e *Env) onboard(args []string) error {
	const cmd = "onboard"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "Model for episode splitting + distillation (envelopes are inferred, no LLM)."},
		{names: []string{"--llm"}, value: true, metavar: "TEXT", help: "LLM backend: api | claude-code. Default: auto-detect."},
		{names: []string{"--limit"}, value: true, metavar: "INTEGER", help: "Process only the N most recent transcripts."},
		{names: []string{"--harness"}, value: true, multiple: true, metavar: "TEXT", help: "Only process these harnesses (repeatable). Default: all discovered."},
		{names: []string{"--min-tools"}, value: true, metavar: "INTEGER", help: "Merge episodes below this tool-call count."},
		{names: []string{"--max-tools"}, value: true, metavar: "INTEGER", help: "LLM-split episodes above this tool-call count."},
		{names: []string{"--min-traces"}, value: true, metavar: "INTEGER", help: "Minimum cluster size to distill a pathway."},
		{names: []string{"--lookback-days"}, value: true, metavar: "INTEGER", help: "How far back to scan ingested traces when distilling (default: all history)."},
		{names: []string{"--threshold"}, value: true, metavar: "FLOAT", help: "Pathway clustering/retrieval similarity threshold (0-1)."},
		{names: []string{"--dry-run"}, help: "Preview: discover and deterministically verify each episode; makes NO model calls and writes no traces or pathways."},
		{names: []string{"--allow-no-embedder"}, help: "Onboard without the pathway embedder."},
		decomposeOpt,
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Turn existing conversations into token-saving pathways — the day-one flow.", opts)
	}
	var limit *int64
	if p.has("--limit") {
		n, err := clickInt(p, "--limit", 0)
		if err != nil {
			return e.usage(cmd, err)
		}
		limit = &n
	}
	minTools, err := clickInt(p, "--min-tools", 3)
	if err != nil {
		return e.usage(cmd, err)
	}
	maxTools, err := clickInt(p, "--max-tools", 30)
	if err != nil {
		return e.usage(cmd, err)
	}
	minTraces, err := clickInt(p, "--min-traces", 3)
	if err != nil {
		return e.usage(cmd, err)
	}
	lookback, err := clickInt(p, "--lookback-days", 3650)
	if err != nil {
		return e.usage(cmd, err)
	}
	var threshold *float64
	if p.has("--threshold") {
		f, err := clickFloat(p, "--threshold", 0)
		if err != nil {
			return e.usage(cmd, err)
		}
		threshold = &f
	}
	if p.has("--llm") {
		v := p.str("--llm", "")
		if err := e.checkLLMFlag(v); err != nil {
			return err
		}
		e.env["OPENDAISUGI_LLM_BACKEND"] = v
		e.Environ = append(e.Environ, "OPENDAISUGI_LLM_BACKEND="+v)
	}
	dry := p.flag("--dry-run")
	asJSON := p.flag("--json")
	model := p.str("--model", splitDefaultModel)
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	if err := e.renamedBackendAt(cmd, dataDir); err != nil {
		return err
	}
	if _, set := e.env["OPENDAISUGI_CONFORMANCE_RECORD"]; set && !dry {
		return e.notYet("daisugi onboard with OPENDAISUGI_CONFORMANCE_RECORD set")
	}
	// Everything below, up to the first write, only reads: a refusal
	// changes nothing. The oracle checks the matcher in effect: a key that
	// nothing builds turns pathways off. A matcher the oracle builds and
	// this binary does not is refused.
	_, notBuilt, err := e.matcher()
	if err != nil {
		return e.refuse(cmd, err)
	}
	// A dry run's note goes to stderr, after the resolved line, so a
	// --json report on stdout stays valid JSON.
	dryNote := ""
	if notBuilt != "" {
		fix := "  Set matcher_model: lexical or potion in config.yaml"
		if dry {
			dryNote = fmt.Sprintf("note: matcher_model %s is not a built embedder, so a real onboard "+
				"would distil NO token-saving pathways.\n%s\n", pystr.Repr(notBuilt), fix)
		} else if !p.flag("--allow-no-embedder") {
			if err := e.echoResolvedAt(dataDir); err != nil {
				return e.refuse(cmd, err)
			}
			e.errf("onboard needs the pathway embedder to turn traces into token-saving "+
				"pathways, but matcher_model %s is not a built embedder. Without it this run "+
				"would spend model tokens on envelope generation and distil ZERO pathways.\n%s\n"+
				"  Or build only the verified journal (no pathways):  --allow-no-embedder\n",
				pystr.Repr(notBuilt), fix)
			return exit(2)
		}
	}
	c := e.llmClient()
	viaAPI := c.Backend() != "claude-code"
	if !dry {
		if err := c.Check(model); err != nil {
			return e.refuse(cmd, err)
		}
	}
	decompose := p.flag("--allow-shell-decomposition")
	if !p.flagSet("--allow-shell-decomposition") {
		cfg, err := config.Load(gateroot.Join(dataDir, "config.yaml"))
		if err != nil {
			return e.configLoadErr(cmd, err)
		}
		decompose = cfg.ShellAllowDecomposition
	}
	if _, err := os.Stat(filepath.Join(dataDir, "local_tier1.json")); err == nil {
		// load_configured_tier1 reads it; the facade's Tier-1 slot is not
		// used by tend, so only a config the oracle raises on matters.
		if raw, err := os.ReadFile(filepath.Join(dataDir, "local_tier1.json")); err != nil || !utf8.Valid(raw) {
			return e.refuse(cmd, errors.New("a Tier-1 config this binary does not read"))
		}
	}
	var cache *splitCache
	if !dry {
		if cache, err = readSplitCache(filepath.Join(dataDir, "split_cache.db")); err != nil {
			return e.refuse(cmd, err)
		}
	}
	roots, err := e.transcriptRoots()
	if err != nil {
		return e.refuse(cmd, err)
	}
	found, err := discoverTranscripts(roots)
	if err != nil {
		return e.refuse(cmd, err)
	}
	if hs := p.vals["--harness"]; len(hs) > 0 {
		want := map[string]bool{}
		for _, h := range hs {
			want[h] = true
		}
		kept := found[:0:0]
		for _, t := range found {
			if want[t.harness] {
				kept = append(kept, t)
			}
		}
		found = kept
	}
	report := pyjson.NewObject()
	nFound := len(found)
	if limit != nil {
		idx := pySlice(len(found), *limit)
		found = found[:len(idx)]
	}
	effMax := maxTools
	if dry {
		effMax = 1_000_000_000
	}
	traces := &tracejournal.Journal{TracesDir: gateroot.Join(dataDir, "journal/traces")}
	var runs []*onboardTranscript
	neutral := ""
	defer func() {
		if neutral != "" {
			os.RemoveAll(neutral)
		}
	}()
	for _, t := range found {
		ot := &onboardTranscript{t: t}
		runs = append(runs, ot)
		if t.harness != "claude-code" && t.harness != "codex" {
			name := t.path[strings.LastIndexByte(t.path, '/')+1:]
			ot.warning = "no parser for harness " + pystr.Repr(t.harness) + ": " + name
			continue
		}
		eps, perr, err := e.parseForOnboard(t.path, t.harness, minTools, effMax, model, viaAPI, cache, &neutral)
		if err != nil {
			return e.refuse(cmd, fmt.Errorf("%s: %v", t.path, err))
		}
		if perr != "" {
			ot.warning = "parse failed for " + t.path + ": " + perr
			continue
		}
		ot.processed = true
		ot.episodes = len(eps)
		items, err := e.prepareIngest(cmd, t.path, eps, traces, decompose, dry, maxTools)
		if err != nil {
			return err
		}
		ot.items = items
	}
	// The writes, in the oracle's order: the journal, the split cache,
	// each transcript's traces, then tend.
	if err := e.echoResolvedAt(dataDir); err != nil {
		return e.refuse(cmd, err)
	}
	e.errf("%s", dryNote)
	j, err := tracejournal.Open(dataDir)
	if err != nil {
		return e.afterWrites(cmd, err)
	}
	defer j.Close()
	if cache != nil {
		if err := cache.write(); err != nil {
			return e.afterWrites(cmd, err)
		}
	}
	say := func(msg string) {
		if !asJSON {
			e.note("%s", msg)
		}
	}
	var warnings []any
	byHarness := pyjson.NewObject()
	var processed, episodes, passed, failed, skipped, preview, errored int
	firstError := ""
	if len(found) == 0 {
		warnings = append(warnings, "no transcripts discovered — check OPENDAISUGI_TRANSCRIPT_ROOTS or "+
			"your harness data directory")
		say("onboard: no transcripts discovered; nothing to distill")
	} else {
		say(fmt.Sprintf("onboard: processing %d transcript(s)", len(found)))
		for _, ot := range runs {
			if !ot.processed {
				warnings = append(warnings, ot.warning)
				continue
			}
			processed++
			n := 0
			if v, ok := byHarness.Get(ot.t.harness); ok {
				n = v.(int)
			}
			byHarness.Set(ot.t.harness, n+1)
			episodes += ot.episodes
			var p1 int
			for _, it := range ot.items {
				if !dry && (it.status == "OK" || it.status == "FAIL") {
					created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
					if err := j.Log(it.task, it.env, it.plan, it.result, it.traceID, created); err != nil {
						return e.afterWrites(cmd, err)
					}
				}
				switch it.status {
				case "OK":
					passed++
					p1++
				case "FAIL":
					failed++
				case "SKIP":
					skipped++
				case "TOO-LARGE":
					preview++
				case "ERROR":
					errored++
					if firstError == "" && it.hasErr {
						firstError = it.errText
					}
				}
			}
			name := ot.t.path[strings.LastIndexByte(ot.t.path, '/')+1:]
			say(fmt.Sprintf("onboard: %s -> %d episode(s) (%d passed)", name, ot.episodes, p1))
		}
		if processed > 0 && errored > 0 && passed == 0 && failed == 0 {
			hint := fmt.Sprintf("onboard: envelope generation failed for all %d processed episode(s) and no traces "+
				"were logged — is your model backend configured? Set OPENDAISUGI_LLM_BACKEND=claude-code for "+
				"no-API-key mode, or provide an API key for your model.", errored)
			if firstError != "" {
				hint += " First error: " + firstError
			}
			warnings = append(warnings, hint)
			say(hint)
		}
	}
	var created, updated int
	if len(found) > 0 {
		if dry {
			say("onboard: dry-run — skipping distillation (no pathways written)")
		} else {
			say("onboard: distilling pathways (tend)…")
			rep, err := e.runTend(dataDir, model, minTraces, lookback, false, threshold)
			if err != nil {
				return err
			}
			created, updated = rep.Created, rep.Updated
			for _, w := range rep.Warnings {
				warnings = append(warnings, w)
			}
			say(fmt.Sprintf("onboard: distilled %d new pathway(s), %d updated", created, updated))
		}
	}
	if warnings == nil {
		warnings = []any{}
	}
	if asJSON {
		report.Set("transcripts_found", nFound).Set("transcripts_processed", processed).Set("by_harness", byHarness).
			Set("episodes_total", episodes).Set("traces_passed", passed).Set("traces_failed", failed).
			Set("traces_skipped", skipped).Set("traces_preview_skipped", preview).Set("traces_errored", errored).
			Set("pathways_created", created).Set("pathways_updated", updated).Set("dry_run", dry).
			Set("warnings", warnings)
		e.out("%s\n", pyjson.DumpsIndent(report, 2, true))
		return nil
	}
	e.out("\n")
	keys := append([]string(nil), byHarness.Keys()...)
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %d", k, byHarness.Value(k).(int))
	}
	by := strings.Join(parts, ", ")
	if by == "" {
		by = "—"
	}
	e.out("Discovered %d transcript(s); processed %d (%s).\n", nFound, processed, by)
	if dry {
		previewed := passed + failed
		e.out("Preview — deterministic verify, no model calls, nothing written:\n")
		line := fmt.Sprintf("  %d episode(s) previewed: %d would verify, %d would fail", previewed, passed, failed)
		if skipped > 0 {
			line += fmt.Sprintf("; %d already present", skipped)
		}
		e.out("%s\n", line)
		if preview > 0 {
			e.out("  %d more episode(s) too large to preview here — a real onboard LLM-splits each into "+
				"sub-episodes (which mostly verify), so the counts above cover only the %d that don't need "+
				"splitting.\n", preview, previewed)
		}
		if !decompose && failed > 0 {
			e.out("  tip: many failures are compound shells (a && b) rejected without decomposition — add " +
				"--allow-shell-decomposition to preview them as they'd run.\n")
		}
		e.out("Dry run — re-run without --dry-run to journal + distill.\n")
	} else {
		e.out("Journal: %d verified, %d failed, %d already present.\n", passed, failed, skipped)
		e.out("Pathways: %d new, %d updated.\n", created, updated)
		if created > 0 || updated > 0 {
			e.out("  → Token routing is live: matching tasks now skip envelope generation (Tier-0).\n")
		}
		e.out("  → Trust: replay any action with `daisugi journal replay <id>`; journal at %s.\n",
			gateroot.Join(dataDir, "journal"))
	}
	for _, w := range warnings {
		e.out("  warning: %s\n", w)
	}
	return nil
}

// parseForOnboard is get_parser(format, ...).parse(path) with the split
// cache: the finalized episodes, or the text of the exception the parse
// raised (onboard warns and goes on), or an error for input this binary
// does not read the oracle's way.
func (e *Env) parseForOnboard(path, format string, minTools, maxTools int64, model string, viaAPI bool,
	cache *splitCache, neutral *string) ([]any, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	eps, err := transcript.Read(raw, format)
	var pe *transcript.ParseError
	if errors.As(err, &pe) {
		return nil, pe.Msg, nil
	}
	if err != nil {
		return nil, "", err
	}
	eps = transcript.MergeSmall(eps, minTools)
	eps, err = transcript.Split(eps, maxTools, func(content string) (any, error) {
		if cache != nil {
			v, hit, err := cache.get(model, content)
			if err != nil {
				return nil, err
			}
			if hit {
				return v, nil
			}
		}
		var v any
		var err error
		if viaAPI {
			v, err = e.splitAPI(model, content)
		} else {
			v, err = e.splitClaude(content, neutral)
		}
		if err != nil || v == nil {
			return v, err
		}
		if cache != nil {
			cache.put(v, model, content)
		}
		return v, nil
	})
	var out []any
	if err == nil {
		out, err = transcript.Finalize(eps)
	}
	if errors.As(err, &pe) {
		return nil, pe.Msg, nil
	}
	if err != nil {
		return nil, "", err
	}
	return out, "", nil
}

// prepareIngest is ingest_episodes up to the journal writes: each
// episode's status, with what a real run journals for it.
func (e *Env) prepareIngest(cmd, sourceFile string, episodes []any, traces *tracejournal.Journal, decompose, dry bool,
	previewMax int64) ([]*ingestItem, error) {
	sum := sha256Hex(sourceFile)[:8]
	var items []*ingestItem
	for _, x := range episodes {
		ep := x.(*pyjson.Object)
		it := &ingestItem{id: ep.Value("id").(string), task: ep.Value("task").(string)}
		raw := ep.Value("steps").([]any)
		// The parser's episodes hold step models: a plan made of them
		// dumps every field, defaults included.
		steps := make([]any, len(raw))
		for i, x := range raw {
			so, _ := x.(*pyjson.Object)
			typ, _ := so.Value("type").(string)
			m := pmodel.StepTypes[typ]
			if m == nil {
				return nil, e.refuse(cmd, fmt.Errorf("a step of type %s this binary does not read", pystr.Repr(typ)))
			}
			v, verr := pmodel.Validate(m.Name, m, so, pmodel.Python)
			if verr != nil {
				return nil, e.refuse(cmd, fmt.Errorf("a step that does not validate (%s)", short(verr)))
			}
			steps[i] = v
		}
		it.steps = len(steps)
		it.traceID = "import-" + sum + "-" + it.id
		items = append(items, it)
		_, lerr := traces.LoadTrace(it.traceID)
		var le *tracejournal.LoadError
		switch {
		case lerr == nil:
			it.status = "SKIP"
			continue
		case errors.As(lerr, &le) && le.Type == "FileNotFoundError":
		default:
			return nil, e.refuse(cmd, fmt.Errorf("a journal trace this binary does not read (%v)", lerr))
		}
		if dry && int64(it.steps) > previewMax {
			it.status = "TOO-LARGE"
			continue
		}
		if !traceIDOK(it.traceID) {
			return nil, e.refuse(cmd, fmt.Errorf("a trace id the journal rejects (%s)", it.traceID))
		}
		if err := e.ingestOne(cmd, it, steps, decompose, dry); err != nil {
			return nil, err
		}
	}
	return items, nil
}
