package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/rank"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tree"
	"daisugi-verify/internal/verify"
	"daisugi-verify/internal/wardenroot"
)

var treeHelp = `Usage: daisugi tree [OPTIONS] COMMAND [ARGS]...

  The delegation tree: prove each edge, keep the budgets, ask the operator.

Options:
  --help  Show this message and exit.

Commands:
  check           Prove that the child's envelope fits inside the parent's.
  root            Make a root of the tree: the operator's envelope and...
  spawn           Prove the edge, reserve the child's budget and register...
  run             Start a child of the tree on sprig, wait for it, and...
  prune-branches  Remove the worktrees of branches whose card is closed...
  adopt           Apply a branch's edits to your working tree.
  end             Record that a node ended; its parent gets the unspent...
  answer          Answer an ask: allow starts the child as proposed,...
  status          Show the tree: each node's state, budgets and deadline,...
`

// treeCmd is `daisugi tree`: tree.py through its commands.
func (e *Env) treeCmd(args []string) error {
	if len(args) == 0 {
		e.out("%s", treeHelp)
		return exit(2)
	}
	switch args[0] {
	case "--help":
		e.out("%s", treeHelp)
		return nil
	case "check":
		return e.treeCheck(args[1:])
	case "root":
		return e.treeRoot(args[1:])
	case "spawn":
		return e.treeSpawn(args[1:])
	case "run":
		return e.treeRun(args[1:])
	case "prune-branches":
		return e.treePrune(args[1:])
	case "adopt":
		return e.treeAdopt(args[1:])
	case "end":
		return e.treeEnd(args[1:])
	case "answer":
		return e.treeAnswer(args[1:])
	case "status":
		return e.treeStatus(args[1:])
	}
	e.errf("Usage: daisugi tree [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi tree --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

var treeDataOpt = opt{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."}
var treeRootOpt = opt{names: []string{"--root"}, value: true, metavar: "PATH", help: "Warden state directory (default: the data directory's warden)."}
var treeZ3Opt = opt{names: []string{"--z3-timeout-ms"}, value: true, metavar: "INTEGER", help: "Z3 budget for each proof, in ms."}

func (e *Env) treeDirs(p *parsed) (dataDir, wardenRoot string) {
	dataDir = wardenroot.PathStr(p.str("--data-dir", e.dataHome()))
	wardenRoot = wardenroot.PathStr(p.str("--root", wardenroot.Join(dataDir, "warden")))
	return dataDir, wardenRoot
}

// treeEnvelope is cli._tree_envelope: an envelope file (JSON); a bad one
// exits 2.
func (e *Env) treeEnvelope(cmd, path string) (*pyjson.Object, error) {
	what := "Tried to read the envelope in " + path + "."
	fix := "Fix the file and run again."
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, e.fail3(what, "There is no such file.", "Fix the path and run again.", 2)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, e.fail3(what, "It could not be read as UTF-8 text.", fix, 2)
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return nil, e.fail3(what, "It could not be read as UTF-8 text.", fix, 2)
	}
	doc, jerr := pyjson.LoadsPy(text, 900)
	if jerr != nil {
		if jerr.TooDeep || jerr.NotJSON {
			return nil, e.refuse(cmd, fmt.Errorf("%s holds JSON this binary does not read", path))
		}
		return nil, e.fail3(what, "It did not parse: "+strings.SplitN(jerr.Error(), "\n", 2)[0], fix, 2)
	}
	o, ok := doc.(*pyjson.Object)
	if !ok {
		return nil, e.fail3(what, "It is not a JSON object.", fix, 2)
	}
	v, verr := pmodel.Validate("Envelope", pmodel.Envelope, o, pmodel.Python)
	if verr != nil {
		return nil, e.fail3(what, "It is not a valid envelope: "+envelopeWhy(verr), fix, 2)
	}
	return v.(*pyjson.Object), nil
}

// envelopeWhy is cli._envelope_why: each error's place and message.
func envelopeWhy(verr *pmodel.ValidationError) string {
	var parts []string
	for _, er := range verr.Errs {
		var loc []string
		for _, l := range er.Loc {
			loc = append(loc, fmt.Sprint(l))
		}
		if len(loc) == 0 {
			parts = append(parts, er.Msg)
		} else {
			parts = append(parts, strings.Join(loc, ".")+": "+er.Msg)
		}
	}
	return strings.Join(parts, "; ")
}

func (e *Env) treeFail(err error) error {
	var te *tree.Error
	if errors.As(err, &te) {
		return e.fail3(te.What, te.Why, te.Fix, te.Code)
	}
	var verr *pmodel.ValidationError
	if errors.As(err, &verr) {
		return e.fail("tree", verr)
	}
	return e.fail("tree", err)
}

// treeCount is an int option, or nil when absent.
func treeCount(p *parsed, name string) (*big.Int, error) {
	if !p.has(name) {
		return nil, nil
	}
	n, err := orchInt(name, p.str(name, ""))
	if err != nil {
		return nil, err
	}
	return big.NewInt(n), nil
}

func (e *Env) treeTimeout(p *parsed) (int, error) {
	if !p.has("--z3-timeout-ms") {
		return tree.DefaultTimeoutMs, nil
	}
	n, err := orchInt("--z3-timeout-ms", p.str("--z3-timeout-ms", ""))
	return int(n), err
}

func (e *Env) treeCheck(args []string) error {
	const cmd = "tree check"
	opts := []opt{treeZ3Opt, {names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 2)
	if err != nil {
		return e.usageArgs(cmd, "PARENT CHILD", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PARENT CHILD", "Prove that the child's envelope fits inside the parent's.", opts)
	}
	if len(p.args) < 2 {
		missing := "PARENT"
		if len(p.args) == 1 {
			missing = "CHILD"
		}
		return e.usageArgs(cmd, "PARENT CHILD", &usageError{"Missing argument '" + missing + "'."})
	}
	timeout, err := e.treeTimeout(p)
	if err != nil {
		return e.usageArgs(cmd, "PARENT CHILD", err)
	}
	parent, err := e.treeEnvelope(cmd, p.args[0])
	if err != nil {
		return err
	}
	child, err := e.treeEnvelope(cmd, p.args[1])
	if err != nil {
		return err
	}
	res := tree.EdgeOK(parent, child, timeout)
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(res.Doc(), 2, true))
	} else {
		e.out("%s", res.Text())
	}
	if !res.Holds {
		return exit(1)
	}
	return nil
}

func (e *Env) treeRoot(args []string) error {
	const cmd = "tree root"
	opts := []opt{
		{names: []string{"--session"}, value: true, metavar: "TEXT", help: "The root's session id."},
		{names: []string{"--tokens"}, value: true, metavar: "INTEGER", help: "The root's token budget."},
		{names: []string{"--turns"}, value: true, metavar: "INTEGER", help: "The root's turn budget."},
		treeDataOpt, treeRootOpt,
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "ENVELOPE", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " ENVELOPE", "Make a root of the tree: the operator's envelope and budget. Only the operator runs this.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "ENVELOPE", &usageError{"Missing argument 'ENVELOPE'."})
	}
	if !p.has("--session") {
		return e.usageArgs(cmd, "ENVELOPE", &usageError{"Missing option '--session'."})
	}
	tokens, err := treeCount(p, "--tokens")
	if err != nil {
		return e.usageArgs(cmd, "ENVELOPE", err)
	}
	turns, err := treeCount(p, "--turns")
	if err != nil {
		return e.usageArgs(cmd, "ENVELOPE", err)
	}
	env, err := e.treeEnvelope(cmd, p.args[0])
	if err != nil {
		return err
	}
	dataDir, wardenRoot := e.treeDirs(p)
	session := p.str("--session", "")
	path, err := tree.MakeRoot(dataDir, wardenRoot, env, session, tokens, turns)
	if err != nil {
		return e.treeFail(err)
	}
	e.out("root %s registered → %s\n", session, path)
	return nil
}

func (e *Env) treeSpawn(args []string) error {
	const cmd = "tree spawn"
	opts := []opt{
		{names: []string{"--parent"}, value: true, metavar: "TEXT", help: "The parent's session id."},
		{names: []string{"--session"}, value: true, metavar: "TEXT", help: "The child's session id."},
		{names: []string{"--tokens"}, value: true, metavar: "INTEGER", help: "Tokens to reserve for the child."},
		{names: []string{"--turns"}, value: true, metavar: "INTEGER", help: "Turns to reserve for the child."},
		treeZ3Opt, treeDataOpt, treeRootOpt,
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "ENVELOPE", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " ENVELOPE", "Prove the edge, reserve the child's budget and register its envelope.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "ENVELOPE", &usageError{"Missing argument 'ENVELOPE'."})
	}
	for _, need := range []string{"--parent", "--session"} {
		if !p.has(need) {
			return e.usageArgs(cmd, "ENVELOPE", &usageError{"Missing option '" + need + "'."})
		}
	}
	tokens, err := treeCount(p, "--tokens")
	if err != nil {
		return e.usageArgs(cmd, "ENVELOPE", err)
	}
	turns, err := treeCount(p, "--turns")
	if err != nil {
		return e.usageArgs(cmd, "ENVELOPE", err)
	}
	timeout, err := e.treeTimeout(p)
	if err != nil {
		return e.usageArgs(cmd, "ENVELOPE", err)
	}
	env, err := e.treeEnvelope(cmd, p.args[0])
	if err != nil {
		return err
	}
	dataDir, wardenRoot := e.treeDirs(p)
	parent, session := p.str("--parent", ""), p.str("--session", "")
	res, err := tree.DoSpawn(dataDir, wardenRoot, env, parent, session, tokens, turns, timeout)
	if err != nil {
		return e.treeFail(err)
	}
	if res.Status == "started" {
		e.out("started %s under %s → %s\n", session, parent, res.Path)
		if res.Inherited != nil {
			e.out("%s takes the deadline of %s: %s.\n", session, parent, tree.Q(res.Inherited))
		}
		return nil
	}
	e.errf("not started: the edge from %s to %s is refused: %s\n", parent, session, strings.Join(res.Reasons, "; "))
	if res.Status == "asked" {
		e.errf("%s has had %d proposals refused, so this one goes to the operator as ask %s.\n", parent, tree.AskAfter, res.AskID)
		e.errf("The operator answers it with: daisugi tree answer %s allow|deny\n", res.AskID)
		return exit(3)
	}
	return exit(1)
}

// treeRun is `daisugi tree run`: the one starter. stdin is {task,
// envelope, tools, max_turns}; it spawns the child, starts sprig with the
// reservation, waits, ends it, and prints {status, session, answer,
// tokens_used, turns_used, reasons, ask_id}.
func (e *Env) treeRun(args []string) error {
	const cmd = "tree run"
	opts := []opt{
		{names: []string{"--parent"}, value: true, metavar: "TEXT", help: "The parent's session id."},
		{names: []string{"--session"}, value: true, metavar: "TEXT", help: "The child's session id (default: one minted from the parent)."},
		{names: []string{"--profile"}, value: true, metavar: "TEXT", help: "The agent profile the child runs on."},
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "The model id (free text)."},
		{names: []string{"--tokens"}, value: true, metavar: "INTEGER", help: "Tokens to reserve for the child."},
		{names: []string{"--turns"}, value: true, metavar: "INTEGER", help: "Turns to reserve for the child."},
		{names: []string{"--parent-tokens-used"}, value: true, metavar: "INTEGER", help: "The tokens the parent used itself so far."},
		{names: []string{"--parent-turns-used"}, value: true, metavar: "INTEGER", help: "The turns the parent used itself so far."},
		{names: []string{"--keys-fd"}, value: true, metavar: "INTEGER", help: "Read the resolved agent profiles, with keys, from this descriptor."},
		treeZ3Opt, treeDataOpt, treeRootOpt,
		{names: []string{"--sprig-fence"}, help: "Start nothing unless DAISUGI_SPRIG_FENCE names the fence sprig runs under."},
		{names: []string{"--parallel"}, value: true, metavar: "INTEGER", help: "Branches live at once when the request has attempts (default 4)."},
		{names: []string{"--check"}, value: true, multiple: true, metavar: "TEXT", help: "NAME=COMMAND: a hard check run in each branch's worktree."},
		{names: []string{"--judge"}, value: true, metavar: "TEXT", help: "PROFILE/MODEL that judges the branches (another family)."},
		{names: []string{"--max-run-s"}, value: true, metavar: "INTEGER", help: "Kill the whole sprig run past this many seconds (default: no cap). The envelope's max_execution_time_s caps each step."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "[-]", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " [-]", "Start a child of the tree on sprig, wait for it, and end it: the one starter.", opts)
	}
	for _, need := range []string{"--parent", "--profile"} {
		if !p.has(need) {
			return e.usageArgs(cmd, "[-]", &usageError{"Missing option '" + need + "'."})
		}
	}
	counts := map[string]*big.Int{}
	for _, name := range []string{"--tokens", "--turns", "--parent-tokens-used", "--parent-turns-used", "--keys-fd", "--parallel", "--max-run-s"} {
		n, err := treeCount(p, name)
		if err != nil {
			return e.usageArgs(cmd, "[-]", err)
		}
		counts[name] = n
	}
	timeout, err := e.treeTimeout(p)
	if err != nil {
		return e.usageArgs(cmd, "[-]", err)
	}
	what := "Tried to read the tree run request on stdin."
	fix := "Fix the request and run again."
	if len(p.args) > 0 && p.args[0] != "-" {
		return e.fail3(what, "The only argument is -, for stdin.", fix, 2)
	}
	data, _ := io.ReadAll(e.Stdin)
	req, why := treeRunRequest(data)
	if why != "" {
		return e.fail3(what, why, fix, 2)
	}
	envDoc, _ := req.Value("envelope").(*pyjson.Object)
	v, verr := pmodel.Validate("Envelope", pmodel.Envelope, envDoc, pmodel.Python)
	if verr != nil {
		return e.fail3(what, "envelope is not a valid envelope: "+envelopeWhy(verr), fix, 2)
	}
	envelope := v.(*pyjson.Object)
	var bundle []byte
	if fd := counts["--keys-fd"]; fd != nil {
		f := os.NewFile(uintptr(fd.Int64()), "keys")
		b, rerr := io.ReadAll(f)
		if f != nil {
			f.Close()
		}
		if f == nil || rerr != nil {
			return e.fail3("Tried to read the agent profiles.", "Descriptor "+fd.String()+" cannot be read.",
				"Pass the descriptor the profiles are on.", 2)
		}
		bundle = b
	}
	profiles := agentprof.FromBundle(bundle)
	dataDir, wardenRoot := e.treeDirs(p)
	self, err := install.Self()
	if err != nil {
		return e.fail(cmd, err)
	}
	sprig := "sprig"
	if s, ok := e.lookup("DAISUGI_SPRIG"); ok {
		sprig = s
	}
	cwd, err := os.Getwd()
	if err != nil {
		return e.fail(cmd, err)
	}
	starter := &supervise.Starter{Self: self, TempDir: e.gettempdir(), Sprig: sprig, Environ: e.Environ,
		LookPath: func(name string) (string, error) {
			if strings.Contains(name, "/") {
				return name, nil
			}
			return lookPath(name, e.env["PATH"])
		}}
	// DAISUGI_SPRIG_FENCE: the fence prefix sprig starts under (FN-R-25).
	// A value that is not a list of words refuses, and so does a run that
	// coppice marks --sprig-fence with no fence named: never run unfenced
	// by accident (FN-R-28).
	if raw, ok := e.lookup("DAISUGI_SPRIG_FENCE"); ok && raw != "" {
		var words []string
		if json.Unmarshal([]byte(raw), &words) != nil || len(words) < 2 {
			return e.fail3("Tried to start sprig under the fence.", "DAISUGI_SPRIG_FENCE is not a JSON list of the fence's words.",
				"Set it to the fence command, as coppice does, or unset it.", 2)
		}
		starter.Fence = words
	} else if p.flag("--sprig-fence") {
		return e.fail3("Tried to start sprig under the fence.", "--sprig-fence is set, but DAISUGI_SPRIG_FENCE is not.",
			"Set DAISUGI_SPRIG_FENCE to the fence command, as coppice does.", 2)
	}
	var session *string
	if p.has("--session") {
		s := p.str("--session", "")
		session = &s
	}
	profile := p.str("--profile", "")
	tools := []string{}
	for _, t := range req.Value("tools").([]any) {
		tools = append(tools, t.(string))
	}
	var maxTurns *big.Int
	if mt, ok := req.Value("max_turns").(pyjson.Int); ok {
		maxTurns, _ = new(big.Int).SetString(mt.Text, 10)
	}
	// The envelope's max_execution_time_s is the cap of each step sprig
	// takes. The run as a whole has no cap unless --max-run-s sets one: a
	// long task is a long run, and leads exist for the longest (round 9).
	maxRunS := 0
	if n := counts["--max-run-s"]; n != nil {
		if !n.IsInt64() || n.Int64() > 1<<31-1 {
			return e.usageArgs(cmd, "[-]", &usageError{"--max-run-s is too large."})
		}
		maxRunS = int(n.Int64())
	}
	timeoutS := 30
	if perms, ok := envelope.Value("permissions").(*pyjson.Object); ok {
		if n, ok := perms.Value("max_execution_time_s").(pyjson.Int); ok {
			if x, err := strconv.Atoi(n.Text); err == nil {
				timeoutS = x
			}
		}
	}
	if at, ok := req.Value("attempts").(pyjson.Int); ok && at.Text != "1" {
		if session != nil {
			return e.fail3(what, "--session cannot name a child when the request has attempts.", fix, 2)
		}
		attempts, _ := strconv.Atoi(at.Text)
		if req.Value("resume") != nil {
			return e.fail3(what, "resume cannot be combined with attempts.", fix, 2)
		}
		tools = supervise.WithoutHelp(tools)
		var checks []supervise.Check
		for _, raw := range p.vals["--check"] {
			name, cmdText, sep := strings.Cut(raw, "=")
			argv, serr := verify.ShlexSplit(cmdText)
			if serr != nil {
				argv = nil
			}
			if !sep || name == "" || len(argv) == 0 {
				return e.fail3(what, "--check wants NAME=COMMAND, not "+pystr.Repr(raw)+".", fix, 2)
			}
			checks = append(checks, supervise.Check{Name: name, Argv: argv, Required: true})
		}
		var judge *string
		if p.has("--judge") {
			j := p.str("--judge", "")
			judge = &j
		}
		par := 0
		if counts["--parallel"] != nil {
			par = int(counts["--parallel"].Int64())
		}
		bo := supervise.BranchOpts{Parent: p.str("--parent", ""), Envelope: envelope, Profiles: profiles,
			ProfileName: &profile, Model: p.str("--model", ""), Tokens: counts["--tokens"], Turns: counts["--turns"],
			ParentTokensUsed: counts["--parent-tokens-used"], ParentTurnsUsed: counts["--parent-turns-used"],
			Task: req.Value("task").(string), Tools: tools, ToolsRepr: pmodel.Repr(req.Value("tools")),
			MaxTurns: maxTurns, Cwd: cwd, TimeoutS: maxRunS, StepCapS: timeoutS, TimeoutMs: timeout, Attempts: attempts,
			Why: req.Value("why").(string), Parallel: par, Checks: checks, Judge: judge}
		if judge != nil {
			bo.JudgeFn = e.branchJudge(profiles)
		}
		run, rerr := starter.RunBranches(dataDir, wardenRoot, bo)
		if rerr != nil {
			return e.treeFail(rerr)
		}
		e.out("%s\n", pyjson.Dumps(run.Doc(), true))
		switch run.Status {
		case "ran":
			return nil
		case "asked":
			return exit(3)
		}
		if run.Usage {
			return exit(2)
		}
		return exit(1)
	}
	resume, _ := req.Value("resume").(string)
	node, err := starter.RunNode(dataDir, wardenRoot, supervise.NodeOpts{Resume: resume, Parent: p.str("--parent", ""),
		Session: session, Envelope: envelope, Profiles: profiles, ProfileName: &profile,
		Model: p.str("--model", ""), Tokens: counts["--tokens"], Turns: counts["--turns"],
		ParentTokensUsed: counts["--parent-tokens-used"], ParentTurnsUsed: counts["--parent-turns-used"],
		Task: req.Value("task").(string), Tools: tools, ToolsRepr: pmodel.Repr(req.Value("tools")),
		MaxTurns: maxTurns, Cwd: cwd, TimeoutS: maxRunS, StepCapS: timeoutS, DefaultModel: "haiku", TimeoutMs: timeout})
	if err != nil {
		return e.treeFail(err)
	}
	e.out("%s\n", pyjson.Dumps(node.Doc(), true))
	switch node.Status {
	case "started":
		return nil
	case "asked":
		return exit(3)
	}
	return exit(1)
}

// resumeShape is what a session id looks like in the resume field.
var resumeShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// treeRunRequest is tree_run.read_request: the request, or a line that
// says what is wrong.
func treeRunRequest(data []byte) (*pyjson.Object, string) {
	text, derr := pystr.DecodeStrict(data)
	if derr != nil {
		return nil, "It could not be read as UTF-8 text."
	}
	v, jerr := pyjson.LoadsPy(text, 900)
	if jerr != nil {
		return nil, "It did not parse: " + strings.SplitN(jerr.Error(), "\n", 2)[0]
	}
	doc, ok := v.(*pyjson.Object)
	if !ok {
		return nil, "It is not a JSON object."
	}
	if t, ok := doc.Value("task").(string); !ok || pystr.Strip(t) == "" {
		return nil, "task must be text."
	}
	if _, ok := doc.Value("envelope").(*pyjson.Object); !ok {
		return nil, "envelope must be an object."
	}
	tools, ok := doc.Value("tools").([]any)
	if !ok {
		return nil, "tools must be a list of text."
	}
	for _, t := range tools {
		if _, ok := t.(string); !ok {
			return nil, "tools must be a list of text."
		}
	}
	// resume: the session of an earlier segment of this worker.
	if r := doc.Value("resume"); r != nil {
		rs, ok := r.(string)
		if !ok || !resumeShape.MatchString(rs) {
			return nil, "resume must be a session id."
		}
	}
	if mt := doc.Value("max_turns"); mt != nil {
		n, ok := mt.(pyjson.Int)
		v, okk := new(big.Int).SetString(n.Text, 10)
		if !ok || !okk || v.Sign() < 1 {
			return nil, "max_turns must be a whole number of 1 or more."
		}
	}
	// tree_run.read_request: attempts is 1 or absent, or 2 to 8 with a why.
	if at := doc.Value("attempts"); at != nil {
		single := false
		switch x := at.(type) {
		case pyjson.Int:
			single = x.Text == "1"
		}
		if !single {
			n, ok := at.(pyjson.Int)
			v, okk := new(big.Int).SetString(n.Text, 10)
			if !ok || !okk || v.Cmp(big.NewInt(2)) < 0 || v.Cmp(big.NewInt(8)) > 0 {
				return nil, "attempts must be a whole number from 2 to 8."
			}
			switch doc.Value("why") {
			case "failed_verify", "failed_tests", "low_confidence", "operator":
			default:
				return nil, "why must be one of failed_verify, failed_tests, low_confidence, operator."
			}
		}
	}
	return doc, ""
}

func (e *Env) treeEnd(args []string) error {
	const cmd = "tree end"
	opts := []opt{
		{names: []string{"--tokens-used"}, value: true, metavar: "INTEGER", help: "Tokens it used."},
		{names: []string{"--turns-used"}, value: true, metavar: "INTEGER", help: "Turns it used."},
		treeDataOpt, treeRootOpt,
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "SESSION", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " SESSION", "Record that a node ended; its parent gets the unspent budget back and its envelope is unregistered.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "SESSION", &usageError{"Missing argument 'SESSION'."})
	}
	tokens, err := treeCount(p, "--tokens-used")
	if err != nil {
		return e.usageArgs(cmd, "SESSION", err)
	}
	turns, err := treeCount(p, "--turns-used")
	if err != nil {
		return e.usageArgs(cmd, "SESSION", err)
	}
	dataDir, wardenRoot := e.treeDirs(p)
	session := p.args[0]
	node, err := tree.End(dataDir, wardenRoot, session, tokens, turns)
	if err != nil {
		return e.treeFail(err)
	}
	e.out("ended %s; its envelope is unregistered.\n", session)
	if node.Parent != nil {
		for _, ax := range []struct {
			name           string
			reserved, used *big.Int
		}{{"tokens", node.Tokens, node.TokensUsed}, {"turns", node.Turns, node.TurnsUsed}} {
			if ax.reserved != nil && ax.used != nil {
				back := new(big.Int).Sub(ax.reserved, ax.used)
				if back.Sign() < 0 {
					back.SetInt64(0)
				}
				e.out("%s: used %s of %s; %s go back to %s.\n", ax.name, ax.used, ax.reserved, back, *node.Parent)
			}
		}
	}
	return nil
}

func (e *Env) treeAnswer(args []string) error {
	const cmd = "tree answer"
	opts := []opt{treeDataOpt, treeRootOpt}
	p, err := parseArgs(args, opts, 2)
	if err != nil {
		return e.usageArgs(cmd, "ASK_ID VERDICT", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " ASK_ID VERDICT", "Answer an ask: allow starts the child as proposed, marked not proved. Only the operator runs this.", opts)
	}
	if len(p.args) < 2 {
		missing := "ASK_ID"
		if len(p.args) == 1 {
			missing = "VERDICT"
		}
		return e.usageArgs(cmd, "ASK_ID VERDICT", &usageError{"Missing argument '" + missing + "'."})
	}
	dataDir, wardenRoot := e.treeDirs(p)
	id := p.args[0]
	ask, path, err := tree.Answer(dataDir, wardenRoot, id, p.args[1])
	if err != nil {
		return e.treeFail(err)
	}
	if path == "" {
		e.out("denied %s; %s may propose again.\n", id, ask.Value("parent"))
	} else {
		e.out("allowed %s: started %s under %s, not proved → %s\n", id, ask.Value("session"), ask.Value("parent"), path)
	}
	return nil
}

func (e *Env) treeStatus(args []string) error {
	const cmd = "tree status"
	opts := []opt{treeDataOpt, {names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Show the tree: each node's state, budgets and deadline, and the open asks. Reads only.", opts)
	}
	dataDir, _ := e.treeDirs(p)
	doc := tree.StatusDoc(dataDir)
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(doc, 2, true))
	} else {
		e.out("%s", tree.StatusText(doc))
	}
	return nil
}

// branchJudge is the judge loop of tree_branch.run_branches: ask the judge
// while the fit names a next pair and a vote was recorded.
func (e *Env) branchJudge(profiles agentprof.Profiles) func(doc *pyjson.Object, dataDir string, res *pyjson.Object) (*pyjson.Object, error) {
	return func(doc *pyjson.Object, dataDir string, res *pyjson.Object) (*pyjson.Object, error) {
		client := e.llmClient()
		call := e.judgeCall(client)
		getenv := func(k string) string { return e.env[k] }
		for res.Value("next") != nil {
			out, err := rank.JudgeOnce(func() (*rank.Ranking, error) { return rank.Parse(doc) }, dataDir, profiles, getenv,
				nil, rank.Now(), call)
			if err != nil {
				return nil, err
			}
			if !out.Recorded {
				break
			}
			res = out.Fit
		}
		return res, nil
	}
}

// treePrune is `tree prune-branches`.
func (e *Env) treePrune(args []string) error {
	const cmd = "tree prune-branches"
	opts := []opt{treeDataOpt, {names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Remove the worktrees of branches whose card is closed or never opened.", opts)
	}
	dataDir, _ := e.treeDirs(p)
	removed := supervise.PruneBranches(dataDir)
	switch {
	case p.flag("--json"):
		l := make([]any, len(removed))
		for i, r := range removed {
			l[i] = r
		}
		e.out("%s\n", pyjson.Dumps(pyjson.NewObject().Set("removed", l), true))
	case len(removed) > 0:
		e.out("Removed the worktrees of %s.\n", strings.Join(removed, ", "))
	default:
		e.out("No worktree to remove.\n")
	}
	return nil
}

// treeAdopt is `tree adopt`.
func (e *Env) treeAdopt(args []string) error {
	const cmd = "tree adopt"
	opts := []opt{{names: []string{"--cwd"}, value: true, metavar: "TEXT", help: "The tree to apply to. Default: the repository the branch came from."},
		treeDataOpt, {names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "SESSION", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " SESSION", "Apply a branch's edits to your working tree.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "SESSION", &usageError{"Missing argument 'SESSION'."})
	}
	dataDir, _ := e.treeDirs(p)
	res := supervise.Adopt(dataDir, p.args[0], p.str("--cwd", ""))
	if p.flag("--json") {
		e.out("%s\n", pyjson.Dumps(res.Doc(), true))
	} else {
		e.out("%s\n", res.Why)
	}
	if res.Status == "refused" {
		return exit(2)
	}
	return nil
}
