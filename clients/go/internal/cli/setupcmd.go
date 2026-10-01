package cli

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

var tiersHelp = `Usage: daisugi tiers [OPTIONS] COMMAND [ARGS]...

  Tier-0/1/2 routing stats derived from the journal.

Options:
  --help  Show this message and exit.

Commands:
  setup  Detect hardware, recommend a local model, and optionally qualify and wire it.
  stats  Not yet in this binary.
`

func (e *Env) tiers(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", tiersHelp)
		return nil
	}
	switch args[0] {
	case "setup":
		return e.tiersSetup(args[1:])
	case "stats":
		return e.notYet("daisugi tiers stats")
	}
	e.errf("Usage: daisugi tiers [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi tiers --help' for help.\n\n"+
		"Error: No such command '%s'.\n", args[0])
	return exit(2)
}

// setupMoved is `daisugi setup`: a stub that names its replacement.
func (e *Env) setupMoved(args []string) error {
	if len(args) > 0 && args[0] == "--help" {
		e.out("Usage: daisugi setup [OPTIONS]\n\n  Moved: `daisugi setup` is now `daisugi tiers setup` (same flags).\n\n" +
			"Options:\n  --help  Show this message and exit.\n")
		return nil
	}
	if len(args) > 0 {
		if strings.HasPrefix(args[0], "-") {
			return e.usage("setup", &usageError{"No such option: " + args[0]})
		}
		return e.usage("setup", &usageError{"Got unexpected extra argument (" + args[0] + ")"})
	}
	return e.fail3("`daisugi setup` moved.",
		"hardware detection and local-model qualification now live under `tiers`.",
		"run: daisugi tiers setup", 2)
}

// hardware is hardware.HardwareProfile on Linux.
type hardware struct {
	system, arch string
	cpus         int
	ram          *float64
	vram         float64
	gpu          any
}

func (h hardware) discrete() bool { return h.vram > 0 }

func (h hardware) budget() float64 {
	if h.discrete() {
		return pyRound1(h.vram * 0.8)
	}
	if h.ram != nil && *h.ram != 0 {
		return pyRound1(*h.ram * 0.6)
	}
	return 0
}

func utsString(b [65]int8) string {
	var s []byte
	for _, c := range b {
		if c == 0 {
			break
		}
		s = append(s, byte(c))
	}
	return string(s)
}

// cpuCount is os.cpu_count(): the CPUs online.
func cpuCount() int {
	raw, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return 1
	}
	n := 0
	for _, part := range strings.Split(strings.TrimSpace(string(raw)), ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(lo)
		if err1 != nil {
			return 1
		}
		b := a
		if isRange {
			if b, err1 = strconv.Atoi(hi); err1 != nil {
				return 1
			}
		}
		n += b - a + 1
	}
	if n < 1 {
		return 1
	}
	return n
}

func (e *Env) detectHardware() hardware {
	var u syscall.Utsname
	h := hardware{system: "Linux", cpus: cpuCount(), ram: ramGB()}
	if syscall.Uname(&u) == nil {
		h.system, h.arch = utsString(u.Sysname), utsString(u.Machine)
	}
	h.vram, h.gpu = e.gpuProbe()
	if math.IsNaN(h.vram) {
		h.vram = 0
	}
	return h
}

// recommendation is hardware.recommend_model.
type recommendation struct {
	size      string
	params    int
	download  float64
	families  []string
	rationale string
}

// pyG is format(f, "g").
func pyG(f float64) string {
	s := strconv.FormatFloat(f, 'g', 6, 64)
	if strings.Contains(s, "e") {
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		digits := strings.TrimLeft(exp[1:], "0")
		for len(digits) < 2 {
			digits = "0" + digits
		}
		return mant + "e" + sign + digits
	}
	return s
}

func recommend(h hardware) recommendation {
	b := h.budget()
	tiers := []struct {
		below    float64
		label    string
		params   int
		download float64
	}{{3, "≤1B", 1, 0.8}, {6, "~3B", 3, 2.2}, {12, "~8B", 8, 5.0}, {24, "~14B", 14, 9.0}, {math.Inf(1), "~32B", 32, 20.0}}
	var r recommendation
	for _, t := range tiers {
		if b < t.below {
			r.size, r.params, r.download = t.label, t.params, t.download
			break
		}
	}
	var where string
	switch {
	case h.discrete():
		where = pyG(h.vram) + "GB VRAM (" + pyStrOf(h.gpu) + ")"
	case h.ram != nil && *h.ram != 0:
		where = pyG(*h.ram) + "GB RAM (CPU inference — expect slower generation; favor the smaller end and a low context size)"
	default:
		where = "undetected memory (treating conservatively)"
	}
	r.rationale = "budget ~" + pyFixed(b, 0) + "GB from " + where + ". Recommending a " + r.size +
		"-class instruct model at Q4_K_M. This is provisional — qualify it on YOUR box (run the candidate " +
		"against the real envelope schema and check the pass rate) before trusting it as Tier-1; the model " +
		"family is your pick, not a verified default."
	if r.params >= 3 {
		r.families = []string{"Granite", "Ministral", "Gemma", "Llama"}
	} else {
		r.families = []string{"Granite", "Llama"}
	}
	return r
}

// probeTasks is local_setup.DEFAULT_PROBE_TASKS.
var probeTasks = []string{
	"Delete .tmp files older than 7 days in /var/log",
	"Read /data/sales.csv and print the row count",
	"List the running processes and save them to processes.txt",
}

// pyPercent0 is format(f, ".0%").
func pyPercent0(f float64) string { return pyFixed(f*100, 0) + "%" }

func floatAny(f float64) any { return pyjson.Float(f) }

func (e *Env) tiersSetup(args []string) error {
	const cmd = "tiers setup"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--endpoint"}, value: true, metavar: "TEXT", help: "OpenAI-compatible local /v1 URL to qualify (e.g. http://localhost:8080/v1)."},
		{names: []string{"--remote"}, value: true, metavar: "TEXT", help: "host[:port] of a self-hosted model server to probe and record."},
		{names: []string{"--kind"}, value: true, metavar: "TEXT", help: "auto | ollama | openai | anthropic. The wire --remote speaks."},
		{names: []string{"--context"}, value: true, metavar: "INTEGER", help: "Override the probed context window in tokens."},
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "Model name served by --endpoint or --remote."},
		{names: []string{"--threshold"}, value: true, metavar: "FLOAT", help: "Min valid-envelope pass rate to promote."},
		{names: []string{"--repeats"}, value: true, metavar: "INTEGER", help: "Sample each probe task N times."},
		{names: []string{"--wire"}, help: "Persist the model as Tier-1 if it qualifies."},
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
		{names: []string{"--matcher"}, value: true, metavar: "TEXT", help: "Set the pathway matcher and stop: lexical (the default, no model), " +
			"potion (a one-time download of about 30 MB), all-MiniLM-L6-v2 (needs torch) or int8."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Detect hardware, recommend a local model, and optionally qualify and wire it.", opts)
	}
	threshold, err := clickFloat(p, "--threshold", 0.8)
	if err != nil {
		return e.usage(cmd, err)
	}
	repeats, err := clickInt(p, "--repeats", 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.has("--context") {
		if _, err := clickInt(p, "--context", 0); err != nil {
			return e.usage(cmd, err)
		}
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
	if p.has("--matcher") {
		return e.setMatcher(cmd, gateroot.Join(dataDir, "config.yaml"), p.str("--matcher", ""))
	}
	endpoint, remote := p.str("--endpoint", ""), p.str("--remote", "")
	if endpoint != "" && remote != "" {
		return e.fail3("--endpoint and --remote are mutually exclusive.",
			"--endpoint qualifies a local /v1 server against the envelope-generation gate. --remote probes "+
				"and records a model host for your harness to point at.",
			"run one at a time: --endpoint URL --model NAME, or --remote HOST[:PORT].", 1)
	}
	if remote != "" {
		return e.notYet("daisugi tiers setup --remote")
	}
	model := p.str("--model", "")
	if endpoint != "" && model == "" {
		e.errf("--model is required with --endpoint (the model name the local server serves).\n")
		return exit(2)
	}
	h := e.detectHardware()
	rec := recommend(h)
	type qualification struct {
		attempts, valid int
		rate            float64
		passed          bool
	}
	var qual *qualification
	wired := false
	if endpoint != "" {
		c := e.llmClient()
		base := endpoint
		t := envgen.NewTier1(model, &base, nil, "")
		if err := c.Check(t.Model); err != nil {
			return e.refuse(cmd, err)
		}
		q := &qualification{}
		n := repeats
		if n < 1 {
			n = 1
		}
		for i := int64(0); i < n; i++ {
			for _, task := range probeTasks {
				if t.Generate(c, task, nil) != nil {
					q.valid++
				}
			}
		}
		q.attempts = len(probeTasks) * int(n)
		q.rate = pyjson.Round(float64(q.valid)/float64(q.attempts), 2)
		q.passed = q.attempts > 0 && q.rate >= threshold
		qual = q
		if q.passed && p.flag("--wire") {
			if err := os.MkdirAll(dataDir, 0o777); err != nil {
				return e.failPy(cmd, err)
			}
			body := pyjson.DumpsIndent(pyjson.NewObject().Set("model", model).Set("base_url", endpoint), 2, true) + "\n"
			if err := os.WriteFile(filepath.Join(dataDir, envgen.ConfigFile), []byte(body), 0o666); err != nil {
				return e.failPy(cmd, err)
			}
			wired = true
		}
	}
	budget := h.budget()
	if p.flag("--json") {
		var ram any
		if h.ram != nil {
			ram = floatAny(*h.ram)
		}
		fams := make([]any, len(rec.families))
		for i, f := range rec.families {
			fams[i] = f
		}
		var q any
		if qual != nil {
			q = pyjson.NewObject().Set("attempts", qual.attempts).Set("valid", qual.valid).
				Set("pass_rate", floatAny(qual.rate)).Set("passed", qual.passed).Set("threshold", floatAny(threshold)).
				Set("wired", wired)
		}
		payload := pyjson.NewObject().
			Set("hardware", pyjson.NewObject().Set("system", h.system).Set("arch", h.arch).Set("cpu_count", h.cpus).
				Set("ram_gb", ram).Set("vram_gb", floatAny(h.vram)).Set("gpu_name", h.gpu).
				Set("unified_memory", false).Set("model_budget_gb", floatAny(budget))).
			Set("recommendation", pyjson.NewObject().Set("size_class", rec.size).Set("params_b_max", rec.params).
				Set("quant", "Q4_K_M").Set("runtime", "llamafile").Set("est_download_gb", floatAny(rec.download)).
				Set("candidate_families", fams).Set("provisional", true).Set("rationale", rec.rationale)).
			Set("qualification", q)
		e.out("%s\n", pyjson.DumpsIndent(payload, 2, true))
		return nil
	}
	line := "Hardware: " + h.system + "/" + h.arch + ", " + strconv.Itoa(h.cpus) + " CPU"
	if h.ram != nil && *h.ram != 0 {
		line += ", " + pyjson.FloatRepr(*h.ram) + "GB RAM"
	} else {
		line += ", RAM undetected"
	}
	if h.discrete() {
		line += ", " + pyjson.FloatRepr(h.vram) + "GB VRAM (" + pyStrOf(h.gpu) + ")"
	} else {
		line += ", no discrete GPU"
	}
	e.out("%s\n", line)
	e.out("Model budget: ~%sGB\n\n", pyjson.FloatRepr(budget))
	e.out("Recommended: a %s-class instruct model at Q4_K_M via llamafile (~%sGB).\n", rec.size,
		pyjson.FloatRepr(rec.download))
	e.out("  candidate families (your pick, none verified-best): %s\n", strings.Join(rec.families, ", "))
	e.out("  %s\n\n", rec.rationale)
	if qual == nil {
		e.out("Get a local server running, then qualify + wire it:\n")
		e.out("  1. Pick a model:  daisugi models list   (or any: daisugi models search QUERY)\n")
		e.out("     Fetch its GGUF pinned to a commit:  daisugi models pin <gguf-repo> --pull\n")
		e.out("  2. Serve it with llamafile (github.com/mozilla-ai/llamafile):\n")
		e.out("     llamafile --server -m <model>.gguf --port 8080 --nobrowser\n")
		e.out("  3. Qualify:   daisugi tiers setup --endpoint http://localhost:8080/v1 --model <name> --wire\n")
		e.out("\nPathway matcher: lexical by default (no model, no download). For better recall:\n")
		e.out("  daisugi tiers setup --matcher potion   (a one-time download of about 30 MB)\n")
		return nil
	}
	verdict := "FAILED"
	if qual.passed {
		verdict = "PASSED"
	}
	e.out("Qualification: %s — %d/%d valid envelopes (pass rate %s, threshold %s).\n", verdict, qual.valid,
		qual.attempts, pyPercent0(qual.rate), pyPercent0(threshold))
	switch {
	case wired:
		e.out("  → Wired as Tier-1 in %s; `daisugi onboard`/`tend` will now defer to it.\n", dataDir)
	case qual.passed:
		e.out("  → Passed. Re-run with --wire to persist it as Tier-1.\n")
	default:
		// The provider declines rather than raises, so no attempt is an
		// error and the all-errored hint is never reached (SET-3).
		e.out("  → Not promoted. Try a larger model, a higher quant, or lower --threshold deliberately.\n")
	}
	return nil
}

// matchers are cli._MATCHERS: the matcher_model values tiers setup sets.
var matchers = []string{"lexical", "potion", "all-MiniLM-L6-v2", "int8"}

// setMatcher is cli._set_matcher: `tiers setup --matcher` records the
// pathway matcher in config.yaml.
func (e *Env) setMatcher(cmd, path, matcher string) error {
	known := false
	for _, m := range matchers {
		known = known || m == matcher
	}
	if !known {
		return e.fail3(pystr.Repr(matcher)+" is not a pathway matcher.",
			"the built matchers are "+strings.Join(matchers, ", ")+".",
			"run: daisugi tiers setup --matcher potion", 2)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("%s is not one this binary rewrites: %w", path, err))
	}
	if err := config.Save(path, e.home, map[string]any{"matcher_model": matcher}); err != nil {
		return e.refuse(cmd, fmt.Errorf("%s is not one this binary rewrites: %w", path, err))
	}
	e.out("Pathway matcher set to %s (was %s) in %s.\n", matcher, cfg.MatcherModel, path)
	if matcher != cfg.MatcherModel {
		e.out("A new matcher embeds text differently. Run `daisugi tend` to re-embed pathways.\n")
	}
	return nil
}
