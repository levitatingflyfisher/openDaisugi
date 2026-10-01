package install

import (
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// This file is install's four default layers (skill, MCP, capture and
// instructions) and their reverse, as install.py writes them, and the
// plan of every layer in Python's apply order.

//go:embed assets/skill assets/openclaw_plugin
var layerAssets embed.FS

// SkillName is install._SKILL_NAME.
const SkillName = "opendaisugi-checklist"

// Layer names as the plan prints them.
const (
	LayerSkill        = "skill"
	LayerMCP          = "mcp"
	LayerCapture      = "capture"
	LayerInstructions = "instructions"
)

// mdMarker is install._CLAUDE_MD_MARKER.
const mdMarker = "<!-- opendaisugi-managed -->"

// mdBlock is install._CLAUDE_MD_BLOCK.
const mdBlock = mdMarker + `
## openDaisugi — automatic pathway routing

Before planning any task with 3 or more steps, call the ` + "`find_pathway`" + `
MCP tool. If similarity ≥ 0.85, use the returned cached plan via
` + "`run_plan`" + ` instead of re-planning from scratch. When a cached pathway is
used, note it explicitly: "Using cached opendaisugi pathway (similarity
X.XX) — skipping re-plan."

If no pathway matches, proceed normally. After execution, the run is
journaled automatically and will feed distillation on the next
` + "`daisugi tend`" + ` cycle.
` + mdMarker + "\n"

// codexMCPBlock is install._CODEX_MCP_BLOCK.
const codexMCPBlock = "\n[mcp_servers.opendaisugi]\ncommand = \"daisugi\"\nargs = [\"mcp\", \"serve\"]\n"

// AssetFile is one file of an embedded tree.
type AssetFile struct{ Rel, Text string }

// SkillFiles is every file under an embedded directory, by its path below
// it, in name order.
func SkillFiles(dir string) []AssetFile {
	var out []AssetFile
	_ = fs.WalkDir(layerAssets, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := layerAssets.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, AssetFile{Rel: strings.TrimPrefix(p, dir+"/"), Text: string(b)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// SkillText is the SKILL.md install --print-skill prints.
func SkillText() string {
	b, _ := layerAssets.ReadFile("assets/skill/" + SkillName + "/SKILL.md")
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// AgentsSkillTarget is install._agents_skill_target.
func AgentsSkillTarget(home, fallback string) string {
	agents := gateroot.Join(home, ".agents/skills")
	if exists(agents) || !exists(gateroot.Join(home, fallback)) {
		return gateroot.Join(agents, SkillName)
	}
	return gateroot.Join(gateroot.Join(home, fallback), SkillName)
}

// Opts are the layers an install writes beyond the four default ones.
type Opts struct {
	Gate, Gateway, Enforce, Ask bool
	URL                         string
	// Report is the floor report the gate layer wires ("", "herdr" or
	// "coppice").
	Report string
}

// skillTarget is where a runtime's skill goes.
func skillTarget(rt Runtime, home string) string {
	switch rt.Key {
	case "claude":
		return AgentsSkillTarget(home, ".claude/skills")
	case "codex":
		return AgentsSkillTarget(home, ".codex/skills")
	case "hermes":
		return gateroot.Join(home, ".hermes/skills/opendaisugi/"+SkillName)
	}
	return gateroot.Join(home, ".openclaw/workspace/skills/"+SkillName)
}

// PlanAllSteps is Runtime.plan for the default layers and the opt-in ones:
// the supported steps in layer order, then the gap notes.
func PlanAllSteps(rt Runtime, home string, o Opts) []Step {
	j := func(rel string) string { return gateroot.Join(home, rel) }
	var steps []Step
	add := func(layer, desc, target string) {
		steps = append(steps, Step{Layer: layer, Description: desc, Target: target, Supported: true})
	}
	add(LayerSkill, "Symlink opendaisugi-checklist skill", skillTarget(rt, home))
	switch rt.Key {
	case "claude":
		add(LayerMCP, `Register MCP server "opendaisugi"`, j(".claude.json"))
		add(LayerCapture, "Add PreToolUse capture hook", j(".claude/settings.json"))
		add(LayerInstructions, "Append pathway guidance", j(".claude/CLAUDE.md"))
	case "codex":
		add(LayerMCP, "Register opendaisugi MCP server", j(".codex/config.toml"))
		add(LayerInstructions, "Append pathway guidance", j(".codex/AGENTS.md"))
	case "hermes":
		add(LayerMCP, "Register opendaisugi MCP server", j(".hermes/config.yaml"))
		add(LayerCapture, "Add pre_tool_call capture hook", j(".hermes/config.yaml"))
	case "openclaw":
		add(LayerMCP, "Register opendaisugi MCP server", j(".openclaw/openclaw.json"))
		add(LayerCapture, "Install before_tool_call capture plugin", j(".openclaw/extensions/opendaisugi"))
		add(LayerInstructions, "Append pathway guidance", j(".openclaw/workspace/AGENTS.md"))
	}
	rest := PlanSteps(rt, home, o.Gate, o.Gateway, o.Enforce, o.Ask, o.URL)
	var gaps []Step
	for _, s := range rest {
		if !s.Supported {
			gaps = append(gaps, s)
			continue
		}
		steps = append(steps, s)
		if s.Layer == LayerGate && rt.Key == "claude" {
			if o.Report == "herdr" {
				add(LayerGate, "Add Stop + Notification floor-report hooks (Herdr)", j(".claude/settings.json"))
			}
			if o.Report == "herdr" || o.Report == "coppice" {
				add(LayerGate, "Add SubagentStart + SubagentStop hooks (subagent rows)", j(".claude/settings.json"))
			}
		}
	}
	return append(steps, gaps...)
}

// priorOf is the last edit of p in the change, whose content later edits
// of p are planned on.
func priorOf(ch *Change, p string) *Edit {
	var prior *Edit
	for _, e := range ch.Edits {
		if e != nil && e.Path == p && e.Kind == "" && e.Warning == "" {
			prior = e
		}
	}
	return prior
}

// textOf is path.read_text() after the earlier edits of this run.
func textOf(p string, prior *Edit) (string, bool, error) {
	if prior != nil {
		return prior.Content, true, nil
	}
	return readText(p)
}

// PlanSkill is _link_skill as this binary does it: the skill's files,
// copied from the binary (there is no package directory to link to).
func PlanSkill(target string) *Edit {
	return &Edit{Path: target, Kind: "skill", Listed: true}
}

// PlanMCP is _patch_mcp: entry set at key path in a JSON (or JSON5) file
// unless one is there, the file left alone when it does not parse.
func PlanMCP(p string, json5 bool, keys []string, entry *pyjson.Object, what string, prior *Edit) (*Edit, error) {
	text, existed, err := textOf(p, prior)
	if err != nil {
		return nil, err
	}
	var cfg any = pyjson.NewObject()
	if existed {
		v, lerr := pyjson.Loads(text)
		if lerr != nil && json5 && !errors.Is(lerr, pyjson.ErrUnsupported) {
			v, lerr = pyjson.Loads(stripJSON5(text))
		}
		if lerr != nil {
			if errors.Is(lerr, pyjson.ErrUnsupported) {
				return nil, ErrUnsupported
			}
			return &Edit{Path: p, Warning: p + " is not valid; skipping " + what + " to avoid overwriting user state. " +
				"Fix the file and re-run `daisugi install`."}, nil
		}
		cfg = v
	}
	leaf := cfg
	for _, k := range keys {
		o, ok := leaf.(*pyjson.Object)
		if !ok {
			return nil, failf("'%s' object has no attribute 'setdefault'", pyType(leaf))
		}
		next, has := o.Get(k)
		if !has {
			next = pyjson.NewObject()
			o.Set(k, next)
		}
		leaf = next
	}
	lo, ok := leaf.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported // `in` and item assignment on another type
	}
	if _, has := lo.Get("opendaisugi"); has {
		return nil, nil
	}
	e := &Edit{Path: p, Existed: existed}
	if existed && json5 && json5Comment.MatchString(text) {
		e.PreWarning = p + " contains JSON5 comments which will not survive the rewrite (the writer emits plain " +
			"JSON). The pre-write backup at " + p + ".bak* preserves the original text — restore from it if you " +
			"need the comments back. Tracked as M7 in REVIEW_FINDINGS.md."
	}
	lo.Set("opendaisugi", entry)
	e.Content = dumpSettings(cfg)
	return e, nil
}

func mcpEntry(withType bool) *pyjson.Object {
	o := pyjson.NewObject()
	if withType {
		o.Set("type", "stdio")
	}
	return o.Set("command", "daisugi").Set("args", []any{"mcp", "serve"})
}

// settingsOf reads a Claude settings.json for a capture or report edit:
// nil and a warning edit when it does not parse.
func settingsOf(p string, prior *Edit, warning string) (*pyjson.Object, bool, *Edit, error) {
	v, st, err := jsonOf(p, prior)
	if err != nil {
		return nil, false, nil, err
	}
	if st == readBad {
		return nil, false, &Edit{Path: p, Warning: warning}, nil
	}
	existed := st != readMissing
	if !existed {
		v = pyjson.NewObject()
	}
	s, ok := v.(*pyjson.Object)
	if !ok {
		return nil, false, nil, failf("'%s' object has no attribute 'setdefault'", pyType(v))
	}
	return s, existed, nil, nil
}

// eventList is hooks.setdefault(event, []) as a list.
func eventList(s *pyjson.Object, event string) (*pyjson.Object, []any, error) {
	hv, has := s.Get("hooks")
	if !has {
		hv = pyjson.NewObject()
		s.Set("hooks", hv)
	}
	hooks, ok := hv.(*pyjson.Object)
	if !ok {
		return nil, nil, failf("'%s' object has no attribute 'setdefault'", pyType(hv))
	}
	ev, has := hooks.Get(event)
	if !has {
		ev = []any{}
		hooks.Set(event, ev)
	}
	list, ok := ev.([]any)
	if !ok {
		return nil, nil, ErrUnsupported
	}
	return hooks, list, nil
}

// commandsOf is {h["command"] for e in entries for h in e.get("hooks", [])
// if h.get("type") == "command"}.
func commandsOf(entries []any) ([]any, error) {
	hs, err := commandHooks(entries)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, h := range hs {
		c, has := h.Get("command")
		if !has {
			return nil, failf("'command'") // KeyError
		}
		switch c.(type) {
		case []any, *pyjson.Object:
			return nil, failf("unhashable type: '%s'", pyType(c))
		}
		out = append(out, c)
	}
	return out, nil
}

// PlanClaudeCapture is _patch_claude_settings.
func PlanClaudeCapture(p string, prior *Edit) (*Edit, error) {
	s, existed, warn, err := settingsOf(p, prior, p+" is not valid JSON; skipping hook registration to avoid "+
		"overwriting your Claude Code settings (permissions/env). Fix the file and re-run `daisugi install`.")
	if err != nil || warn != nil {
		return warn, err
	}
	hooks, pre, err := eventList(s, "PreToolUse")
	if err != nil {
		return nil, err
	}
	cmds, err := commandsOf(pre)
	if err != nil {
		return nil, err
	}
	changed := true
	for _, c := range cmds {
		if config.IsRecordHook(c, "") {
			changed = false
		}
	}
	if changed {
		hooks.Set("PreToolUse", append(pre, pyjson.NewObject().
			Set("matcher", "Bash|Edit|Write|Read|Glob|Grep|WebFetch|WebSearch").
			Set("hooks", []any{pyjson.NewObject().Set("type", "command").Set("command", "daisugi hook record --format claude")})))
	}
	// The v0.27.1 print-skill SessionStart hook is taken out.
	if ss, ok := hooks.Value("SessionStart").([]any); ok && len(ss) > 0 {
		found := false
		for _, e := range ss {
			eo, ok := e.(*pyjson.Object)
			if !ok {
				return nil, ErrUnsupported
			}
			for _, h := range asList(eo.Value("hooks")) {
				if ho, ok := h.(*pyjson.Object); ok && ho.Value("command") == "daisugi install --print-skill" {
					found = true
				}
			}
		}
		if found {
			var kept []any
			for _, e := range ss {
				eo := e.(*pyjson.Object)
				var left []any
				for _, h := range asList(eo.Value("hooks")) {
					ho, ok := h.(*pyjson.Object)
					if !ok {
						return nil, ErrUnsupported
					}
					if ho.Value("command") != "daisugi install --print-skill" {
						left = append(left, h)
					}
				}
				if left == nil {
					left = []any{}
				}
				eo.Set("hooks", left)
				if len(left) > 0 {
					kept = append(kept, e)
				}
			}
			if len(kept) == 0 {
				hooks.Delete("SessionStart")
			} else {
				hooks.Set("SessionStart", kept)
			}
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	return &Edit{Path: p, Existed: existed, Content: dumpSettings(s)}, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// PlanReportHooks is _patch_claude_report_hooks (herdr: Stop and
// Notification) or _patch_claude_subagent_hooks.
func PlanReportHooks(p string, prior *Edit, subagent bool) (*Edit, error) {
	what := "floor-report hook"
	if subagent {
		what = "subagent hook"
	}
	s, existed, warn, err := settingsOf(p, prior, p+" is not valid JSON; skipping "+what+" registration to avoid "+
		"overwriting your Claude Code settings. Fix the file and re-run `daisugi install`.")
	if err != nil || warn != nil {
		return warn, err
	}
	type ev struct{ name, flag string }
	evs := []ev{{"Stop", "--event stop"}, {"Notification", "--event notification"}}
	if subagent {
		evs = []ev{{"SubagentStart", "--event subagent_start"}, {"SubagentStop", "--event subagent_stop"}}
	}
	changed := false
	for _, e := range evs {
		hooks, list, err := eventList(s, e.name)
		if err != nil {
			return nil, err
		}
		hs, err := commandHooks(list)
		if err != nil {
			return nil, err
		}
		word := e.flag[len("--event "):]
		have := false
		for _, h := range hs {
			if config.IsRecordHook(h.Value("command"), word) {
				have = true
			}
		}
		if !have {
			entry := pyjson.NewObject().Set("hooks", []any{pyjson.NewObject().Set("type", "command").
				Set("command", "daisugi hook record --format claude "+e.flag)})
			hooks.Set(e.name, append(list, entry))
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	return &Edit{Path: p, Existed: existed, Content: dumpSettings(s)}, nil
}

// PlanInstructions is _patch_instructions: the managed block appended
// once. It makes the file's directory, and keeps no backup.
func PlanInstructions(p string) (*Edit, error) {
	text, _, err := readText(p)
	if err != nil {
		return nil, err
	}
	if strings.Contains(text, mdMarker) {
		return nil, nil
	}
	sep := ""
	if text != "" {
		sep = "\n\n"
	}
	return &Edit{Path: p, Kind: "md", Content: strings.TrimRight(text, "\n") + sep + mdBlock}, nil
}

// PlanCodexMCP is _patch_codex_config.
func PlanCodexMCP(p string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, err
	}
	if strings.Contains(text, "[mcp_servers.opendaisugi]") {
		return nil, nil
	}
	sep := ""
	if text != "" {
		sep = "\n"
	}
	return &Edit{Path: p, Existed: existed, Content: strings.TrimRight(text, "\n") + sep + codexMCPBlock}, nil
}

// yamlOf is yaml.safe_load(path.read_text()) or {} for a Hermes config: a
// warning edit when it does not parse.
func yamlOf(p string) (*pyjson.Object, bool, *pyjson.Object, error) {
	text, existed, err := readText(p)
	if err != nil || !existed {
		return nil, existed, nil, err
	}
	// read_text reads with universal newlines.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	v, exc, why := pyyaml.Load(text)
	if why != nil || !pyyaml.Plain(v) {
		return nil, true, nil, ErrUnsupported
	}
	if exc != nil {
		if exc.Type == "ValueError" {
			// Not a yaml.YAMLError: the oracle's except does not catch it.
			return nil, true, nil, ErrUnsupported
		}
		return nil, true, nil, errYAML
	}
	if !pyjson.Truthy(v) {
		return pyjson.NewObject(), true, nil, nil
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, true, nil, ErrUnsupported
	}
	return o, true, o, nil
}

var errYAML = errors.New("not valid YAML")

func yamlText(o *pyjson.Object) (string, error) {
	t, why := pyyaml.SafeDump(o)
	if why != nil {
		return "", ErrUnsupported
	}
	return t, nil
}

// PlanHermesConfig is _patch_hermes_config.
func PlanHermesConfig(p string) (*Edit, error) {
	cfg, existed, _, err := yamlOf(p)
	if errors.Is(err, errYAML) {
		return &Edit{Path: p, Warning: p + " is not valid YAML; skipping to avoid overwriting your Hermes config. " +
			"Fix the file and re-run `daisugi install`."}, nil
	}
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = pyjson.NewObject()
	}
	changed := false
	mv, has := cfg.Get("mcp_servers")
	if !has {
		mv = pyjson.NewObject()
		cfg.Set("mcp_servers", mv)
	}
	mcp, ok := mv.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	if _, has := mcp.Get("opendaisugi"); !has {
		mcp.Set("opendaisugi", mcpEntry(false))
		changed = true
	}
	hooks, list, err := eventList(cfg, "pre_tool_call")
	if err != nil {
		return nil, err
	}
	const cmd = "daisugi hook record --format hermes"
	have := false
	for _, h := range list {
		if ho, ok := h.(*pyjson.Object); ok && ho.Value("command") == cmd {
			have = true
		}
	}
	if !have {
		hooks.Set("pre_tool_call", append(list, pyjson.NewObject().Set("matcher", ".*").Set("command", cmd).
			Set("timeout", pyjson.Int{Text: "10"})))
		changed = true
	}
	if !changed {
		return nil, nil
	}
	text, err := yamlText(cfg)
	if err != nil {
		return nil, err
	}
	return &Edit{Path: p, Existed: existed, Content: text}, nil
}

// PlanOpenClawPlugin is _install_openclaw_plugin.
func PlanOpenClawPlugin(home string) *Edit {
	return &Edit{Path: gateroot.Join(home, ".openclaw/extensions/opendaisugi"), Kind: "plugin", Listed: true}
}

// missingDir is the error a write into a directory that is not there
// raises, as Python words it.
func missingDir(p string) error {
	if !isDir(filepath.Dir(p)) {
		return failf("[Errno 2] No such file or directory: %s", pyjson.Repr(p))
	}
	return nil
}

// PlanInstall is Runtime.apply(home, layers, ...) decided without
// writing: every edit in Python's order. A step Python raises on fails
// the runtime, with the edits before it kept (Partial).
func PlanInstall(rt Runtime, home, self, root string, o Opts) (*Change, error) {
	ch := &Change{Runtime: rt}
	j := func(rel string) string { return gateroot.Join(home, rel) }
	switch rt.Key {
	case "codex":
		ch.Mkdirs = []string{j(".codex")}
	case "hermes":
		ch.Mkdirs = []string{j(".hermes")}
	case "openclaw":
		ch.Mkdirs = []string{j(".openclaw/workspace")}
	}
	type step func() (*Edit, error)
	var steps []step
	add := func(s step) { steps = append(steps, s) }
	add(func() (*Edit, error) { return PlanSkill(skillTarget(rt, home)), nil })
	settings := j(".claude/settings.json")
	mode := "audit"
	if o.Enforce {
		mode = "enforce"
	}
	switch rt.Key {
	case "claude":
		add(func() (*Edit, error) {
			return PlanMCP(j(".claude.json"), false, []string{"mcpServers"}, mcpEntry(true), "MCP registration",
				priorOf(ch, j(".claude.json")))
		})
		add(func() (*Edit, error) {
			e, err := PlanClaudeCapture(settings, priorOf(ch, settings))
			if err == nil && e != nil && e.Warning == "" {
				err = missingDir(settings)
			}
			return e, err
		})
		add(func() (*Edit, error) { return PlanInstructions(j(".claude/CLAUDE.md")) })
		if o.Gate {
			add(func() (*Edit, error) {
				entry := HookEntry(self, HookOptions{Mode: mode, Root: root, Ask: o.Ask})
				e, err := PlanClaudeGateOn(settings, entry, priorOf(ch, settings))
				if err == nil && e != nil && e.Warning == "" {
					err = missingDir(settings)
				}
				return e, err
			})
			if o.Report == "herdr" {
				add(func() (*Edit, error) { return PlanReportHooks(settings, priorOf(ch, settings), false) })
			}
			if o.Report == "herdr" || o.Report == "coppice" {
				add(func() (*Edit, error) { return PlanReportHooks(settings, priorOf(ch, settings), true) })
			}
		}
		if o.Gateway {
			add(func() (*Edit, error) {
				e, err := PlanClaudeBaseURL(settings, o.URL, priorOf(ch, settings))
				if err == nil && e != nil && e.Warning == "" {
					err = missingDir(settings)
				}
				return e, err
			})
		}
	case "codex":
		add(func() (*Edit, error) { return PlanCodexMCP(j(".codex/config.toml")) })
		add(func() (*Edit, error) { return PlanInstructions(j(".codex/AGENTS.md")) })
		if o.Gate {
			add(func() (*Edit, error) {
				return PlanCodexGate(j(".codex/hooks.json"), HookEntry(self, HookOptions{Mode: mode, Root: root}))
			})
		}
		if o.Gateway {
			add(func() (*Edit, error) {
				return PlanCodexBaseURLOn(j(".codex/config.toml"), o.URL, priorOf(ch, j(".codex/config.toml")))
			})
		}
	case "hermes":
		add(func() (*Edit, error) { return PlanHermesConfig(j(".hermes/config.yaml")) })
	case "openclaw":
		cfg := j(".openclaw/openclaw.json")
		add(func() (*Edit, error) {
			return PlanMCP(cfg, true, []string{"mcp", "servers"}, mcpEntry(false), "MCP registration", priorOf(ch, cfg))
		})
		add(func() (*Edit, error) { return PlanInstructions(j(".openclaw/workspace/AGENTS.md")) })
		add(func() (*Edit, error) { return PlanOpenClawPlugin(home), nil })
		if o.Gateway {
			add(func() (*Edit, error) {
				return PlanMCP(cfg, true, []string{"models", "providers"},
					pyjson.NewObject().Set("baseUrl", o.URL).Set("api", "anthropic-messages"),
					"gateway provider registration", priorOf(ch, cfg))
			})
		}
	}
	for _, s := range steps {
		e, err := s()
		if isFail(err) {
			ch.Failed, ch.Why, ch.Partial = true, err.Error(), true
			return ch, nil
		}
		if err != nil {
			return nil, err
		}
		if e != nil {
			ch.Edits = append(ch.Edits, e)
		}
	}
	return ch, nil
}

// clearPath is skill_paths._clear: a symlink or file unlinked, a
// directory removed with what is in it. It is true when something went.
func clearPath(p string) (bool, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return false, nil
	}
	if st.IsDir() {
		return true, os.RemoveAll(p)
	}
	return true, os.Remove(p)
}

// sameSkill is whether target already holds this binary's skill, file for
// file, with nothing else in it.
func sameSkill(target string) bool {
	if st, err := os.Lstat(target); err != nil || !st.IsDir() {
		return false
	}
	want := map[string]string{}
	for _, f := range SkillFiles("assets/skill/" + SkillName) {
		want[f.Rel] = f.Text
	}
	n := 0
	ok := true
	_ = filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err != nil || !ok {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			ok = false
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(target, p)
		b, rerr := os.ReadFile(p)
		if rerr != nil || want[rel] != string(b) {
			ok = false
		}
		n++
		return nil
	})
	return ok && n == len(want)
}

// applyLayerEdit performs the edits only this file plans.
func applyLayerEdit(e *Edit) (string, error) {
	switch e.Kind {
	case "skill":
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o777); err != nil {
			return "", err
		}
		if sameSkill(e.Path) {
			return "", nil
		}
		if _, err := clearPath(e.Path); err != nil {
			return "", err
		}
		for _, f := range SkillFiles("assets/skill/" + SkillName) {
			p := filepath.Join(e.Path, f.Rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(f.Text), 0o666); err != nil {
				return "", err
			}
		}
		return "", nil
	case "plugin":
		if err := os.MkdirAll(e.Path, 0o777); err != nil {
			return "", err
		}
		for _, f := range SkillFiles("assets/openclaw_plugin") {
			p := filepath.Join(e.Path, f.Rel)
			if isSymlink(p) {
				if err := os.Remove(p); err != nil {
					return "", err
				}
			}
			if _, err := gateroot.WriteFile(p, f.Text); err != nil {
				return "", err
			}
		}
		return "", nil
	case "md":
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o777); err != nil {
			return "", err
		}
		return gateroot.WriteFile(e.Path, e.Content)
	case "remove":
		_, err := clearPath(e.Path)
		return "", err
	case "md-unpatch":
		if err := Backup(e.Path); err != nil {
			return "", err
		}
		return gateroot.WriteFile(e.Path, e.Content)
	}
	return "", errors.New("unknown edit kind " + e.Kind)
}

// PlanRemove is a _clear of p for the reverse, or nil when nothing is
// there.
func PlanRemove(p string) *Edit {
	if _, err := os.Lstat(p); err != nil {
		return nil
	}
	return &Edit{Path: p, Kind: "remove", Listed: true}
}

// PlanUnpatchInstructions is _unpatch_instructions.
func PlanUnpatchInstructions(p string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, ErrUnsupported
	}
	if !existed {
		return nil, nil
	}
	start := strings.Index(text, mdMarker)
	if start < 0 {
		return nil, nil
	}
	second := strings.Index(text[start+len(mdMarker):], mdMarker)
	if second < 0 {
		return nil, nil
	}
	end := start + len(mdMarker) + second + len(mdMarker)
	cleaned := strings.TrimRight(text[:start]+text[end:], "\n")
	if cleaned != "" {
		cleaned += "\n"
	}
	return &Edit{Path: p, Kind: "md-unpatch", Content: cleaned, Listed: true}, nil
}

// PlanPopMCP is _pop_json_mcp.
func PlanPopMCP(p, key string) (*Edit, error) {
	v, st, err := readJSON(p)
	if err != nil {
		if isFail(err) {
			return nil, ErrUnsupported
		}
		return nil, err
	}
	if st != readOK {
		return nil, nil
	}
	cfg, ok := v.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	mv, has := cfg.Get(key)
	if !has {
		return nil, nil
	}
	m, ok := mv.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	if _, has := m.Get("opendaisugi"); !has {
		return nil, nil
	}
	m.Delete("opendaisugi")
	if m.Len() == 0 {
		cfg.Delete(key)
	}
	return &Edit{Path: p, Existed: true, Content: dumpSettings(cfg)}, nil
}

// PlanCodexMCPRemove is the config.toml half of CodexRuntime.reverse.
func PlanCodexMCPRemove(p string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, ErrUnsupported
	}
	if !existed || !strings.Contains(text, strings.TrimSpace(codexMCPBlock)) {
		return nil, nil
	}
	cleaned := strings.TrimRight(strings.ReplaceAll(text, codexMCPBlock, ""), "\n")
	if cleaned != "" {
		cleaned += "\n"
	}
	return &Edit{Path: p, Existed: true, Content: cleaned}, nil
}

// PlanHermesReverse is the config.yaml half of HermesRuntime.reverse.
func PlanHermesReverse(p string) (*Edit, error) {
	cfg, existed, _, err := yamlOf(p)
	if !existed || errors.Is(err, errYAML) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnsupported
	}
	changed := false
	if servers, ok := cfg.Value("mcp_servers").(*pyjson.Object); ok {
		if _, has := servers.Get("opendaisugi"); has {
			servers.Delete("opendaisugi")
			if servers.Len() == 0 {
				cfg.Delete("mcp_servers")
			}
			changed = true
		}
	}
	if hooks, ok := cfg.Value("hooks").(*pyjson.Object); ok {
		if pre, ok := hooks.Value("pre_tool_call").([]any); ok {
			var kept []any
			for _, h := range pre {
				if ho, ok := h.(*pyjson.Object); ok && config.IsRecordHook(ho.Value("command"), "") {
					continue
				}
				kept = append(kept, h)
			}
			if len(kept) != len(pre) {
				changed = true
				if len(kept) > 0 {
					hooks.Set("pre_tool_call", kept)
				} else {
					hooks.Delete("pre_tool_call")
				}
				if hooks.Len() == 0 {
					cfg.Delete("hooks")
				}
			}
		}
	}
	if !changed {
		return nil, nil
	}
	text, err := yamlText(cfg)
	if err != nil {
		return nil, err
	}
	return &Edit{Path: p, Existed: true, Content: text}, nil
}

// PlanOpenClawUnconfig is the openclaw.json half of OpenClawRuntime.reverse:
// the MCP server and the gateway provider taken out in one rewrite.
func PlanOpenClawUnconfig(p string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, ErrUnsupported
	}
	if !existed {
		return nil, nil
	}
	v, lerr := pyjson.Loads(text)
	if lerr != nil {
		if v, lerr = pyjson.Loads(stripJSON5(text)); lerr != nil {
			if errors.Is(lerr, pyjson.ErrUnsupported) {
				return nil, ErrUnsupported
			}
			return nil, nil
		}
	}
	cfg, ok := v.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	changed := false
	for _, keys := range [][2]string{{"mcp", "servers"}, {"models", "providers"}} {
		top, has := cfg.Get(keys[0])
		if !has {
			continue
		}
		to, ok := top.(*pyjson.Object)
		if !ok {
			return nil, ErrUnsupported
		}
		inner, has := to.Get(keys[1])
		if !has {
			continue
		}
		io, ok := inner.(*pyjson.Object)
		if !ok {
			return nil, ErrUnsupported
		}
		if _, has := io.Get("opendaisugi"); !has {
			continue
		}
		io.Delete("opendaisugi")
		if io.Len() == 0 {
			to.Delete(keys[1])
			if to.Len() == 0 {
				cfg.Delete(keys[0])
			}
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return &Edit{Path: p, Existed: true, Content: dumpSettings(cfg)}, nil
}

// PlanReverseAll is Runtime.reverse: every managed change reversed, in
// Python's order.
func PlanReverseAll(rt Runtime, home string, gone map[string]bool) (*Change, error) {
	j := func(rel string) string { return gateroot.Join(home, rel) }
	ch := &Change{Runtime: rt}
	switch rt.Key {
	case "claude", "codex":
		hooksFile := j(".claude/settings.json")
		if rt.Key == "codex" {
			hooksFile = j(".codex/hooks.json")
		}
		if why := unknownGateHook(hooksFile); why != "" {
			ch.Failed, ch.Why = true, why
			return ch, nil
		}
	}
	var steps []func() (*Edit, error)
	add := func(s func() (*Edit, error)) { steps = append(steps, s) }
	// A path an earlier runtime's reverse removed is gone by the time
	// this one runs, as Python's runtimes reverse one after another.
	remove := func(p string) *Edit {
		if gone[p] {
			return nil
		}
		e := PlanRemove(p)
		if e != nil {
			gone[p] = true
		}
		return e
	}
	removeBoth := func(fallback string) {
		add(func() (*Edit, error) { return remove(j(".agents/skills/" + SkillName)), nil })
		add(func() (*Edit, error) { return remove(gateroot.Join(j(fallback), SkillName)), nil })
	}
	settings := j(".claude/settings.json")
	switch rt.Key {
	case "claude":
		removeBoth(".claude/skills")
		add(func() (*Edit, error) { return PlanPopMCP(j(".claude.json"), "mcpServers") })
		add(func() (*Edit, error) {
			return popHook(settings, isAnyRecordHook, []string{"PreToolUse"}, priorOf(ch, settings))
		})
		add(func() (*Edit, error) {
			return popHook(settings, config.IsGateHook, []string{"PreToolUse"}, priorOf(ch, settings))
		})
		add(func() (*Edit, error) {
			return popHook(settings, isAnyRecordHook, []string{"Stop", "Notification", "SubagentStart", "SubagentStop"},
				priorOf(ch, settings))
		})
		add(func() (*Edit, error) { return PlanPopEnvKey(settings, "ANTHROPIC_BASE_URL", priorOf(ch, settings)) })
		add(func() (*Edit, error) { return PlanUnpatchInstructions(j(".claude/CLAUDE.md")) })
	case "codex":
		removeBoth(".codex/skills")
		add(func() (*Edit, error) { return PlanCodexMCPRemove(j(".codex/config.toml")) })
		add(func() (*Edit, error) {
			return popHook(j(".codex/hooks.json"), config.IsGateHook, []string{"PreToolUse"}, nil)
		})
		add(func() (*Edit, error) {
			return PlanCodexUnpatchOn(j(".codex/config.toml"), priorOf(ch, j(".codex/config.toml")))
		})
		add(func() (*Edit, error) { return PlanUnpatchInstructions(j(".codex/AGENTS.md")) })
	case "hermes":
		add(func() (*Edit, error) { return remove(j(".hermes/skills/opendaisugi/" + SkillName)), nil })
		add(func() (*Edit, error) { return PlanHermesReverse(j(".hermes/config.yaml")) })
	case "openclaw":
		add(func() (*Edit, error) { return remove(j(".openclaw/workspace/skills/" + SkillName)), nil })
		add(func() (*Edit, error) { return PlanOpenClawUnconfig(j(".openclaw/openclaw.json")) })
		add(func() (*Edit, error) { return remove(j(".openclaw/extensions/opendaisugi")), nil })
		add(func() (*Edit, error) { return PlanUnpatchInstructions(j(".openclaw/workspace/AGENTS.md")) })
	}
	for _, s := range steps {
		e, err := s()
		if isFail(err) {
			// reverse() would raise, and uninstall would print the
			// exception's text, which this binary cannot reproduce.
			return nil, ErrUnsupported
		}
		if err != nil {
			return nil, err
		}
		if e != nil {
			ch.Edits = append(ch.Edits, e)
		}
	}
	return ch, nil
}

// popHook is PlanPopHook with a hook file that is not JSON left alone.
func popHook(p string, match func(any) bool, events []string, prior *Edit) (*Edit, error) {
	return PlanPopHook(p, match, events, prior)
}

// pyReprPath is repr(str(path)).
func pyReprPath(p string) string { return pystr.Repr(p) }
