package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"daisugi-verify/internal/gate"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/verify"
)

// typerFile is typer's Path(exists=True, dir_okay=False, readable=True):
// the usage error it gives, or nil.
func typerFile(name, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return &usageError{fmt.Sprintf("Invalid value for %s: File '%s' does not exist.", name, path)}
	}
	if st.IsDir() {
		return &usageError{fmt.Sprintf("Invalid value for %s: File '%s' is a directory.", name, path)}
	}
	if syscall.Access(path, 4) != nil {
		return &usageError{fmt.Sprintf("Invalid value for %s: File '%s' is not readable.", name, path)}
	}
	return nil
}

// verifyCmd is `daisugi verify PLAN --envelope ENVELOPE [--json]`: the
// verifier on two files, as `verify(plan, envelope)` answers. A Z3 check
// that does not finish is a warning, as in the oracle's lenient verify.
func (e *Env) verifyCmd(args []string) error {
	const cmd = "verify"
	opts := []opt{
		{names: []string{"--envelope"}, value: true, metavar: "FILE",
			help: "Path to a YAML file containing a serialized Envelope."},
		{names: []string{"--json"}, help: "Emit VerificationResult as JSON."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "{plan_path}", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " {plan_path}", "Verify an action plan against a safety envelope.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "{plan_path}", &usageError{"Missing argument 'plan_path'."})
	}
	planPath := p.args[0]
	if err := typerFile("'plan_path'", planPath); err != nil {
		return e.usageArgs(cmd, "{plan_path}", err)
	}
	if !p.has("--envelope") {
		return e.usageArgs(cmd, "{plan_path}", &usageError{"Missing option '--envelope'."})
	}
	envPath := p.str("--envelope", "")
	if err := typerFile("'--envelope'", envPath); err != nil {
		return e.usageArgs(cmd, "{plan_path}", err)
	}
	plan, perr, refusal := loadModelYAML(planPath, "ActionPlan", pmodel.ActionPlan)
	if refusal != "" {
		return e.refuse(cmd, fmt.Errorf("%s", refusal))
	}
	if perr != "" {
		return e.fail3("Tried to verify the plan "+planPath+".", "It did not parse: "+perr, "Fix the file and run again.", 2)
	}
	env, perr, refusal := loadModelYAML(envPath, "Envelope", pmodel.Envelope)
	if refusal != "" {
		return e.refuse(cmd, fmt.Errorf("%s", refusal))
	}
	if perr != "" {
		return e.fail3("Tried to verify against the envelope "+envPath+".", "It did not parse: "+perr,
			"Fix the file and run again.", 2)
	}
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("the envelope does not read: %v", err))
	}
	if why := verify.Stage2Refusal(venv); why != "" {
		return e.refuse(cmd, fmt.Errorf("%s", why))
	}
	vplan, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("the plan does not read: %v", err))
	}
	t0 := time.Now()
	r := verify.Verify(vplan, venv, verify.VerifyOptions{Z3TimeoutMs: 500})
	d := float64(time.Since(t0).Nanoseconds()) / 1e6
	dump, why := supervise.VerificationDump(r, env.Value("id"), plan.Value("id"), d)
	if why != "" {
		return e.refuse(cmd, fmt.Errorf("%s", why))
	}
	if p.flag("--json") {
		e.echo("%s\n", pyjson.DumpsIndent(dump, 2, true))
	} else {
		status := "FAILED"
		if r.OK {
			status = "OK"
		}
		e.echo("Verification: %s\n", status)
		e.echo("  plan:     %s\n", pyStrOf(plan.Value("id")))
		e.echo("  envelope: %s\n", pyStrOf(env.Value("id")))
		e.echo("  duration: %sms\n", pyFormatFixed(d, 2))
		if vs, _ := dump.Value("violations").([]any); len(vs) > 0 {
			e.echo("  violations:\n")
			for _, v := range vs {
				o := v.(*pyjson.Object)
				e.echo("    - [%s] %s\n", pyStrOf(o.Value("stage")), pyStrOf(o.Value("message")))
			}
		}
		if len(r.Warnings) > 0 {
			e.echo("  warnings:\n")
			for _, w := range r.Warnings {
				e.echo("    - %s\n", w)
			}
		}
	}
	if !r.OK {
		return exit(1)
	}
	return nil
}

// pyFormatFixed is f"{x:.{n}f}".
func pyFormatFixed(x float64, n int) string { return fmt.Sprintf("%.*f", n, x) }

// hookReport is `daisugi hook report [--pane P] [--root R]`: one pane
// state event on stdin, validated, appended to the session tree and
// delivered, as _state_report.hook_report_argv does from the command line:
// the pane identity comes from this process's own environment.
func (e *Env) hookReport(args []string) error {
	const cmd = "hook report"
	opts := []opt{
		{names: []string{"--pane"}, value: true, metavar: "TEXT", help: "Pane id to stamp onto the event, if known."},
		{names: []string{"--root"}, value: true, metavar: "PATH",
			help: "Gate data root — where the session tree this event appends to lives."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Read one PaneStateEvent JSON line from stdin and deliver it.", opts)
	}
	raw, _ := io.ReadAll(e.Stdin)
	argv := []string{"--root", gateroot.PathStr(p.str("--root", e.dataHome()+"/gate"))}
	if p.has("--pane") {
		argv = append(argv, "--pane", p.str("--pane", ""))
	}
	stdout, stderr, code := gate.HookReportCLI(argv, raw, e.Environ)
	if stdout != "" {
		e.echo("%s\n", stdout)
	}
	if stderr != "" {
		e.echoErr("%s\n", stderr)
	}
	if code != 0 {
		return exit(code)
	}
	return nil
}
