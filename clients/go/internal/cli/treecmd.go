package cli

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tree"
)

var treeHelp = `Usage: daisugi tree [OPTIONS] COMMAND [ARGS]...

  The delegation tree: prove each edge, keep the budgets, ask the operator.

Options:
  --help  Show this message and exit.

Commands:
  check   Prove that the child's envelope fits inside the parent's.
  root    Make a root of the tree: the operator's envelope and budget.
  spawn   Prove the edge, reserve the child's budget and register its...
  run     Start a child of the tree on sprig, wait for it, and end it:...
  end     Record that a node ended; its parent gets the unspent budget...
  answer  Answer an ask: allow starts the child as proposed, marked not...
  status  Show the tree: each node's state, budgets and deadline, and...
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
var treeRootOpt = opt{names: []string{"--root"}, value: true, metavar: "PATH", help: "Gate state directory (default: the data directory's gate)."}
var treeZ3Opt = opt{names: []string{"--z3-timeout-ms"}, value: true, metavar: "INTEGER", help: "Z3 budget for each proof, in ms."}

func (e *Env) treeDirs(p *parsed) (dataDir, gateRoot string) {
	dataDir = gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	gateRoot = gateroot.PathStr(p.str("--root", gateroot.Join(dataDir, "gate")))
	return dataDir, gateRoot
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
	dataDir, gateRoot := e.treeDirs(p)
	session := p.str("--session", "")
	path, err := tree.MakeRoot(dataDir, gateRoot, env, session, tokens, turns)
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
	dataDir, gateRoot := e.treeDirs(p)
	parent, session := p.str("--parent", ""), p.str("--session", "")
	res, err := tree.DoSpawn(dataDir, gateRoot, env, parent, session, tokens, turns, timeout)
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
	for _, name := range []string{"--tokens", "--turns", "--parent-tokens-used", "--parent-turns-used", "--keys-fd"} {
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
	dataDir, gateRoot := e.treeDirs(p)
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
	timeoutS := 30
	if perms, ok := envelope.Value("permissions").(*pyjson.Object); ok {
		if n, ok := perms.Value("max_execution_time_s").(pyjson.Int); ok {
			if x, err := strconv.Atoi(n.Text); err == nil {
				timeoutS = x
			}
		}
	}
	node, err := starter.RunNode(dataDir, gateRoot, supervise.NodeOpts{Parent: p.str("--parent", ""),
		Session: session, Envelope: envelope, Profiles: profiles, ProfileName: &profile,
		Model: p.str("--model", ""), Tokens: counts["--tokens"], Turns: counts["--turns"],
		ParentTokensUsed: counts["--parent-tokens-used"], ParentTurnsUsed: counts["--parent-turns-used"],
		Task: req.Value("task").(string), Tools: tools, ToolsRepr: pmodel.Repr(req.Value("tools")),
		MaxTurns: maxTurns, Cwd: cwd, TimeoutS: timeoutS, DefaultModel: "haiku", TimeoutMs: timeout})
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
	if mt := doc.Value("max_turns"); mt != nil {
		n, ok := mt.(pyjson.Int)
		v, okk := new(big.Int).SetString(n.Text, 10)
		if !ok || !okk || v.Sign() < 1 {
			return nil, "max_turns must be a whole number of 1 or more."
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
	dataDir, gateRoot := e.treeDirs(p)
	session := p.args[0]
	node, err := tree.End(dataDir, gateRoot, session, tokens, turns)
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
	dataDir, gateRoot := e.treeDirs(p)
	id := p.args[0]
	ask, path, err := tree.Answer(dataDir, gateRoot, id, p.args[1])
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
