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
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/journal"
	"daisugi-verify/internal/pyjson"
)

var rootOpt = opt{names: []string{"--root"}, value: true, metavar: "PATH",
	help: "Gate state directory (envelopes, shadow log, disarm marker)."}
var jsonOpt = opt{names: []string{"--json"}, help: "Machine-readable JSON output."}

func (e *Env) root(p *parsed) string {
	if p.has("--root") {
		return gateroot.PathStr(p.str("--root", ""))
	}
	return gateroot.Join(e.home, ".opendaisugi/gate")
}

// cmdHelp prints a command's help from its options.
func (e *Env) cmdHelp(cmd, args, summary string, opts []opt) error {
	e.out("Usage: daisugi %s [OPTIONS]%s\n\n  %s\n\nOptions:\n", cmd, args, summary)
	for _, o := range opts {
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

func (e *Env) gateDisarm(args []string) error {
	opts := []opt{rootOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate disarm", err)
	}
	if p.help {
		return e.cmdHelp("gate disarm", "", "Kill switch: the gate allows everything until re-armed.", opts)
	}
	marker, followed, err := gateroot.Disarm(e.root(p))
	e.noteFile(marker, followed)
	if err != nil {
		return e.fail("gate disarm", err)
	}
	e.out("gate DISARMED (marker: %s) — `daisugi gate arm` to re-enable\n", marker)
	return nil
}

func (e *Env) gateArm(args []string) error {
	opts := []opt{rootOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate arm", err)
	}
	if p.help {
		return e.cmdHelp("gate arm", "", "Remove the disarm marker; the gate resumes evaluating calls.", opts)
	}
	if err := gateroot.Arm(e.root(p)); err != nil {
		return e.fail("gate arm", err)
	}
	e.out("gate armed\n")
	return nil
}

// cwd is Path.cwd(), or home when the directory is gone, as gate status
// falls back.
func (e *Env) cwd() string {
	c, err := gateroot.Getwd()
	if err != nil {
		return e.home
	}
	return c
}

func (e *Env) gateStatus(args []string) error {
	opts := []opt{rootOpt, jsonOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate status", err)
	}
	if p.help {
		return e.cmdHelp("gate status", "", "Show armed/disarmed state, the verdict mode, and the registered envelopes.", opts)
	}
	root := e.root(p)
	armed := !gateroot.IsDisarmed(root)
	eff, err := config.EffectiveHookMode(e.home, e.cwd())
	if err != nil {
		return e.refuse("gate status", fmt.Errorf("a Claude Code settings.json holds hooks this binary does not read yet"))
	}
	mode, source := eff.Mode, config.SourceLabel(eff)
	if mode == "" {
		mode, err = config.GateMode(gateroot.Join(gateroot.Parent(root), "config.yaml"))
		if err != nil {
			return e.refuse("gate status", err)
		}
		source = "config"
	}
	envs, err := gateroot.Envelopes(root)
	if err != nil {
		return e.refuse("gate status", err)
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
	if p.flag("--json") {
		o := pyjson.NewObject().Set("armed", armed).Set("mode", mode).Set("mode_source", source).
			Set("envelopes", strsAny(envs))
		e.out("%s\n", pyjson.Dumps(o, true))
		for _, g := range gone {
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
		// A gate hook in a form this CLI does not read. It may enforce.
		shown = "unknown gate hook"
	}
	e.out("gate: %s · mode: %s (%s)\n", state, shown, source)
	if len(envs) == 0 {
		e.out("no envelopes registered — enforce mode would deny everything\n")
	}
	for _, name := range envs {
		e.out("  envelope: %s\n", name)
	}
	for _, g := range gone {
		e.errf("%s\n", g)
	}
	return nil
}

func strsAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (e *Env) gateReport(args []string) error {
	opts := []opt{{names: []string{"--session"}, value: true, metavar: "TEXT", help: "One session's log only."}, rootOpt,
		{names: []string{"--json"}, help: "Emit the full report as JSON."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate report", err)
	}
	if p.help {
		return e.cmdHelp("gate report", "", "Summarize the shadow log: what an enforcing gate would have denied.", opts)
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
		return e.fail("gate report", err)
	}
	return e.refuse("gate report", err)
}

func (e *Env) gateSettings(args []string) error {
	opts := []opt{
		{names: []string{"--enforce"}, help: "Emit enforce-mode settings (default is shadow: observation only)."},
		rootOpt,
		{names: []string{"--format"}, value: true, metavar: "TEXT", help: "Host contract (default claude)."},
		{names: []string{"--session"}, value: true, metavar: "TEXT",
			help: "Pin the gate to this registered session's envelope."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate settings", err)
	}
	if p.help {
		return e.cmdHelp("gate settings", "", "Print the Claude Code hooks-settings JSON that wires in the gate. "+
			"Usage: claude --settings \"$(daisugi gate settings)\"", opts)
	}
	self, err := install.Self()
	if err != nil {
		return e.fail("gate settings", err)
	}
	o := install.HookOptions{Mode: "shadow", Root: e.root(p), Format: p.str("--format", "claude")}
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
	cfg, err := config.Load(gateroot.Join(gateroot.Parent(root), "config.yaml"))
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

func (e *Env) gateInit(args []string) error {
	opts := []opt{
		{names: []string{"--workspace"}, value: true, metavar: "PATH", help: "The directory the session may read/write (default: cwd)."},
		{names: []string{"--session"}, value: true, metavar: "TEXT", help: "Bind the envelope to this session id."},
		rootOpt,
		{names: []string{"--force"}, help: "Overwrite an existing envelope."},
		decomposeOpt,
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate init", err)
	}
	if p.help {
		return e.cmdHelp("gate init", "", "Generate and register a reviewable starter envelope for this session.", opts)
	}
	root := e.root(p)
	var decompose bool
	if p.flagSet("--allow-shell-decomposition") {
		decompose = p.flag("--allow-shell-decomposition")
	} else if decompose, err = e.decomposeDefault(root, "gate init"); err != nil {
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
		file = gateroot.SafeSessionID(session)
	}
	target := gateroot.Join(gateroot.EnvelopesDir(root), file+".json")
	if _, err := os.Stat(target); err == nil && !p.flag("--force") {
		e.errf("an envelope for '%s' is already registered at %s; pass --force to overwrite\n", name, target)
		return exit(1)
	}
	var ws string
	if p.has("--workspace") {
		ws = p.str("--workspace", "")
	} else if ws, err = gateroot.Getwd(); err != nil {
		return e.fail("gate init", err)
	}
	if ws, err = gateroot.Resolve(ws); err != nil {
		return e.fail("gate init", err)
	}
	env, err := gateroot.StarterEnvelope(ws, decompose)
	if err != nil {
		return e.fail("gate init", err)
	}
	path, followed, err := gateroot.Register(env, session, root)
	e.noteFile(path, followed)
	if err != nil {
		return e.fail("gate init", err)
	}
	e.out("registered a starter envelope for %s → %s\n", ws, path)
	e.out("REVIEW it before enforcing — it is a tight default, not a finished policy.\n")
	e.out("Then launch shadow mode:\n")
	e.out("  claude --settings \"$(daisugi gate settings --root %s)\"\n", root)
	return nil
}

func (e *Env) gateRegister(args []string) error {
	opts := []opt{
		{names: []string{"--session"}, value: true, metavar: "TEXT",
			help: "Bind to one session id; omit to register the default envelope."},
		rootOpt,
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage("gate register", err)
	}
	if p.help {
		return e.cmdHelp("gate register", " ENVELOPE_PATH", "Register the envelope the gate checks this session's calls against.", opts)
	}
	if len(p.args) == 0 {
		return e.usage("gate register", &usageError{"Missing argument 'ENVELOPE_PATH'."})
	}
	raw, err := os.ReadFile(p.args[0])
	if err != nil {
		return e.fail("gate register", err)
	}
	if !utf8.Valid(raw) {
		return e.fail("gate register", fmt.Errorf("%s is not valid UTF-8", p.args[0]))
	}
	text := string(raw)
	// yaml.safe_load reads the file, JSON included, as YAML 1.1: to it
	// 1e-09 is a string, not a number.
	y, err := config.ParseYAML(text)
	if err != nil {
		return e.refuse("gate register", errors.New("the envelope uses YAML this binary does not read yet"))
	}
	v, err := yamlToJSON(y)
	if err != nil {
		return e.refuse("gate register", err)
	}
	in, isObj := v.(*pyjson.Object)
	if !isObj {
		return e.fail("gate register", errors.New("the envelope file does not hold a mapping"))
	}
	env, err := gateroot.Validate(in)
	var inv *gateroot.InvalidError
	if errors.As(err, &inv) {
		return e.fail("gate register", err)
	}
	if err != nil {
		return e.refuse("gate register", err)
	}
	session := p.str("--session", "")
	path, followed, err := gateroot.Register(env, session, e.root(p))
	e.noteFile(path, followed)
	if err != nil {
		return e.fail("gate register", err)
	}
	which := "default"
	if session != "" {
		which = "session " + session
	}
	e.out("registered %s envelope → %s\n", which, path)
	return nil
}

func (e *Env) gateProposals(args []string) error {
	opts := []opt{rootOpt, {names: []string{"--json"}, help: "Emit proposals as a JSON array."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate proposals", err)
	}
	if p.help {
		return e.cmdHelp("gate proposals", "", "List recorded envelope-edit proposals. A proposal is a file; nobody applies it automatically.", opts)
	}
	d := gateroot.Join(e.root(p), "proposals")
	var props []any
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		entries, err := os.ReadDir(d)
		if err != nil {
			return e.fail("gate proposals", err)
		}
		var names []string
		for _, en := range entries {
			if !utf8.ValidString(en.Name()) {
				return e.refuse("gate proposals", gateroot.ErrName)
			}
			if strings.HasSuffix(en.Name(), ".json") {
				names = append(names, en.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			raw, err := os.ReadFile(gateroot.Join(d, n))
			if err != nil || !utf8.Valid(raw) {
				continue
			}
			v, err := pyjson.Loads(string(raw))
			if errors.Is(err, pyjson.ErrUnsupported) {
				return e.refuse("gate proposals", err)
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
				return e.refuse("gate proposals", errors.New("a proposal holds a value this binary does not print yet"))
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
