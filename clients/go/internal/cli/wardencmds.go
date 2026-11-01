package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/journal"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/statefile"
	"daisugi-verify/internal/tree"
	"daisugi-verify/internal/verify"
	"daisugi-verify/internal/warden"
	"daisugi-verify/internal/wardenroot"
)

var rootOpt = opt{names: []string{"--root"}, value: true, metavar: "PATH",
	help: "Warden state directory (envelopes, audit log, disarm marker)."}
var jsonOpt = opt{names: []string{"--json"}, help: "Machine-readable JSON output."}

func (e *Env) root(p *parsed) string {
	if p.has("--root") {
		return wardenroot.PathStr(p.str("--root", ""))
	}
	return wardenroot.Join(e.dataHome(), "warden")
}

// cmdHelp prints a command's help from its options.
func (e *Env) cmdHelp(cmd, args, summary string, opts []opt) error {
	e.out("Usage: daisugi %s [OPTIONS]%s\n\n  %s\n\nOptions:\n", cmd, args, summary)
	for _, o := range opts {
		if o.hidden {
			continue
		}
		name := strings.Join(o.names, ", ")
		if o.neg != "" {
			name += " / " + o.neg
		}
		if o.value {
			name += " " + o.metavar
		}
		e.out("  %-34s %s\n", name, o.help)
	}
	e.out("  %-34s %s\n", "--help", "Show this message and exit.")
	return nil
}

func (e *Env) wardenDisarm(args []string) error {
	opts := []opt{rootOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("warden disarm", err)
	}
	if p.help {
		return e.cmdHelp("warden disarm", "", "Kill switch: the warden allows everything until re-armed.", opts)
	}
	marker, err := wardenroot.Disarm(e.root(p))
	if err != nil {
		if wr := writeRefused(err); wr != nil {
			e.errf("warden NOT disarmed: %s\n", wr.Text)
			return exit(1)
		}
		return e.fail("warden disarm", err)
	}
	e.out("warden DISARMED (marker: %s) — `daisugi warden arm` to re-enable\n", marker)
	return nil
}

func (e *Env) wardenArm(args []string) error {
	opts := []opt{rootOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("warden arm", err)
	}
	if p.help {
		return e.cmdHelp("warden arm", "", "Remove the disarm marker; the warden resumes evaluating calls.", opts)
	}
	if err := wardenroot.Arm(e.root(p)); err != nil {
		if wr := writeRefused(err); wr != nil {
			e.errf("warden NOT armed: %s\n", wr.Text)
			return exit(1)
		}
		return e.fail("warden arm", err)
	}
	e.out("warden armed\n")
	return nil
}

// cwd is Path.cwd(), or home when the directory is gone, as warden status
// falls back.
func (e *Env) cwd() string {
	c, err := wardenroot.Getwd()
	if err != nil {
		return e.home
	}
	return c
}

func (e *Env) wardenStatus(args []string) error {
	opts := []opt{rootOpt, jsonOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("warden status", err)
	}
	if p.help {
		return e.cmdHelp("warden status", "", "Show armed/disarmed state, the verdict mode, and the registered envelopes.", opts)
	}
	root := e.root(p)
	armed := !wardenroot.IsDisarmed(root)
	eff, err := config.EffectiveHookMode(e.home, e.cwd())
	if err != nil {
		return e.refuse("warden status", fmt.Errorf("a Claude Code settings.json holds hooks this binary does not read yet"))
	}
	mode, source := eff.Mode, config.SourceLabel(eff)
	if mode == "" {
		mode, err = config.WardenMode(wardenroot.Join(wardenroot.Parent(root), "config.yaml"))
		if err != nil {
			return e.refuse("warden status", err)
		}
		source = "config"
	}
	envs, err := wardenroot.Envelopes(root)
	if err != nil {
		return e.refuse("warden status", err)
	}
	// A hook whose program is gone fails on every call: an enforce hook
	// then denies them all. Say so on stderr, whatever the output format.
	files := []string{e.home + "/.claude/settings.json"}
	if c := e.cwd() + "/.claude/settings.json"; c != files[0] {
		files = append(files, c)
	}
	var gone []string
	for _, f := range files {
		for _, g := range config.MissingHookPrograms(f) {
			gone = append(gone, config.MissingHookWarning(f, g))
		}
	}
	for _, f := range files {
		if config.StaleModeHooks(f) {
			gone = append(gone, config.StaleModeWarning(f))
		}
	}
	// An old config.yaml may name a verifier client this box lacks (a
	// restored Python-era config). The warden still decides; say so once.
	if cfg, cerr := config.Load(wardenroot.Join(wardenroot.Parent(root), "config.yaml")); cerr == nil {
		if msg := warden.ClientProblem(cfg.VerifierClient, e.env); msg != "" {
			gone = append(gone, msg)
		}
	}
	dialect, err := wardenDialectStatus(root, mode)
	if err != nil {
		return e.refuse("warden status", err)
	}
	hints := e.renameHints()
	// Each path the warden refuses to read, with the fix (GR-R-8); the key and
	// the lines appear only when there is one.
	refused := statefile.RefusedPaths(root)
	openFiles := e.unconfined(root)
	if p.flag("--json") {
		o := pyjson.NewObject().Set("armed", armed).Set("mode", mode).Set("mode_source", source).
			Set("envelopes", strsAny(envs)).Set("dialect", dialect)
		if len(refused) > 0 {
			o.Set("refused", strsAny(refused))
		}
		if openFiles {
			o.Set("unconfined", true)
		}
		e.out("%s\n", pyjson.Dumps(o, true))
		for _, g := range append(gone, hints...) {
			e.errf("%s\n", g)
		}
		return nil
	}
	state := "armed"
	if !armed {
		state = "DISARMED"
	}
	shown := mode
	if mode == config.KindUnknown {
		// A warden hook in a form this CLI does not read. It may enforce.
		shown = "unknown warden hook"
	}
	e.out("warden: %s · mode: %s (%s)\n", state, shown, source)
	if len(envs) == 0 {
		e.out("no envelopes registered — enforce mode would deny everything\n")
	}
	for _, name := range envs {
		e.out("  envelope: %s\n", name)
	}
	for _, line := range refused {
		e.out("refused: %s\n", line)
	}
	e.out("%s\n", wardenDialectLine(dialect))
	if openFiles {
		e.out("%s\n", unconfinedLine)
	}
	for _, h := range hints {
		e.out("%s\n", h)
	}
	for _, g := range gone {
		e.errf("%s\n", g)
	}
	return nil
}

// wardenDialectStatus is cli._warden_dialect_status: the system dialect's hash,
// the pin config.yaml names, the mode that follows, and the calls a word
// would deny.
func wardenDialectStatus(root, mode string) (*pyjson.Object, error) {
	pin, err := config.DialectPin(wardenroot.Join(wardenroot.Parent(root), "config.yaml"))
	if err != nil {
		return nil, err
	}
	n, err := journal.WordWouldDeny(root)
	if err != nil {
		return nil, err
	}
	state, source := "audit", "none"
	var pinV any
	switch {
	case pin != nil && *pin != verify.DialectHash:
		pinV = *pin
		state, source = "mismatch", "pin"
	case pin != nil:
		pinV = *pin
		state, source = "enforced", "pin"
	case mode == "enforce":
		state, source = "enforced", "warden_mode"
	}
	return pyjson.NewObject().Set("hash", verify.DialectHash).Set("pin", pinV).Set("mode", state).
		Set("source", source).Set("word_would_deny", n), nil
}

// wardenDialectLine is cli._warden_dialect_line.
func wardenDialectLine(d *pyjson.Object) string {
	var head string
	switch {
	case d.Value("mode") == "mismatch":
		head = fmt.Sprintf("dialect %s: pin mismatch: %s is not this build's %s; pin is from an earlier dialect; set dialect_enforce: %s or remove it",
			d.Value("hash"), d.Value("pin"), d.Value("hash"), d.Value("hash"))
	case d.Value("source") == "pin":
		head = fmt.Sprintf("dialect %s: enforced (pinned)", d.Value("hash"))
	case d.Value("mode") == "enforced":
		head = fmt.Sprintf("dialect %s: enforced (warden mode enforce)", d.Value("hash"))
	default:
		head = fmt.Sprintf("dialect %s: audit (warden mode audit)", d.Value("hash"))
	}
	return fmt.Sprintf("%s · word would-denies: %d", head, d.Value("word_would_deny"))
}

func strsAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (e *Env) wardenReport(args []string) error {
	opts := []opt{{names: []string{"--session"}, value: true, metavar: "TEXT", help: "One session's log only."}, rootOpt,
		{names: []string{"--json"}, help: "Emit the full report as JSON."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("warden report", err)
	}
	if p.help {
		return e.cmdHelp("warden report", "", "Summarize the audit log: what an enforcing warden would have denied.", opts)
	}
	session := p.str("--session", "")
	files, err := journal.Files(e.root(p), session, session != "")
	if err == nil {
		var rep *journal.Report
		if rep, err = journal.Build(files); err == nil {
			if p.flag("--json") {
				e.out("%s\n", rep.JSON())
				return nil
			}
			var text string
			if text, err = rep.Text(); err == nil {
				e.out("%s", text)
				return nil
			}
		}
	}
	var crash *journal.CrashError
	if errors.As(err, &crash) {
		return e.fail("warden report", err)
	}
	return e.refuse("warden report", err)
}

func (e *Env) wardenSettings(args []string) error {
	opts := []opt{
		{names: []string{"--enforce"}, help: "Emit enforce-mode settings (default is audit: observation only)."},
		rootOpt,
		{names: []string{"--format"}, value: true, metavar: "TEXT", help: "Host contract (default claude)."},
		{names: []string{"--session"}, value: true, metavar: "TEXT",
			help: "Pin the warden to this registered session's envelope."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("warden settings", err)
	}
	if p.help {
		return e.cmdHelp("warden settings", "", "Print the Claude Code hooks-settings JSON that wires in the warden. "+
			"Usage: claude --settings \"$(daisugi warden settings)\"", opts)
	}
	self, err := install.Self()
	if err != nil {
		return e.fail("warden settings", err)
	}
	o := install.HookOptions{Mode: "audit", Root: e.root(p), Format: p.str("--format", "claude")}
	if p.flag("--enforce") {
		o.Mode = "enforce"
	}
	if p.has("--session") {
		s := p.str("--session", "")
		o.Session = &s
	}
	e.out("%s\n", install.SettingsJSON(self, o))
	return nil
}

// decomposeDefault is cli._resolve_decompose for an unset flag.
func (e *Env) decomposeDefault(root, cmd string) (bool, error) {
	cfg, err := config.Load(wardenroot.Join(wardenroot.Parent(root), "config.yaml"))
	if errors.Is(err, config.ErrInvalid) {
		return false, e.fail(cmd, err)
	}
	if err != nil {
		return false, e.refuse(cmd, err)
	}
	return cfg.ShellAllowDecomposition, nil
}

var decomposeOpt = opt{names: []string{"--allow-shell-decomposition"}, neg: "--no-allow-shell-decomposition",
	help: "Let the envelope admit compound shell: 'a && b' and pipes, every head checked. " +
		"Unset reads shell_allow_decomposition from config.yaml."}

func (e *Env) wardenInit(args []string) error {
	opts := []opt{
		{names: []string{"--workspace"}, value: true, metavar: "PATH", help: "The directory the session may read/write (default: cwd)."},
		{names: []string{"--session"}, value: true, metavar: "TEXT", help: "Bind the envelope to this session id."},
		rootOpt,
		{names: []string{"--force"}, help: "Overwrite an existing envelope."},
		decomposeOpt,
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("warden init", err)
	}
	if p.help {
		return e.cmdHelp("warden init", "", "Generate and register a reviewable starter envelope for this session.", opts)
	}
	root := e.root(p)
	var decompose bool
	if p.flagSet("--allow-shell-decomposition") {
		decompose = p.flag("--allow-shell-decomposition")
	} else if decompose, err = e.decomposeDefault(root, "warden init"); err != nil {
		return err
	}
	session := p.str("--session", "")
	name := session
	if name == "" {
		name = "default"
	}
	// The file Register writes: the session id made safe, so the check
	// and the write look at one path.
	file := "default"
	if session != "" {
		file = wardenroot.SafeSessionID(session)
	}
	target := wardenroot.Join(wardenroot.EnvelopesDir(root), file+".json")
	if _, err := os.Stat(target); err == nil && !p.flag("--force") {
		e.errf("an envelope for '%s' is already registered at %s; pass --force to overwrite\n", name, target)
		return exit(1)
	}
	var ws string
	if p.has("--workspace") {
		ws = p.str("--workspace", "")
	} else if ws, err = wardenroot.Getwd(); err != nil {
		return e.fail("warden init", err)
	}
	if ws, err = wardenroot.Resolve(ws); err != nil {
		return e.fail("warden init", err)
	}
	env, err := wardenroot.StarterEnvelope(ws, decompose)
	if err != nil {
		return e.fail("warden init", err)
	}
	path, err := wardenroot.Register(env, session, root)
	if err != nil {
		if wr := writeRefused(err); wr != nil {
			e.errf("not registered: %s\n", wr.Text)
			return exit(1)
		}
		return e.fail("warden init", err)
	}
	e.out("registered a starter envelope for %s → %s\n", ws, path)
	e.out("REVIEW it before enforcing — it is a tight default, not a finished policy.\n")
	e.out("Then launch audit mode:\n")
	// A session's envelope is read only by a warden pinned to it.
	pin := ""
	if session != "" {
		pin = " --session " + verify.ShlexQuote(session)
	}
	e.out("  claude --settings \"$(daisugi warden settings --root %s%s)\"\n", root, pin)
	return nil
}

func (e *Env) wardenRegister(args []string) error {
	opts := []opt{
		{names: []string{"--session"}, value: true, metavar: "TEXT",
			help: "Bind to one session id; omit to register the default envelope."},
		rootOpt,
		{names: []string{"--parent"}, value: true, metavar: "TEXT",
			help: "Register a child of this session: the edge to it is proved first, and a refused edge registers nothing."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage("warden register", err)
	}
	if p.help {
		return e.cmdHelp("warden register", " ENVELOPE_PATH", "Register the envelope the warden checks this session's calls against.", opts)
	}
	if len(p.args) == 0 {
		return e.usage("warden register", &usageError{"Missing argument 'ENVELOPE_PATH'."})
	}
	raw, err := os.ReadFile(p.args[0])
	if err != nil {
		return e.fail("warden register", err)
	}
	if !utf8.Valid(raw) {
		return e.fail("warden register", fmt.Errorf("%s is not valid UTF-8", p.args[0]))
	}
	text := string(raw)
	// yaml.safe_load reads the file, JSON included, as YAML 1.1: to it
	// 1e-09 is a string, not a number.
	y, err := config.ParseYAML(text)
	var yerr *config.YAMLError
	if errors.As(err, &yerr) {
		// safe_load raises: the oracle's traceback names the exception.
		return e.fail("warden register", yerr)
	}
	if err != nil {
		return e.refuse("warden register", errors.New("the envelope uses YAML this binary does not read yet"))
	}
	v, err := yamlToJSON(y)
	if err != nil {
		return e.refuse("warden register", err)
	}
	in, isObj := v.(*pyjson.Object)
	if !isObj {
		return e.fail("warden register", errors.New("the envelope file does not hold a mapping"))
	}
	env, err := wardenroot.Validate(in)
	var inv *wardenroot.InvalidError
	if errors.As(err, &inv) {
		return e.fail("warden register", err)
	}
	if err != nil {
		return e.refuse("warden register", err)
	}
	if unchecked := opaqueUnrecognized(env); len(unchecked) > 0 {
		e.errf("envelope declares %d item(s) nothing can check: %s. Give each an expr, name a word (keep_unchanged), or set enforce: false.\n",
			len(unchecked), strings.Join(unchecked, ", "))
		return exit(2)
	}
	if refused := e.lintMCPRules(env, e.root(p)); refused {
		return exit(2)
	}
	session := p.str("--session", "")
	if p.has("--parent") {
		env, err = e.provedChild(env, session, p.str("--parent", ""), e.root(p))
		if err != nil {
			return err
		}
	}
	path, err := wardenroot.Register(env, session, e.root(p))
	if err != nil {
		if wr := writeRefused(err); wr != nil {
			e.errf("not registered: %s\n", wr.Text)
			return exit(1)
		}
		return e.fail("warden register", err)
	}
	which := "default"
	if session != "" {
		which = "session " + session
	}
	e.out("registered %s envelope → %s\n", which, path)
	return nil
}

// opaqueUnrecognized is models.opaque_unrecognized over a validated envelope:
// the type of each enforced item that declares a safety property nothing can
// check (SG-8, SG-10), invariants first, in order.
func opaqueUnrecognized(env *pyjson.Object) []string {
	var out []string
	for _, kv := range []struct{ key, label string }{{"invariants", "invariant"}, {"postconditions", "postcondition"}} {
		items, _ := env.Value(kv.key).([]any)
		for _, it := range items {
			o, ok := it.(*pyjson.Object)
			if !ok {
				continue
			}
			if enforce, _ := o.Value("enforce").(bool); !enforce || o.Value("expr") != nil {
				continue
			}
			typ, _ := o.Value("type").(string)
			if verify.OpaqueUnrecognizedType(kv.label, typ) {
				out = append(out, typ)
			}
		}
	}
	return out
}

func (e *Env) wardenProposals(args []string) error {
	opts := []opt{rootOpt, {names: []string{"--json"}, help: "Emit proposals as a JSON array."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("warden proposals", err)
	}
	if p.help {
		return e.cmdHelp("warden proposals", "", "List recorded envelope-edit proposals. A proposal is a file; nobody applies it automatically.", opts)
	}
	d := wardenroot.Join(e.root(p), "proposals")
	var props []any
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		entries, err := os.ReadDir(d)
		if err != nil {
			return e.fail("warden proposals", err)
		}
		var names []string
		for _, en := range entries {
			if !utf8.ValidString(en.Name()) {
				return e.refuse("warden proposals", wardenroot.ErrName)
			}
			if strings.HasSuffix(en.Name(), ".json") {
				names = append(names, en.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			raw, err := os.ReadFile(wardenroot.Join(d, n))
			if err != nil || !utf8.Valid(raw) {
				continue
			}
			v, err := pyjson.Loads(string(raw))
			if errors.Is(err, pyjson.ErrUnsupported) {
				return e.refuse("warden proposals", err)
			}
			if o, isObj := v.(*pyjson.Object); err == nil && isObj {
				props = append(props, o)
			}
		}
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(orEmpty(props), 2, true))
		return nil
	}
	if len(props) == 0 {
		e.out("(no pending proposals)\n")
		return nil
	}
	for _, x := range props {
		o := x.(*pyjson.Object)
		var f [4]string
		for i, k := range []string{"id", "kind", "scope", "expiresAt"} {
			v, present := o.Get(k)
			if !present {
				f[i] = "?"
				continue
			}
			s, ok := pyStr(v)
			if !ok {
				return e.refuse("warden proposals", errors.New("a proposal holds a value this binary does not print yet"))
			}
			f[i] = s
		}
		e.out("%s  [%s]  scope=%s  expires=%s\n", f[0], f[1], f[2], f[3])
	}
	return nil
}

// yamlToJSON turns a parsed YAML node into the values json.loads would
// give for the same data. A mapping with a key that is not a string is
// refused: pydantic's handling of it is not modeled.
func yamlToJSON(v config.Value) (any, error) {
	switch v.Kind {
	case config.Null:
		return nil, nil
	case config.Bool:
		return v.B, nil
	case config.Int:
		return pyjson.Loads(v.Text)
	case config.Float:
		f, err := strconv.ParseFloat(v.Text, 64)
		if err != nil {
			return nil, err
		}
		return pyjson.Float(f), nil
	case config.Str:
		return v.Text, nil
	case config.Other:
		return nil, errors.New("the envelope holds a date")
	case config.Seq:
		out := make([]any, len(v.Items))
		for i, it := range v.Items {
			c, err := yamlToJSON(it)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	if v.NonStrKeys > 0 {
		return nil, errors.New("the envelope has a mapping key that is not a string")
	}
	o := pyjson.NewObject()
	for _, k := range v.Keys {
		c, err := yamlToJSON(v.Map[k])
		if err != nil {
			return nil, err
		}
		o.Set(k, c)
	}
	return o, nil
}

// writeRefused is the *statefile.WriteRefused in err, or nil: a writer was
// refused a path the warden would refuse to read (GR-R-9).
func writeRefused(err error) *statefile.WriteRefused {
	var wr *statefile.WriteRefused
	if errors.As(err, &wr) {
		return wr
	}
	return nil
}

// noteFile says when a write went through a symlink to another file.
func (e *Env) noteFile(path, followed string) {
	if followed != "" {
		e.errf("note: %s is a symlink; wrote %s.\n", path, followed)
	}
}

func orEmpty(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

// pyStr is str(v) for the JSON values whose str is plain.
func pyStr(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "None", true
	case bool:
		if x {
			return "True", true
		}
		return "False", true
	case pyjson.Int:
		return x.Text, true
	case string:
		return x, true
	}
	return "", false
}

// provedChild is cli._proved_child: the child as it registers once the
// edge from parent is proved, or a one-line refusal and exit 1.
func (e *Env) provedChild(env *pyjson.Object, session, parent, root string) (*pyjson.Object, error) {
	refuse := func(why string) (*pyjson.Object, error) {
		e.errf("not registered: %s\n", why)
		return nil, exit(1)
	}
	if parent == "" {
		return refuse("--parent needs a session id")
	}
	if session == "" {
		return refuse("--parent needs --session; the starter names the child's session")
	}
	if !tree.ValidSession(session) {
		return refuse("the child's session id " + tree.Q(session) + " is not one the tree takes")
	}
	if st, err := os.Stat(wardenroot.Join(wardenroot.EnvelopesDir(root), wardenroot.SafeSessionID(session)+".json")); err == nil && st != nil {
		return refuse("session " + session + " is registered already")
	}
	penv := tree.LoadRegistered(parent, root)
	res := tree.EdgeOK(penv, env, tree.DefaultTimeoutMs)
	if !res.Holds {
		return refuse("the edge from " + parent + " to " + session + " is refused: " + strings.Join(res.Reasons, "; "))
	}
	return tree.WithKey(res.Child, "parent_envelope", penv.Value("id")), nil
}

// renameHints is cli._rename_hints: lines for state that still carries a name
// from before the rename, the old data directory and each old config key.
// `daisugi install` moves them.
func (e *Env) renameHints() []string {
	data := e.dataHome()
	var out []string
	old, isDir := data+"/gate", false // names-ok: old dir
	if st, err := os.Stat(old); err == nil && st.IsDir() {
		isDir = true
	}
	if _, err := os.Stat(data + "/warden"); isDir && err != nil {
		out = append(out, fmt.Sprintf("found %s from before the rename: run daisugi install to move it", old))
	}
	for _, k := range config.StaleKeys(data + "/config.yaml") {
		out = append(out, fmt.Sprintf("config: unknown key %s (now %s)", k[0], k[1]))
	}
	return out
}

// lintMCPRules is cli._lint_mcp_rules: notes go to stderr; a refused rule
// prints one line each and registers nothing.
func (e *Env) lintMCPRules(env *pyjson.Object, root string) bool {
	perms, _ := env.Value("permissions").(*pyjson.Object)
	if perms == nil {
		return false
	}
	rules, _ := perms.Value("mcp_rules").([]any)
	if len(rules) == 0 {
		return false
	}
	refusals, notes := wardenroot.LintRules(rules, wardenroot.LoadSchemas(root, func(s string) { e.errf("%s\n", s) }), perms)
	for _, n := range notes {
		e.errf("%s\n", n)
	}
	for _, r := range refusals {
		e.errf("not registered: %s\n", r)
	}
	return len(refusals) > 0
}

// wardenSchemas is `daisugi warden schemas import|list`.
func (e *Env) wardenSchemas(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("Usage: daisugi warden schemas [OPTIONS] COMMAND [ARGS]...\n\n  The MCP tool schemas on file, which `warden register` checks mcp_rules\n  against.\n\nCommands:\n  import  Store a tools/list result as the schema of one MCP server.\n  list    Print the schemas on file.\n")
		return nil
	}
	opts := []opt{rootOpt}
	switch args[0] {
	case "import":
		p, err := parseArgs(args[1:], opts, 2)
		if err != nil {
			return e.usage("warden schemas import", err)
		}
		if p.help {
			return e.cmdHelp("warden schemas import", " SERVER FILE", "Store a tools/list result as the schema of one MCP server.", opts)
		}
		if len(p.args) < 2 {
			name := "SERVER"
			if len(p.args) == 1 {
				name = "FILE"
			}
			return e.usage("warden schemas import", &usageError{"Missing argument '" + name + "'."})
		}
		path, count, err := wardenroot.ImportSchema(e.root(p), p.args[0], p.args[1])
		if err != nil {
			var se *wardenroot.SchemaError
			if errors.As(err, &se) || writeRefused(err) != nil {
				e.errf("daisugi warden schemas import: %s\n", err.Error())
				return exit(2)
			}
			return e.fail("warden schemas import", err)
		}
		e.out("stored the schema of %s (%d tools) at %s\n", p.args[0], count, path)
		return nil
	case "list":
		p, err := parseArgs(args[1:], opts, 0)
		if err != nil {
			return e.usage("warden schemas list", err)
		}
		if p.help {
			return e.cmdHelp("warden schemas list", "", "Print the schemas on file.", opts)
		}
		root := e.root(p)
		schemas := wardenroot.LoadSchemas(root, func(s string) { e.errf("%s\n", s) })
		if len(schemas) == 0 {
			e.out("no schemas on file; import one with: daisugi warden schemas import <server> <tools-list.json>\n")
			return nil
		}
		for _, s := range schemas {
			e.out("%s  %d tools  %s\n", s.Server, len(s.Obj.Value("tools").([]any)), wardenroot.Join(wardenroot.Join(root, "schemas"), s.Server+".json"))
		}
		return nil
	}
	e.errf("Usage: daisugi warden schemas [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi warden schemas --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}
