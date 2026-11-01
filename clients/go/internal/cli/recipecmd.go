package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/recipes"
	"daisugi-verify/internal/switchyard"
	"daisugi-verify/internal/wardenroot"
)

const recipeHelp = `Usage: daisugi recipe [OPTIONS] COMMAND [ARGS]...

  Recipes: named setups that run through install. List, show, apply, revert.

Commands:
  apply   Show what a recipe would write; with --yes, write it.
  list    List the recipes.
  revert  Undo a recipe from its receipt.
  show    Show a recipe: its slots, params and steps.
`

func (e *Env) recipeCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", recipeHelp)
		return nil
	}
	switch args[0] {
	case "list":
		return e.recipeList(args[1:])
	case "show":
		return e.recipeShow(args[1:])
	case "apply":
		return e.recipeApply(args[1:])
	case "revert":
		return e.recipeRevert(args[1:])
	}
	e.errf("Usage: daisugi recipe [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi recipe --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

var recipeJSONOpt = opt{names: []string{"--json"}, help: "Machine-readable JSON output."}

// recipeOrExit is _recipe_or_exit.
func (e *Env) recipeOrExit(name string) (*recipes.Recipe, error) {
	names := recipes.Names()
	for _, n := range names {
		if n == name {
			r, err := recipes.Load(name)
			if err != nil {
				return nil, e.fail("recipe", err)
			}
			return r, nil
		}
	}
	e.errf("unknown recipe '%s'. Recipes: %s\n", name, strings.Join(names, ", "))
	return nil, exit(2)
}

func (e *Env) recipeList(args []string) error {
	const cmd = "recipe list"
	opts := []opt{recipeJSONOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "List the recipes.", opts)
	}
	var rows []any
	for _, n := range recipes.Names() {
		r, err := recipes.Load(n)
		if err != nil {
			return e.fail(cmd, err)
		}
		if p.flag("--json") {
			rows = append(rows, pyjson.NewObject().Set("name", r.Name()).Set("title", r.Title()).Set("doc", r.Doc()))
		} else {
			e.out("%-26s%s\n", r.Name(), r.Title())
		}
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.Dumps(rows, true))
	}
	return nil
}

func (e *Env) recipeShow(args []string) error {
	const cmd = "recipe show"
	opts := []opt{recipeJSONOpt}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " NAME", "Show a recipe: its slots, params and steps.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "NAME", "NAME")
	}
	r, err := e.recipeOrExit(p.args[0])
	if err != nil {
		return err
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(r.Obj, 2, true))
		return nil
	}
	e.out("%s\n", r.RenderShow())
	return nil
}

// recipeSets is _parse_sets: the --set keys in order, and their values.
func (e *Env) recipeSets(p *parsed) ([]string, map[string]string, error) {
	var order []string
	vals := map[string]string{}
	for _, item := range p.vals["--set"] {
		key, value, found := strings.Cut(item, "=")
		if !found || key == "" {
			e.errf("--set needs KEY=VALUE, not '%s'\n", item)
			return nil, nil, exit(2)
		}
		if _, seen := vals[key]; !seen {
			order = append(order, key)
		}
		vals[key] = value
	}
	return order, vals, nil
}

// stepPlan is one entry of recipes.plan_apply.
type stepPlan struct {
	index   int
	kind    string
	summary string
	writes  []string
	state   string
	notes   []string
}

func (s *stepPlan) object() *pyjson.Object {
	writes := make([]any, len(s.writes))
	for i, w := range s.writes {
		writes[i] = w
	}
	notes := make([]any, len(s.notes))
	for i, n := range s.notes {
		notes[i] = n
	}
	return pyjson.NewObject().Set("index", pyjson.Int{Text: strconv.Itoa(s.index)}).Set("kind", s.kind).
		Set("summary", s.summary).Set("writes", writes).Set("state", s.state).Set("notes", notes)
}

func strList(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func objStr(o *pyjson.Object, k string) string {
	s, _ := o.Value(k).(string)
	return s
}

func (e *Env) recipeConfigPath() string { return wardenroot.Join(e.dataHome(), "config.yaml") }

func (e *Env) recipeEnvelopePath() string {
	return wardenroot.Join(e.dataHome(), "warden/envelopes/default.json")
}

// recipeRuntimes is recipes._runtimes.
func (e *Env) recipeRuntimes(step *pyjson.Object) ([]install.Runtime, error) {
	if names := strList(step.Value("runtime")); len(names) > 0 {
		return install.Select(names)
	}
	return install.Detect(e.home, install.LookPath(e.env)), nil
}

// recipeOpts is the install options a step names.
func (e *Env) recipeOpts(step *pyjson.Object) (install.Opts, error) {
	only, err := install.ComponentLayers(strList(step.Value("only")))
	if err != nil {
		return install.Opts{}, err
	}
	url := objStr(step, "base_url")
	if url == "" {
		url = install.DefaultBaseURL
	}
	return install.Opts{Router: only[install.LayerBaseURL], Enforce: pyjson.Truthy(step.Value("enforce")),
		URL: url, Report: objStr(step, "report"), Only: only}, nil
}

// cfgField is getattr(cfg, key): the value, and whether it is set.
func cfgField(cfg config.Config, key string) (string, bool) {
	ptr := func(p *string) (string, bool) {
		if p == nil {
			return "", false
		}
		return *p, true
	}
	switch key {
	case "routing_mode":
		return cfg.RoutingMode, true
	case "routing_model":
		return ptr(cfg.RoutingModel)
	case "router_local_model":
		return ptr(cfg.RouterLocalModel)
	case "switchyard_efficient_model":
		return ptr(cfg.SwitchyardEfficientModel)
	case "switchyard_capable_model":
		return cfg.SwitchyardCapableModel, true
	}
	return ptr(cfg.SwitchyardAPIKeyEnv)
}

type cfgChange struct {
	key, old, new string
	oldSet        bool
}

func shown(v string, set bool) string {
	if !set {
		return "(unset)"
	}
	return v
}

// recipeConfigChanges is recipes._config_changes, with _check_config.
func (e *Env) recipeConfigChanges(step *pyjson.Object) ([]cfgChange, config.Config, error) {
	cfg, err := config.Load(e.recipeConfigPath())
	if err != nil {
		return nil, cfg, fmt.Errorf("%s is not a config this binary reads: %v", e.recipeConfigPath(), err)
	}
	sets, _ := step.Value("set").(*pyjson.Object)
	update := map[string]string{}
	for _, k := range sets.Keys() {
		update[k] = objStr(sets, k)
	}
	mode := cfg.RoutingMode
	if v, has := update["routing_mode"]; has {
		mode = v
		found := false
		for _, m := range recipes.RoutingModes {
			if m == v {
				found = true
			}
		}
		if !found {
			return nil, cfg, fmt.Errorf("unknown routing_mode '%s'. Choose one of: %s", v, strings.Join(recipes.RoutingModes, ", "))
		}
	}
	if mode == "switchyard" {
		sc := syConfig(cfg)
		if v, has := update["switchyard_efficient_model"]; has {
			sc.EfficientModel = &v
		}
		if v, has := update["switchyard_capable_model"]; has {
			sc.CapableModel = v
		}
		if v, has := update["switchyard_api_key_env"]; has {
			sc.APIKeyEnv = &v
		}
		if sc.EfficientModel == nil || *sc.EfficientModel == "" {
			return nil, cfg, errors.New("routing_mode switchyard needs switchyard_efficient_model: a local model id, or a claude-* id")
		}
		if _, err := switchyard.Render(switchyard.TargetsFromConfig(sc), cfg.SwitchyardRouteID,
			func(string) bool { return true }); err != nil {
			return nil, cfg, fmt.Errorf("cannot build the Switchyard route: %v", err)
		}
	}
	var changes []cfgChange
	for _, k := range sets.Keys() {
		old, set := cfgField(cfg, k)
		if !set || old != update[k] {
			changes = append(changes, cfgChange{k, old, update[k], set})
		}
	}
	return changes, cfg, nil
}

func addUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// planRecipe is recipes.plan_apply.
func (e *Env) planRecipe(r *recipes.Recipe, params *recipes.Params) ([]*stepPlan, error) {
	var plan []*stepPlan
	for i, raw := range r.Steps() {
		idx := i + 1
		if w := objStr(raw, "when"); w != "" && !params.Has(w) {
			plan = append(plan, &stepPlan{idx, objStr(raw, "kind"), recipes.StepSummary(raw), nil, "skip",
				[]string{"skipped (no --set " + w + ")"}})
			continue
		}
		filled, err := r.Fill(raw, params, idx)
		if err != nil {
			return nil, err
		}
		step := filled.(*pyjson.Object)
		kind := objStr(step, "kind")
		entry := &stepPlan{index: idx, kind: kind, summary: recipes.StepSummary(step), state: "run"}
		switch kind {
		case "install":
			runtimes, err := e.recipeRuntimes(step)
			if err != nil {
				return nil, err
			}
			o, err := e.recipeOpts(step)
			if err != nil {
				return nil, err
			}
			if o.Enforce {
				envs, _ := wardenroot.Envelopes(wardenroot.Join(e.dataHome(), "warden"))
				if len(envs) == 0 {
					return nil, fmt.Errorf("step %d: enforce needs a registered envelope", idx)
				}
			}
			if len(runtimes) == 0 {
				entry.state = "kept"
				entry.notes = []string{"no supported agent runtime detected"}
				break
			}
			for _, rt := range runtimes {
				for _, s := range install.PlanAllSteps(rt, e.home, o) {
					if s.Supported && s.Target != "" {
						entry.writes = addUnique(entry.writes, s.Target)
					}
				}
			}
		case "harness":
			entry.writes = []string{install.Target(objStr(step, "name"), e.home, e.env)}
		case "config":
			changes, _, err := e.recipeConfigChanges(step)
			if err != nil {
				return nil, err
			}
			if len(changes) == 0 {
				entry.state = "kept"
			} else {
				entry.writes = []string{e.recipeConfigPath()}
				for _, c := range changes {
					entry.notes = append(entry.notes, fmt.Sprintf("%s: %s -> %s", c.key, shown(c.old, c.oldSet), c.new))
				}
			}
		case "warden-init":
			if _, err := os.Stat(e.recipeEnvelopePath()); err == nil {
				entry.state = "kept"
			} else {
				entry.writes = []string{e.recipeEnvelopePath()}
			}
		}
		plan = append(plan, entry)
	}
	return plan, nil
}

// renderPlan is recipes._plan_lines.
func renderPlan(total int, plan []*stepPlan, verb string) []string {
	var lines []string
	for _, p := range plan {
		lines = append(lines, fmt.Sprintf("[%d/%d] %s  %s", p.index, total, p.kind, p.summary))
		switch p.state {
		case "skip":
			lines = append(lines, "  "+p.notes[0])
		case "kept":
			if len(p.notes) > 0 {
				lines = append(lines, "  "+p.notes[0])
			} else {
				lines = append(lines, "  kept")
			}
		default:
			for _, w := range p.writes {
				lines = append(lines, "  "+verb+" "+w)
			}
			if p.kind == "config" {
				for _, n := range p.notes {
					lines = append(lines, "    "+n)
				}
			}
		}
	}
	return lines
}

func (e *Env) recipeReceiptPath(name string) string {
	return wardenroot.Join(e.dataHome(), "recipes/"+name+".json")
}

func (e *Env) readReceipt(name string) *pyjson.Object {
	raw, err := os.ReadFile(e.recipeReceiptPath(name))
	if err != nil {
		return nil
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// runRecipeStep is recipes._run_step: the paths written, the extra lines,
// and the undo (nil for none).
func (e *Env) runRecipeStep(step *pyjson.Object, idx int) (written, lines []string, undo *pyjson.Object, err error) {
	switch objStr(step, "kind") {
	case "note":
		return nil, nil, nil, nil
	case "install":
		runtimes, err := e.recipeRuntimes(step)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(runtimes) == 0 {
			return nil, []string{"no supported agent runtime detected"}, nil, nil
		}
		o, err := e.recipeOpts(step)
		if err != nil {
			return nil, nil, nil, err
		}
		self, err := install.Self()
		if err != nil {
			return nil, nil, nil, err
		}
		self, _ = install.HookPath(self, e.env, e.miseWhich)
		root := wardenroot.Join(e.dataHome(), "warden")
		// What each runtime lacks now: only that is this recipe's to undo.
		layers, err := install.ComponentLayers(strList(step.Value("only")))
		if err != nil {
			return nil, nil, nil, err
		}
		lacking := pyjson.NewObject()
		for _, rt := range runtimes {
			absent := map[string]bool{}
			for layer := range layers {
				if install.Wires(rt, layer) && !install.LayerPresent(rt, e.home, layer) {
					absent[layer] = true
				}
			}
			if len(absent) > 0 {
				names := install.ComponentNames(absent)
				list := make([]any, len(names))
				for i, n := range names {
					list[i] = n
				}
				lacking.Set(rt.Key, list)
			}
		}
		var failures []string
		for _, rt := range runtimes {
			ch, err := install.PlanInstall(rt, e.home, self, root, o)
			if err != nil {
				return nil, nil, nil, err
			}
			if _, err := install.ApplyChange(ch); err != nil {
				return nil, nil, nil, err
			}
			if ch.Failed {
				failures = append(failures, ch.Runtime.Name+": "+ch.Why)
			} else {
				for _, w := range ch.Written() {
					written = addUnique(written, w)
				}
			}
		}
		if len(failures) > 0 {
			return nil, nil, nil, errors.New(strings.Join(failures, "; "))
		}
		if len(written) > 0 && lacking.Len() > 0 {
			undo = pyjson.NewObject().Set("uninstall", lacking)
		}
		return written, nil, undo, nil
	case "harness":
		name := objStr(step, "name")
		_, statErr := os.Stat(install.Target(name, e.home, e.env))
		paths, err := install.InstallHarness(name, e.home, e.env)
		if err != nil {
			return nil, nil, nil, err
		}
		if statErr == nil { // the file was there before: not this recipe's to remove
			return paths, nil, nil, nil
		}
		return paths, nil, pyjson.NewObject().Set("harness", name), nil
	case "config":
		changes, cfg, err := e.recipeConfigChanges(step)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(changes) == 0 {
			return nil, nil, nil, nil
		}
		path := e.recipeConfigPath()
		updates := map[string]any{}
		restore := pyjson.NewObject()
		for _, c := range changes {
			updates[c.key] = c.new
			if c.oldSet {
				restore.Set(c.key, c.old)
			} else {
				restore.Set(c.key, nil)
			}
			lines = append(lines, fmt.Sprintf("%s: %s -> %s", c.key, shown(c.old, c.oldSet), c.new))
		}
		if err := config.Save(path, e.home, updates); err != nil {
			return nil, nil, nil, err
		}
		written = []string{path}
		newCfg, err := config.Load(path)
		if err != nil {
			return nil, nil, nil, err
		}
		if newCfg.RoutingMode == "switchyard" {
			text, err := switchyard.Render(switchyard.TargetsFromConfig(syConfig(newCfg)), cfg.SwitchyardRouteID,
				func(string) bool { return true })
			if err != nil {
				return nil, nil, nil, err
			}
			tomlPath, err := switchyard.WriteConfig(e.dataHome(), text, "switchyard.toml")
			if err != nil {
				return nil, nil, nil, err
			}
			written = append(written, tomlPath)
		}
		return written, lines, pyjson.NewObject().Set("restore", restore), nil
	}
	// warden-init
	target := e.recipeEnvelopePath()
	if _, err := os.Stat(target); err == nil {
		return nil, nil, nil, nil
	}
	cfg, err := config.Load(e.recipeConfigPath())
	if err != nil {
		return nil, nil, nil, err
	}
	ws := e.expandUser(objStr(step, "workspace"))
	if ws, err = wardenroot.Resolve(ws); err != nil {
		return nil, nil, nil, err
	}
	env, err := wardenroot.StarterEnvelope(ws, cfg.ShellAllowDecomposition)
	if err != nil {
		return nil, nil, nil, err
	}
	path, err := wardenroot.Register(env, "", wardenroot.Join(e.dataHome(), "warden"))
	if err != nil {
		return nil, nil, nil, err
	}
	return []string{path}, nil, pyjson.NewObject().Set("remove_envelope", "default"), nil
}

func (e *Env) recipeApply(args []string) error {
	const cmd = "recipe apply"
	opts := []opt{
		{names: []string{"--set"}, value: true, multiple: true, metavar: "TEXT", help: "A recipe param, KEY=VALUE. Repeatable."},
		{names: []string{"--yes", "-y"}, help: "Write the changes. Without it: a dry run."},
		recipeJSONOpt,
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " NAME", "Show what a recipe would write; with --yes, write it.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "NAME", "NAME")
	}
	name := p.args[0]
	r, err := e.recipeOrExit(name)
	if err != nil {
		return err
	}
	order, given, err := e.recipeSets(p)
	if err != nil {
		return err
	}
	params, err := r.ResolveParams(order, given)
	if err != nil {
		e.errf("%v\n", err)
		return exit(2)
	}
	plan, err := e.planRecipe(r, params)
	if err != nil {
		e.errf("%v\n", err)
		return exit(2)
	}
	total := len(r.Steps())
	asJSON := p.flag("--json")
	if !p.flag("--yes") {
		if asJSON {
			steps := make([]any, len(plan))
			for i, s := range plan {
				steps[i] = s.object()
			}
			e.out("%s\n", pyjson.DumpsIndent(pyjson.NewObject().Set("recipe", name).Set("dry_run", true).Set("steps", steps), 2, true))
			return nil
		}
		for _, l := range renderPlan(total, plan, "would write") {
			e.out("%s\n", l)
		}
		e.out("Dry run. Add --yes to apply.\n")
		return nil
	}
	// The names of the warden and the router changed: move the owner's own
	// state once, before anything new is written (as `daisugi install` does).
	var moveOut io.Writer = e.Stdout
	if asJSON {
		moveOut = e.Stderr
	}
	if _, err := install.MoveOldState(e.dataHome(), e.home, e.env, time.Now(), moveOut, false); err != nil {
		var refused *install.MoveRefused
		if errors.As(err, &refused) {
			return exit(2)
		}
		return e.fail(cmd, err)
	}
	old := e.readReceipt(name)
	oldUndo := map[string]any{}
	if old != nil {
		if list, ok := old.Value("steps").([]any); ok {
			for _, s := range list {
				if so, ok := s.(*pyjson.Object); ok {
					if ix, ok := so.Value("index").(pyjson.Int); ok {
						oldUndo[ix.Text] = so.Value("undo")
					}
				}
			}
		}
	}
	say := func(format string, a ...any) {
		if !asJSON {
			e.out(format, a...)
		}
	}
	var done []any
	completed := 0
	var failed error
	failedAt := 0
	for i, raw := range r.Steps() {
		idx := i + 1
		entry := plan[i]
		say("[%d/%d] %s  %s\n", idx, total, objStr(raw, "kind"), entry.summary)
		if entry.state == "skip" {
			say("  %s\n", entry.notes[0])
			completed = idx
			if prev := oldUndo[strconv.Itoa(idx)]; prev != nil { // the earlier run's undo stays
				done = append(done, pyjson.NewObject().Set("index", pyjson.Int{Text: strconv.Itoa(idx)}).
					Set("kind", objStr(raw, "kind")).Set("undo", prev))
			}
			continue
		}
		filled, _ := r.Fill(raw, params, idx)
		step := filled.(*pyjson.Object)
		written, lines, undo, err := e.runRecipeStep(step, idx)
		if err != nil {
			failed, failedAt = err, idx
			break
		}
		for _, w := range written {
			say("  wrote %s\n", w)
		}
		for _, l := range lines {
			if objStr(step, "kind") == "config" {
				say("    %s\n", l)
			} else {
				say("  %s\n", l)
			}
		}
		if len(written) == 0 && len(lines) == 0 && objStr(step, "kind") != "note" {
			say("  kept\n")
		}
		var undoV any = mergeUndo(undo, oldUndo[strconv.Itoa(idx)])
		done = append(done, pyjson.NewObject().Set("index", pyjson.Int{Text: strconv.Itoa(idx)}).
			Set("kind", objStr(raw, "kind")).Set("undo", undoV))
		completed = idx
	}
	if failed != nil && old != nil {
		// Steps this run did not reach keep the undo of the run that did them.
		have := map[string]bool{}
		for _, d := range done {
			have[d.(*pyjson.Object).Value("index").(pyjson.Int).Text] = true
		}
		if list, ok := old.Value("steps").([]any); ok {
			for _, so := range list {
				if o, ok := so.(*pyjson.Object); ok {
					if ix, ok := o.Value("index").(pyjson.Int); ok && !have[ix.Text] {
						done = append(done, o)
					}
				}
			}
		}
		sort.SliceStable(done, func(i, j int) bool {
			a, _ := strconv.Atoi(done[i].(*pyjson.Object).Value("index").(pyjson.Int).Text)
			b, _ := strconv.Atoi(done[j].(*pyjson.Object).Value("index").(pyjson.Int).Text)
			return a < b
		})
	}
	if done == nil {
		done = []any{}
	}
	receipt := pyjson.NewObject().Set("recipe", name).
		Set("applied_at", time.Now().UTC().Format("2006-01-02T15:04:05Z")).
		Set("params", params.Object()).Set("completed", pyjson.Int{Text: strconv.Itoa(completed)}).Set("steps", done)
	path := e.recipeReceiptPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return e.fail(cmd, err)
	}
	if err := os.WriteFile(path, []byte(pyjson.DumpsIndent(receipt, 2, true)+"\n"), 0o666); err != nil {
		return e.fail(cmd, err)
	}
	if failed != nil {
		e.errf("step %d failed: %v\n", failedAt, failed)
		e.errf("recipe stopped at step %d; nothing after it ran. Run: daisugi recipe revert %s\n", failedAt, name)
		return exit(1)
	}
	if asJSON {
		e.out("%s\n", pyjson.DumpsIndent(receipt, 2, true))
	}
	return nil
}

// knownUndo is recipes.unread_undo's test for one step: nil, or an object
// with a key this version reads.
func knownUndo(v any) bool {
	if v == nil {
		return true
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return false
	}
	for _, k := range []string{"uninstall", "harness", "restore", "remove_envelope"} {
		if _, has := o.Get(k); has {
			return true
		}
	}
	return false
}

// mergeUndo is recipes._merge_undo: what this run added, plus what an
// earlier run did.
func mergeUndo(n *pyjson.Object, old any) any {
	if n == nil {
		return old
	}
	oo, ok := old.(*pyjson.Object)
	if ok && oo.Value("restore") != nil && n.Value("restore") != nil {
		// The first run saw the value from before the recipe: it wins.
		oldR, _ := oo.Value("restore").(*pyjson.Object)
		newR, _ := n.Value("restore").(*pyjson.Object)
		// Python: {**new, **old}: new keys first, old values over them.
		out := pyjson.NewObject()
		for _, k := range newR.Keys() {
			if v, has := oldR.Get(k); has {
				out.Set(k, v)
			} else {
				out.Set(k, newR.Value(k))
			}
		}
		for _, k := range oldR.Keys() {
			if _, has := newR.Get(k); !has {
				out.Set(k, oldR.Value(k))
			}
		}
		return pyjson.NewObject().Set("restore", out)
	}
	if !ok || oo.Value("uninstall") == nil || n.Value("uninstall") == nil {
		return n
	}
	oldU, _ := oo.Value("uninstall").(*pyjson.Object)
	newU, _ := n.Value("uninstall").(*pyjson.Object)
	merged := map[string][]string{}
	var keys []string
	for _, k := range oldU.Keys() {
		merged[k] = strList(oldU.Value(k))
		keys = append(keys, k)
	}
	for _, k := range newU.Keys() {
		if _, ok := merged[k]; !ok {
			keys = append(keys, k)
		}
		have := map[string]bool{}
		for _, c := range merged[k] {
			have[c] = true
		}
		for _, c := range strList(newU.Value(k)) {
			if !have[c] {
				merged[k] = append(merged[k], c)
			}
		}
	}
	out := pyjson.NewObject()
	for _, k := range keys {
		set := map[string]bool{}
		for _, c := range merged[k] {
			set[c] = true
		}
		list := []any{}
		for _, c := range install.Components {
			if set[c.Name] {
				list = append(list, c.Name)
				delete(set, c.Name)
			}
		}
		for _, c := range merged[k] { // names outside the table keep their place after it
			if set[c] {
				list = append(list, c)
			}
		}
		out.Set(k, list)
	}
	return pyjson.NewObject().Set("uninstall", out)
}

// undoSummary is recipes._undo_summary.
func undoSummary(undo any) string {
	o, ok := undo.(*pyjson.Object)
	if !ok {
		return "nothing to undo"
	}
	if u, has := o.Get("uninstall"); has {
		uo, _ := u.(*pyjson.Object)
		var parts []string
		if uo != nil {
			for _, k := range uo.Keys() {
				parts = append(parts, strings.Join(strList(uo.Value(k)), ",")+" from "+k)
			}
		}
		return "uninstall " + strings.Join(parts, "; ") + " (added by this recipe)"
	}
	if h, has := o.Get("harness"); has {
		return "remove " + h.(string) + " extension"
	}
	if rs, has := o.Get("restore"); has {
		ro := rs.(*pyjson.Object)
		var parts []string
		for _, k := range ro.Keys() {
			s, set := ro.Value(k).(string)
			parts = append(parts, k+"="+shown(s, set))
		}
		return "restore " + strings.Join(parts, " ")
	}
	return "remove envelope " + objStr(o, "remove_envelope")
}

func (e *Env) recipeRevert(args []string) error {
	const cmd = "recipe revert"
	opts := []opt{{names: []string{"--yes", "-y"}, help: "Undo the changes. Without it: a dry run."}}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " NAME", "Undo a recipe from its receipt.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "NAME", "NAME")
	}
	name := p.args[0]
	receipt := e.readReceipt(name)
	if receipt == nil {
		e.errf("nothing to revert: no receipt for '%s' under %s/\n", name, wardenroot.Join(e.dataHome(), "recipes"))
		return exit(1)
	}
	r, err := recipes.Load(name)
	if err != nil {
		return e.fail(cmd, err)
	}
	total := len(r.Steps())
	steps, _ := receipt.Value("steps").([]any)
	for _, st := range steps {
		so, _ := st.(*pyjson.Object)
		if so == nil || !knownUndo(so.Value("undo")) {
			e.errf("receipt for '%s' holds a step this daisugi does not read; nothing was changed. Remove %s by hand.\n",
				name, e.recipeReceiptPath(name))
			return exit(2)
		}
	}
	if !p.flag("--yes") {
		for i := len(steps) - 1; i >= 0; i-- {
			s := steps[i].(*pyjson.Object)
			e.out("[%s/%d] %s  %s\n", s.Value("index").(pyjson.Int).Text, total, objStr(s, "kind"), undoSummary(s.Value("undo")))
		}
		e.out("Dry run. Add --yes to revert.\n")
		return nil
	}
	var left []any
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i].(*pyjson.Object)
		undo := s.Value("undo")
		e.out("[%s/%d] %s  %s\n", s.Value("index").(pyjson.Int).Text, total, objStr(s, "kind"), undoSummary(undo))
		uo, ok := undo.(*pyjson.Object)
		if !ok {
			continue
		}
		var removed, failures []string
		switch {
		case uo.Value("uninstall") != nil:
			un, _ := uo.Value("uninstall").(*pyjson.Object)
			for _, key := range un.Keys() {
				only, err := install.ComponentLayers(strList(un.Value(key)))
				if err != nil {
					return e.fail(cmd, err)
				}
				runtimes, err := install.Select([]string{key})
				if err != nil {
					failures = append(failures, key+": not a runtime this CLI knows")
					continue
				}
				gone := map[string]bool{}
				for _, rt := range runtimes {
					ch, err := install.PlanReverseAll(rt, e.home, gone, only)
					if err != nil {
						return e.refuse(cmd, err)
					}
					if ch.Failed {
						failures = append(failures, ch.Runtime.Name+": "+ch.Why)
						continue
					}
					if _, err := install.ApplyChange(ch); err != nil {
						return e.fail(cmd, err)
					}
					removed = append(removed, ch.Written()...)
				}
			}
		case uo.Value("harness") != nil:
			paths, err := install.UninstallHarness(objStr(uo, "harness"), e.home, e.env)
			if err != nil {
				return e.fail(cmd, err)
			}
			removed = paths
		case uo.Value("restore") != nil:
			ro := uo.Value("restore").(*pyjson.Object)
			updates := map[string]any{}
			for _, k := range ro.Keys() {
				updates[k] = ro.Value(k)
			}
			path := e.recipeConfigPath()
			if err := config.Save(path, e.home, updates); err != nil {
				return e.fail(cmd, err)
			}
			e.out("  wrote %s\n", path)
			continue
		default:
			target := wardenroot.Join(e.dataHome(), "warden/envelopes/"+objStr(uo, "remove_envelope")+".json")
			if err := os.Remove(target); err == nil {
				removed = []string{target}
			}
		}
		for _, p := range removed {
			e.out("  removed %s\n", p)
		}
		if len(failures) > 0 {
			for _, f := range failures {
				e.out("  not undone: %s\n", f)
			}
			left = append([]any{s}, left...)
		} else if len(removed) == 0 {
			e.out("  kept\n")
		}
	}
	if len(left) > 0 {
		receipt.Set("steps", left)
		if err := os.WriteFile(e.recipeReceiptPath(name), []byte(pyjson.DumpsIndent(receipt, 2, true)+"\n"), 0o666); err != nil {
			return e.fail(cmd, err)
		}
		e.errf("Some steps were not undone. The receipt keeps them. Fix the cause, then run: daisugi recipe revert %s\n", name)
		return exit(1)
	}
	if err := os.Remove(e.recipeReceiptPath(name)); err != nil {
		return e.fail(cmd, err)
	}
	e.out("Reverted. Receipt deleted.\n")
	return nil
}
