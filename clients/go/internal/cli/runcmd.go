package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/verify"
)

// clickPath is click.Path(exists=True, readable=True): the usage error
// click gives, or nil.
func clickPath(name, path string) error {
	if _, err := os.Stat(path); err != nil {
		return &usageError{fmt.Sprintf("Invalid value for %s: Path '%s' does not exist.", name, path)}
	}
	if syscall.Access(path, 4) != nil {
		return &usageError{fmt.Sprintf("Invalid value for %s: Path '%s' is not readable.", name, path)}
	}
	return nil
}

// usageArgs prints click's usage error for a command with a positional.
func (e *Env) usageArgs(cmd, args string, err error) error {
	if u, ok := err.(*usageError); ok {
		e.errf("Usage: daisugi %s [OPTIONS] %s\nTry 'daisugi %s --help' for help.\n\nError: %s\n", cmd, args, cmd, u.msg)
		return exit(2)
	}
	return err
}

// loadModelYAML is Model(**yaml.safe_load(path.read_text())): the model's
// dump, or the first line of the error the oracle prints ("" and a refusal
// for what this binary does not read the way Python does).
func loadModelYAML(path, title string, m *pmodel.Model) (dump *pyjson.Object, parseErr, refusal string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Sprintf("%s cannot be read: %v", path, err)
	}
	if !utf8.Valid(raw) {
		return nil, "", path + " is not UTF-8"
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
	v, exc, why := pyyaml.Load(text)
	if why != nil {
		return nil, "", fmt.Sprintf("%s holds YAML this binary does not read (%s)", path, why.Why)
	}
	if exc != nil {
		if pyyaml.Caught(exc) {
			return nil, pyyaml.FirstLine(exc), ""
		}
		return nil, "", fmt.Sprintf("%s holds YAML the oracle rejects with a %s, which it does not catch", path, exc.Type)
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, "", path + " does not hold a mapping"
	}
	if !pyyaml.Plain(o) {
		return nil, "", path + " holds a date or a key that is not text"
	}
	out, verr := pmodel.Validate(title, m, o, pmodel.Python)
	if verr != nil {
		if why := verr.Unreadable(); why != "" {
			return nil, "", path + ": " + why
		}
		return nil, strings.SplitN(verr.String(), "\n", 2)[0], ""
	}
	return out.(*pyjson.Object), "", ""
}

// verifyPlan is the whole-plan verify a run makes first; a test swaps it
// to make a Z3 check answer unknown.
var verifyPlan = verify.Verify

// prepared is a plan and envelope read and verified before anything is
// written.
type prepared struct {
	plan, env    *pyjson.Object
	venv         verify.Envelope
	verification *pyjson.Object
}

// prepare verifies plan against env as Supervisor.run does first, and
// refuses what this binary cannot journal the way the oracle does.
func prepare(plan, env *pyjson.Object) (*prepared, string) {
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		return nil, "the envelope does not read: " + err.Error()
	}
	if why := verify.Stage2Refusal(venv); why != "" {
		return nil, why
	}
	vplan, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
	if err != nil {
		return nil, "the plan does not read: " + err.Error()
	}
	t0 := time.Now()
	r := verifyPlan(vplan, venv, verify.VerifyOptions{Z3TimeoutMs: 500})
	d := float64(time.Since(t0).Nanoseconds()) / 1e6
	if len(r.Timeouts) > 0 {
		// A Z3 check that did not finish fails the run here, where the
		// oracle's lenient verify keeps it as a warning (K2-2).
		r.OK = false
		r.Violations = append(r.Violations, verify.V("z3", r.Timeouts[0]).With(pyjson.NewObject(), nil))
	}
	dump, why := supervise.VerificationDump(r, env.Value("id"), plan.Value("id"), d)
	if why != "" {
		return nil, why
	}
	return &prepared{plan: plan, env: env, venv: venv, verification: dump}, ""
}

// runCmd is `daisugi run PLAN -e ENVELOPE`: the plan executed under the
// supervisor.
func (e *Env) runCmd(args []string) error {
	const cmd = "run"
	opts := []opt{
		{names: []string{"--envelope", "-e"}, value: true, metavar: "PATH", help: "Path to envelope YAML."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Root data directory for the journal."},
		{names: []string{"--dry-run"}, help: "Use DryRunExecutor — no real subprocesses."},
		{names: []string{"--yes", "-y"}, help: "Auto-approve every step (sets DAISUGI_APPROVE=always for this run)."},
		{names: []string{"--json"}, help: "Emit the run session as JSON on stdout."},
		agentOpt,
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "PLAN_PATH", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PLAN_PATH", "Execute PLAN against ENVELOPE under runtime supervision.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "PLAN_PATH", &usageError{"Missing argument 'PLAN_PATH'."})
	}
	planPath := p.args[0]
	if err := clickPath("'plan_path'", planPath); err != nil {
		return e.usageArgs(cmd, "PLAN_PATH", err)
	}
	if !p.has("--envelope") {
		return e.usageArgs(cmd, "PLAN_PATH", &usageError{"Missing option '--envelope' / '-e'."})
	}
	envPath := p.str("--envelope", "")
	if err := clickPath("'--envelope' / '-e'", envPath); err != nil {
		return e.usageArgs(cmd, "PLAN_PATH", err)
	}
	agent, err := e.checkAgent(p)
	if err != nil {
		return err
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	env, perr, refusal := loadModelYAML(envPath, "Envelope", pmodel.Envelope)
	if refusal != "" {
		return e.refuse(cmd, fmt.Errorf("%s", refusal))
	}
	if perr != "" {
		return e.fail3("Tried to read the envelope "+envPath+".", "It did not parse: "+perr, "Fix the file and run again.", 2)
	}
	plan, perr, refusal := loadModelYAML(planPath, "ActionPlan", pmodel.ActionPlan)
	if refusal != "" {
		return e.refuse(cmd, fmt.Errorf("%s", refusal))
	}
	if perr != "" {
		return e.fail3("Tried to read the plan "+planPath+".", "It did not parse: "+perr, "Fix the file and run again.", 2)
	}
	if p.flag("--yes") {
		e.env["DAISUGI_APPROVE"] = "always"
		e.Environ = append(e.Environ, "DAISUGI_APPROVE=always")
	}
	pre, why := prepare(plan, env)
	if why != "" {
		return e.refuse(cmd, fmt.Errorf("%s", why))
	}
	var fallback supervise.Fallback
	if fs, _ := env.Value("fallback").(*pyjson.Object); fs != nil && fs.Value("strategy") == "tier2_recompute" {
		fallback = supervise.Recompute(e.llmClient(), env, pre.venv, 500)
	}
	j, err := e.openJournal(cmd, dataDir)
	if err != nil {
		return err
	}
	defer j.Close()
	dry := p.flag("--dry-run")
	if dry {
		e.out("Dry run — no real subprocesses will be spawned\n")
	}
	executors := supervise.DefaultExecutors()
	if dry {
		d := supervise.DryRun{}
		executors = map[string]supervise.Executor{"shell": d, "file_read": d, "file_write": d, "network": d,
			"agentic": d}
	} else {
		executors["shell"] = supervise.Shell{Environ: e.Environ}
		agentic, err := e.agentic(pre.env, agent)
		if err != nil {
			return e.fail(cmd, err)
		}
		executors["agentic"] = agentic
	}
	sup := &supervise.Supervisor{Executors: executors, Journal: j, Z3TimeoutMs: 500, StepTimeoutS: 30,
		MaxOutputBytes: 10 * 1024 * 1024, Fallback: fallback,
		Approval: supervise.Default{Getenv: e.lookup, Stdin: e.Stdin, Stdout: e.Stdout, Terminal: e.terminal}}
	sess := sup.Run(pre.plan, pre.env, pre.venv, pre.verification)
	if sup.LogErr != nil {
		return e.fail(cmd, sup.LogErr)
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(sess.JSON(), 2, true))
	} else {
		e.printRun(sess)
	}
	switch sess.Status {
	case supervise.Succeeded:
		return nil
	case supervise.Rejected:
		vs, _ := sess.Verification.Value("violations").([]any)
		for _, v := range vs {
			o := v.(*pyjson.Object)
			e.echoErr("  %s: %s\n", o.Value("stage"), o.Value("message"))
		}
		first := "Verification rejected the plan."
		if len(vs) > 0 {
			o := vs[0].(*pyjson.Object)
			first = fmt.Sprintf("[%s] %s", o.Value("stage"), o.Value("message"))
		}
		return e.fail3("Tried to run the plan "+planPath+".", first,
			"Edit the plan or widen the envelope; see the violation above.", 2)
	case supervise.Aborted:
		return exit(130)
	}
	return exit(1)
}

// clickANSI is click's _ansi_re: typer.echo drops these sequences from
// what it writes to a stream that is not a terminal.
var clickANSI = regexp.MustCompile("\x1b\\[[;?0-9]*[a-zA-Z]")

// echo is typer.echo(text) to stdout; echoErr to stderr.
func (e *Env) echo(format string, a ...any) {
	text := fmt.Sprintf(format, a...)
	if f, ok := e.Stdout.(*os.File); !ok || !supervise.IsTerminal(f) {
		text = clickANSI.ReplaceAllString(text, "")
	}
	e.out("%s", text)
}

func (e *Env) echoErr(format string, a ...any) {
	text := fmt.Sprintf(format, a...)
	if f, ok := e.Stderr.(*os.File); !ok || !supervise.IsTerminal(f) {
		text = clickANSI.ReplaceAllString(text, "")
	}
	e.errf("%s", text)
}

func pyNone(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

func (e *Env) printRun(s *supervise.Session) {
	e.echo("Run %s (%s)\n", s.ID, s.Status)
	for _, o := range s.Steps {
		rc := "None"
		if o.RC != nil {
			rc = strconv.Itoa(*o.RC)
		}
		e.echo("  %s: %s (rc=%s, approved_by=%s, %s ms)\n", o.StepID, o.Status, rc, pyNone(o.ApprovedBy),
			strconv.FormatFloat(o.DurationMs, 'f', 1, 64))
		if o.Error != nil && *o.Error != "" {
			e.echo("      error: %s\n", *o.Error)
		}
		if o.Stdout != "" {
			lines := pystr.Splitlines(pystr.RStrip(o.Stdout))
			for i, ln := range lines {
				if i == 5 {
					break
				}
				e.echo("      %s\n", ln)
			}
		}
	}
	if s.TraceID != nil {
		e.echo("Journal: %s\n", *s.TraceID)
	}
}

// terminal is sys.stdin.isatty() and sys.stdout.isatty().
func (e *Env) terminal() bool {
	in, ok1 := e.Stdin.(*os.File)
	out, ok2 := e.Stdout.(*os.File)
	return ok1 && ok2 && supervise.IsTerminal(in) && supervise.IsTerminal(out)
}
