package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/journal"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// matcherThresholds is active_threshold for each built matcher.
var matcherThresholds = map[string]float64{
	"all-MiniLM-L6-v2": 0.55, "potion": 0.59, "lexical": 0.25, "int8": 0.45,
}

// carried is whether this binary carries the matcher a config key names.
func carried(key string) bool { return key == "lexical" || key == "potion" }

// pyFixed is format(f, ".<n>f"): Python writes nan and inf in lower case.
func pyFixed(f float64, n int) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'f', n, 64)
}

// pyRound1 is round(f, 1).
func pyRound1(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 1, 64), 64)
	return r
}

// dataDir is a --data-dir option as pathlib prints it.
func (e *Env) dataDir(p *parsed) string {
	if p.has("--data-dir") {
		return gateroot.PathStr(p.str("--data-dir", ""))
	}
	return e.dataHome()
}

// status is onboarding.StatusReport.
type status struct {
	dataDir          string
	searchInstalled  bool
	pathwayCount     int64
	pathwayHits      pyjson.Int
	threshold        float64
	total, passed    int64
	failed           int64
	decomposeEnabled bool
	plansWouldDeny   int
	callsWouldDeny   int
}

// statusCmd is `daisugi status`: day-one readiness.
func (e *Env) statusCmd(args []string) error {
	const cmd = "status"
	opts := []opt{dataDirOpt,
		{names: []string{"--threshold"}, value: true, metavar: "FLOAT",
			help: "Pathway retrieval threshold to display; default: the active backend's."},
		jsonOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Show day-one readiness: are token savings live and are actions verified?", opts)
	}
	threshold, err := clickFloat(p, "--threshold", 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	rep := status{dataDir: e.dataDir(p), pathwayHits: pyjson.Int{Text: "0"}}
	// The matcher comes from ~/.opendaisugi/config.yaml, as the pathway
	// commands read it. The binary carries lexical and potion only; under
	// any other matcher it reuses no pathway, so it says so (C-12).
	home := gateroot.Join(e.dataHome(), "config.yaml")
	hcfg, herr := config.Load(home)
	key := "lexical"
	if herr == nil {
		key = hcfg.MatcherModel
	}
	rep.searchInstalled = carried(key)
	if p.has("--threshold") {
		rep.threshold = threshold
	} else {
		if herr != nil {
			return e.configLoadErr(cmd, herr)
		}
		t, ok := matcherThresholds[key]
		if !ok {
			e.errf("matcher_model=%s is not a built embedder.\n", pystr.Repr(key))
			return exit(1)
		}
		rep.threshold = t
	}
	e.statusPathways(&rep)
	e.statusJournal(&rep)
	cfg, err := config.Load(filepath.Join(rep.dataDir, "config.yaml"))
	if err != nil {
		return e.configLoadErr(cmd, err)
	}
	rep.decomposeEnabled = cfg.ShellAllowDecomposition
	rep.plansWouldDeny = tracejournal.WordWouldDeny(rep.dataDir, verify.DialectAuditPrefix)
	calls, err := journal.WordWouldDeny(gateroot.Join(rep.dataDir, "gate"))
	if err != nil {
		return e.refuse(cmd, err)
	}
	rep.callsWouldDeny = calls
	tokenReady := rep.searchInstalled && rep.pathwayCount > 0
	trustReady := rep.total > 0
	// The bash grammar is linked into this binary.
	decomposeReady := rep.decomposeEnabled
	if p.flag("--json") {
		o := pyjson.NewObject().Set("data_dir", rep.dataDir).Set("search_extra_installed", rep.searchInstalled).
			Set("pathway_count", pyjson.Int{Text: itoa64(rep.pathwayCount)}).Set("pathway_hits", rep.pathwayHits).
			Set("retrieval_threshold", rep.threshold).
			Set("journal_total", pyjson.Int{Text: itoa64(rep.total)}).
			Set("journal_passed", pyjson.Int{Text: itoa64(rep.passed)}).
			Set("journal_failed", pyjson.Int{Text: itoa64(rep.failed)}).
			Set("shell_decomposition_enabled", rep.decomposeEnabled).Set("shell_grammar_installed", true).
			Set("word_would_deny_plans", pyjson.Int{Text: strconv.Itoa(rep.plansWouldDeny)}).
			Set("word_would_deny_calls", pyjson.Int{Text: strconv.Itoa(rep.callsWouldDeny)}).
			Set("token_savings_ready", tokenReady).Set("trust_ready", trustReady).
			Set("shell_decomposition_ready", decomposeReady)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	const ok, no = "✓", "✗"
	e.out("opendaisugi status (data dir: %s)\n\n", rep.dataDir)
	e.out("Token savings (pathway routing):\n")
	if rep.searchInstalled {
		e.out("  %s [search] extra installed\n", ok)
	} else {
		e.out("  %s matcher %s is not in this binary — pathways disabled; set matcher_model: lexical or potion\n",
			no, key)
	}
	e.out("  • compiled pathways: %d (%s hits)\n", rep.pathwayCount, rep.pathwayHits.Text)
	e.out("  • retrieval threshold: %s\n", pyFixed(rep.threshold, 2))
	if tokenReady {
		e.out("  → %s token savings are LIVE\n", ok)
	} else {
		e.out("  → %s not yet — run `daisugi onboard`\n", no)
	}
	e.out("\nTrust (verified actions):\n")
	e.out("  • journal traces: %d (%d verified, %d rejected)\n", rep.total, rep.passed, rep.failed)
	e.out("  • verification: strict at stakes high/physical (rejects unprovable invariants)\n")
	e.out("  • dialect would-denies: %d plans in the journal, %d calls in the gate's audit log\n",
		rep.plansWouldDeny, rep.callsWouldDeny)
	if !rep.decomposeEnabled {
		e.out("  • compound shell (`a && b`): rejected outright (ADR-0010 opt-in is off)\n")
	} else {
		e.out("  • compound shell (`a && b`): decomposed, every head allowlist-checked\n")
	}
	if trustReady {
		e.out("  → %s journal populated; replay any action with `daisugi journal replay <id>`\n", ok)
	} else {
		e.out("  → %s empty — run `daisugi onboard` or start capturing\n", no)
	}
	e.out("\nLocal model (Tier-1 — cheap envelope generation):\n")
	wired, err := e.tier1(rep.dataDir)
	if err != nil {
		return err
	}
	if wired != "" {
		e.out("  %s wired: %s\n", ok, wired)
		e.out("  → onboard/tend defer bulk envelope generation to your local model\n")
		return nil
	}
	budget := e.modelBudget()
	e.out("  %s none wired — hardware budget ~%sGB → recommends a %s-class model\n", no, pyFixed(budget, 0),
		sizeClass(budget))
	e.out("  → run `daisugi tiers setup` to pick, qualify, and wire a local model\n")
	return nil
}

// statusPathways reads the store's count and hits. A store that cannot
// be read counts as empty: gather_status logs a warning through a logger
// with no handler, so nothing is printed.
func (e *Env) statusPathways(rep *status) {
	db := filepath.Join(rep.dataDir, "pathways.db")
	if _, err := os.Stat(db); err != nil {
		return
	}
	s, err := pathways.Open(db)
	if err != nil {
		return
	}
	defer s.Close()
	count, hits, err := s.Stats()
	if err != nil {
		return
	}
	rep.pathwayCount = count
	switch h := hits.(type) {
	case int64:
		rep.pathwayHits = pyjson.Int{Text: itoa64(h)}
	case float64:
		// int() of the float SUM.
		if math.IsNaN(h) || math.IsInf(h, 0) {
			// int() raises after the count is read: the count is kept and
			// the hits stay 0.
			return
		}
		rep.pathwayHits = pyjson.Int{Text: strconv.FormatFloat(math.Trunc(h), 'f', 0, 64)}
	}
}

// statusJournal reads the journal's counts read-only: a missing journal
// is not made and reads as empty. A journal that cannot be read counts
// as empty, silently, as for the store.
func (e *Env) statusJournal(rep *status) {
	j, err := tracejournal.OpenReadOnly(rep.dataDir)
	if err != nil {
		return
	}
	defer j.Close()
	t, p, f, _, err := j.Stats()
	if err != nil {
		return
	}
	rep.total, rep.passed, rep.failed = t, p, f
}

// osErrText is str() of the OSError Python raises for err:
// "[Errno N] <strerror>: '<path>'".
func osErrText(err error) string {
	var pe *os.PathError
	var no syscall.Errno
	if errors.As(err, &pe) && errors.As(pe.Err, &no) {
		msg := no.Error()
		if msg != "" {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		return fmt.Sprintf("[Errno %d] %s: %s", int(no), msg, pystr.Repr(pe.Path))
	}
	return err.Error()
}

// tier1 is load_configured_tier1(data_dir), worded as status prints it:
// "<model> @ <base_url>", or "" when none is wired.
func (e *Env) tier1(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "local_tier1.json")
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	// An unreadable or invalid file is ignored with a warning that a
	// logger with no handler drops.
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	if !utf8.Valid(raw) {
		return "", e.refuse("status", fmt.Errorf("%s is not UTF-8", path))
	}
	v, derr := pyjson.LoadsPy(string(raw), 900)
	if derr != nil {
		if derr.NotJSON || derr.TooDeep {
			return "", e.refuse("status", fmt.Errorf("%s holds JSON this binary does not read yet", path))
		}
		return "", nil
	}
	o, isObj := v.(*pyjson.Object)
	if !isObj {
		return "", e.refuse("status", fmt.Errorf("%s does not hold a JSON object", path))
	}
	model, _ := o.Get("model")
	if !truthy(model) {
		return "", nil
	}
	m, isStr := model.(string)
	if !isStr {
		return "", e.refuse("status", fmt.Errorf("the model in %s is not a string", path))
	}
	base, _ := o.Get("base_url")
	bs, ok := pyStr(base)
	if !ok {
		return "", e.refuse("status", fmt.Errorf("the base_url in %s is not a string", path))
	}
	if base != nil && !strings.Contains(m, "/") {
		m = "openai/" + m
	}
	return m + " @ " + bs, nil
}

// truthy is bool() of a JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case pyjson.Int:
		return strings.TrimLeft(strings.TrimPrefix(x.Text, "-"), "0") != ""
	case float64:
		return x != 0
	case pyjson.Float:
		return x != 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return len(x.Keys()) > 0
	}
	return true
}

// modelBudget is detect_hardware().model_budget_gb on Linux: 80% of the
// first GPU's memory as nvidia-smi reports it, else 60% of MemTotal.
// torch is not consulted (C-13).
func (e *Env) modelBudget() float64 {
	if vram := e.gpuMemGB(); vram > 0 {
		return pyRound1(vram * 0.8)
	}
	if ram := ramGB(); ram != nil && *ram != 0 {
		return pyRound1(*ram * 0.6)
	}
	return 0
}

// ramGB is _detect_ram_gb without psutil: MemTotal from /proc/meminfo,
// else the page count times the page size.
func ramGB() *float64 {
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) < 2 {
					break
				}
				kb, err := strconv.ParseInt(f[1], 10, 64)
				if err != nil {
					break
				}
				r := pyRound1(float64(kb*1024) / 1e9)
				return &r
			}
		}
	}
	return nil
}

// gpuMemGB is _detect_gpu's memory: 0 when there is no GPU it reads.
func (e *Env) gpuMemGB() float64 {
	vram, _ := e.gpuProbe()
	return vram
}

// gpuProbe is _detect_gpu's nvidia-smi probe: the first line's memory in
// GiB and the GPU's name, or 0 and nil when there is no nvidia-smi or it
// fails.
func (e *Env) gpuProbe() (float64, any) {
	smi, err := install.LookPath(e.env)("nvidia-smi")
	if err != nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, smi, "--query-gpu=memory.total,name", "--format=csv,noheader,nounits")
	c.Env = e.Environ
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &bytes.Buffer{}
	if c.Run() != nil || !utf8.Valid(out.Bytes()) {
		return 0, nil
	}
	text := pystr.Strip(out.String())
	if text == "" {
		return 0, nil
	}
	first := pystr.Splitlines(text)[0]
	mem, name, found := strings.Cut(first, ",")
	if !found {
		return 0, nil
	}
	f, ok := pmodel.FloatFromString(pystr.Strip(mem))
	if !ok {
		return 0, nil
	}
	return pyRound1(f / 1024), pystr.Strip(name)
}

// sizeClass is recommend_model's size label for a budget.
func sizeClass(budget float64) string {
	for _, t := range []struct {
		below float64
		label string
	}{{3, "≤1B"}, {6, "~3B"}, {12, "~8B"}, {24, "~14B"}, {math.Inf(1), "~32B"}} {
		if budget < t.below {
			return t.label
		}
	}
	return "~32B"
}
