package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"daisugi-verify/internal/hermesdb"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
	"daisugi-verify/internal/wardenroot"
)

// The pando layer (round 7): Pando R2 is there in every harness with no
// setup step. It registers `daisugi pando serve` as the MCP server "pando",
// puts the memory view into the context of a new chat, logs a Claude Code
// chat as it is said, and runs `daisugi pando follow` as a user service so
// Hermes chats and background tends keep going. It is on by default; the
// --only list leaves it out unless it names the pando layer.

// LayerPando is the pando layer's name.
const LayerPando = "pando"

// PandoMCPName is the MCP server name the layer registers.
const PandoMCPName = "pando"

// PandoUnitName is the systemd user unit's file name.
const PandoUnitName = "daisugi-pando.service"

// pandoHookMarker is what every hook command of the layer holds.
const pandoHookMarker = " pando hook"

// hasPando is whether the layer is selected: a default install, or --only
// that names it.
func (o Opts) hasPando() bool { return o.Only == nil || o.Only[LayerPando] }

func pandoMCPEntry(withType bool, self string) *pyjson.Object {
	o := pyjson.NewObject()
	if withType {
		o.Set("type", "stdio")
	}
	return o.Set("command", self).Set("args", []any{"pando", "serve"})
}

func pandoHookCommand(self string) string { return verify.ShlexQuote(self) + pandoHookMarker }

func isPandoHook(c any) bool {
	s, ok := c.(string)
	return ok && strings.HasSuffix(strings.TrimSpace(s), pandoHookMarker)
}

// pandoHookEvents are the Claude Code events the layer hooks. SessionEnd
// catches the last reply: the transcript may lag the Stop hook.
var pandoHookEvents = []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"}

// pandoHookAsync is whether an event's hook runs in the background. Only
// SessionStart prints context, so only it must be waited for. The prompt and
// reply hooks never hold up a prompt, even when a model call holds the store.
// SessionEnd is waited for too: it is the end of the chat and holds nothing
// up, and Claude Code kills a background hook at exit in -p mode, so the last
// reply would be lost. Its own timeout (10 s) is the wait.
func pandoHookAsync(ev string) bool { return ev != "SessionStart" && ev != "SessionEnd" }

func pandoHookEntry(ev, self string) *pyjson.Object {
	h := pyjson.NewObject().Set("type", "command").Set("command", pandoHookCommand(self)).Set("timeout", pyjson.Int{Text: "10"})
	if pandoHookAsync(ev) {
		h.Set("async", true)
	}
	return pyjson.NewObject().Set("hooks", []any{h})
}

// pandoHookFresh sets an existing pando hook to the shape the layer wants.
func pandoHookFresh(ev, self string, ho *pyjson.Object) bool {
	changed := false
	if c := ho.Value("command"); c != pandoHookCommand(self) && !keepsPath(c, pandoHookCommand(self)) {
		ho.Set("command", pandoHookCommand(self))
		changed = true
	}
	async, _ := ho.Value("async").(bool)
	if async != pandoHookAsync(ev) {
		if pandoHookAsync(ev) {
			ho.Set("async", true)
		} else {
			ho.Delete("async")
		}
		changed = true
	}
	return changed
}

// PlanPandoClaudeHooks adds the pando hook to SessionStart, UserPromptSubmit,
// Stop and SessionEnd of a Claude Code settings.json, unless it is there.
func PlanPandoClaudeHooks(p string, prior *Edit, self string) (*Edit, error) {
	s, existed, warn, err := settingsOf(p, prior, p+" is not valid JSON; skipping the pando hooks to avoid "+
		"overwriting your Claude Code settings. Fix the file and re-run `daisugi install`.")
	if err != nil || warn != nil {
		return warn, err
	}
	changed := false
	for _, ev := range pandoHookEvents {
		hooks, list, err := eventList(s, ev)
		if err != nil {
			return nil, err
		}
		have := false
		for _, e := range list {
			eo, ok := e.(*pyjson.Object)
			if !ok {
				return nil, ErrUnsupported
			}
			for _, h := range asList(eo.Value("hooks")) {
				if ho, ok := h.(*pyjson.Object); ok && isPandoHook(ho.Value("command")) {
					have = true
					if pandoHookFresh(ev, self, ho) {
						changed = true
					}
				}
			}
		}
		if !have {
			hooks.Set(ev, append(list, pandoHookEntry(ev, self)))
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	return &Edit{Path: p, Existed: existed, Content: dumpSettings(s)}, nil
}

// pandoTOMLHeader is the table header the layer owns in Codex's config.toml.
const pandoTOMLHeader = "[mcp_servers.pando]"

// pandoBlockLines finds the layer's block as a range of lines: start is the
// header line, end the line after the block. A header counts only when it is
// a whole line (a comment that holds the text does not). The block runs to
// the next table header that is not one of its own sub-tables.
func pandoBlockLines(lines []string) (start, end int, ok bool) {
	start = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == pandoTOMLHeader || strings.HasPrefix(t, pandoTOMLHeader+" #") || strings.HasPrefix(t, pandoTOMLHeader+"#") {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[mcp_servers.pando.") {
			end = i
			break
		}
	}
	return start, end, true
}

// PlanPandoCodexMCP adds [mcp_servers.pando] to a Codex config.toml.
func PlanPandoCodexMCP(p, self string, prior *Edit) (*Edit, error) {
	text, existed, err := textOf(p, prior)
	if err != nil {
		return nil, err
	}
	if _, _, has := pandoBlockLines(strings.Split(text, "\n")); has {
		return nil, nil
	}
	block := "\n" + pandoTOMLHeader + "\ncommand = " + pyjson.Dumps(self, true) + "\nargs = [\"pando\", \"serve\"]\n"
	sep := ""
	if text != "" {
		sep = "\n"
	}
	return &Edit{Path: p, Existed: existed, Content: strings.TrimRight(text, "\n") + sep + block}, nil
}

// PlanPandoCodexMCPRemove takes the block out again.
func PlanPandoCodexMCPRemove(p string, prior *Edit) (*Edit, error) {
	text, existed, err := textOf(p, prior)
	if err != nil {
		return nil, ErrUnsupported
	}
	lines := strings.Split(text, "\n")
	start, end, has := pandoBlockLines(lines)
	if !existed || !has {
		return nil, nil
	}
	kept := append(append([]string{}, lines[:start]...), lines[end:]...)
	cleaned := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if cleaned != "" {
		cleaned += "\n"
	}
	return &Edit{Path: p, Existed: true, Content: cleaned}, nil
}

// hermesHomes are the Hermes dirs the layer writes into: the root and each
// profile dir that has a config.yaml or a state.db.
func hermesHomes(home string) []string {
	root := hermesdb.Root(os.Getenv("HERMES_HOME"), home)
	out := []string{root}
	ents, _ := os.ReadDir(filepath.Join(root, "profiles"))
	for _, e := range ents {
		d := filepath.Join(root, "profiles", e.Name())
		if !e.IsDir() {
			continue
		}
		for _, f := range []string{"config.yaml", "state.db"} {
			if _, err := os.Stat(filepath.Join(d, f)); err == nil {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// PlanPandoHermesConfig sets the pando MCP server and enables the pando
// plugin in a Hermes config.yaml.
func PlanPandoHermesConfig(p, self string, prior *Edit) (*Edit, error) {
	cfg, existed, _, err := yamlOfPrior(p, prior)
	if errors.Is(err, errYAML) {
		return &Edit{Path: p, Warning: p + " is not valid YAML; skipping pando to avoid overwriting your Hermes config. " +
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
	if _, has := mcp.Get(PandoMCPName); !has {
		mcp.Set(PandoMCPName, pandoMCPEntry(false, self))
		changed = true
	}
	pv, has := cfg.Get("plugins")
	if !has {
		pv = pyjson.NewObject()
		cfg.Set("plugins", pv)
	}
	plugins, ok := pv.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	ev, has := plugins.Get("enabled")
	if !has || ev == nil {
		ev = []any{}
	}
	list, ok := ev.([]any)
	if !ok {
		return nil, ErrUnsupported
	}
	found := false
	for _, x := range list {
		found = found || x == "pando"
	}
	if !found {
		plugins.Set("enabled", append(list, "pando"))
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

// PlanPandoHermesReverse takes the MCP server and the plugin entry out.
func PlanPandoHermesReverse(p string, prior *Edit) (*Edit, error) {
	cfg, existed, _, err := yamlOfPrior(p, prior)
	if !existed || errors.Is(err, errYAML) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnsupported
	}
	changed := false
	if servers, ok := cfg.Value("mcp_servers").(*pyjson.Object); ok {
		if _, has := servers.Get(PandoMCPName); has {
			servers.Delete(PandoMCPName)
			if servers.Len() == 0 {
				cfg.Delete("mcp_servers")
			}
			changed = true
		}
	}
	if plugins, ok := cfg.Value("plugins").(*pyjson.Object); ok {
		if list, ok := plugins.Value("enabled").([]any); ok {
			var kept []any
			for _, x := range list {
				if x != "pando" {
					kept = append(kept, x)
				}
			}
			if len(kept) != len(list) {
				changed = true
				if len(kept) > 0 {
					plugins.Set("enabled", kept)
				} else {
					plugins.Delete("enabled")
				}
				if plugins.Len() == 0 {
					cfg.Delete("plugins")
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

// pandoPluginYAML is the manifest of the Hermes plugin.
const pandoPluginYAML = `name: pando
version: "1.0"
description: Puts the Pando memory view into the context of every turn. Managed by daisugi install.
provides_hooks:
  - pre_llm_call
`

// pandoPluginPy is the plugin. The program path is written at install.
const pandoPluginPy = `# Managed by daisugi install. Puts the Pando memory view into the context of
# every turn: Hermes appends what pre_llm_call returns to the user message,
# and does not keep it, so a later turn needs it again. The text is the same
# bytes until the memory changes. It reads the memory through the daisugi
# program and never fails a turn. A process marked DAISUGI_PANDO_SKIP gets
# nothing (the program prints nothing then). The session id goes to the
# program too: a long-running agent's chat (DAISUGI_PANDO_BRANCH) is then
# logged into its branch of the memory, never into the store above it.
import subprocess

DAISUGI = %s


def _view(session_id=None, is_first_turn=False, **kwargs):
    argv = [DAISUGI, "pando", "context"]
    if session_id:
        argv += ["--session", str(session_id)]
    try:
        out = subprocess.run(argv, capture_output=True, text=True, timeout=10)
    except Exception:
        return None
    text = (out.stdout or "").strip()
    if out.returncode != 0 or not text:
        return None
    return {"context": text}


def register(ctx):
    ctx.register_hook("pre_llm_call", _view)
`

// PlanPandoHermesPlugin writes the plugin into dir/plugins/pando.
func PlanPandoHermesPlugin(dir, self string) *Edit {
	cur := true
	for rel, want := range pandoPluginFiles(self) {
		if b, err := os.ReadFile(filepath.Join(dir, "plugins", "pando", rel)); err != nil || string(b) != want {
			cur = false
		}
	}
	if cur {
		return nil
	}
	return &Edit{Path: filepath.Join(dir, "plugins", "pando"), Kind: "pando-plugin", Content: self, Listed: true}
}

func pandoPluginFiles(self string) map[string]string {
	return map[string]string{"plugin.yaml": pandoPluginYAML, "__init__.py": strings.Replace(pandoPluginPy, "%s", pyjson.Dumps(self, true), 1)}
}

// PandoUnitPath is where the user service file goes.
func PandoUnitPath(home string) string {
	return wardenroot.Join(home, ".config/systemd/user/"+PandoUnitName)
}

// pandoUnitMark is the first line of a unit this layer wrote. A unit file
// without it is the owner's own.
const pandoUnitMark = "# Managed by daisugi install."

// pandoUnit is the service file text.
func pandoUnit(self string) string {
	return pandoUnitMark + "\n[Unit]\nDescription=openDaisugi Pando memory follower\n\n" +
		"[Service]\nExecStart=" + verify.ShlexQuote(self) + " pando follow\nRestart=on-failure\nRestartSec=10\n\n" +
		"[Install]\nWantedBy=default.target\n"
}

// PandoUnitNote is the note install prints for the unit: install starts the
// service when systemd runs for the user, and says how to do it by hand when
// it did not.
const PandoUnitNote = "Pando follow service file written. If systemd does not run it for you, start it yourself: " +
	"systemctl --user enable --now " + PandoUnitName

// PlanPandoUnit writes the user service file.
func PlanPandoUnit(home, self string) (*Edit, error) {
	p := PandoUnitPath(home)
	text, existed, err := readText(p)
	if err != nil {
		return nil, err
	}
	want := pandoUnit(self)
	if text == want {
		return nil, nil
	}
	return &Edit{Path: p, Existed: existed, Kind: "pando-unit", Content: want, Listed: true, Note: PandoUnitNote}, nil
}

// UnitHook runs systemctl for the service. nil (the default, and in every
// test) means install only writes the file. The install command sets it for
// a real home and never for a dry run. action is "enable" (daemon-reload,
// then enable --now), "disable" (disable --now) or "reload" (daemon-reload
// after the file is gone). A failure is the hook's to report, in one line;
// it never fails the install.
var UnitHook func(action, unit string) error

// DedupeUnitEdits keeps one pando unit edit across the runtimes of one
// install: Claude Code and Hermes share the file, so the second plan of it
// is dropped.
func DedupeUnitEdits(changes []*Change) {
	seen := map[string]bool{}
	for _, ch := range changes {
		var kept []*Edit
		for _, e := range ch.Edits {
			if e != nil && (e.Kind == "pando-unit" || e.Kind == "pando-unit-remove") {
				if seen[e.Kind+e.Path] {
					continue
				}
				seen[e.Kind+e.Path] = true
			}
			kept = append(kept, e)
		}
		ch.Edits = kept
	}
}

func isOurUnit(p string) bool {
	b, err := os.ReadFile(p)
	return err == nil && strings.HasPrefix(string(b), pandoUnitMark)
}

func applyPandoEdit(e *Edit, backed map[string]bool) error {
	switch e.Kind {
	case "pando-plugin":
		if err := os.MkdirAll(e.Path, 0o777); err != nil {
			return err
		}
		for rel, text := range pandoPluginFiles(e.Content) {
			if _, err := wardenroot.WriteFile(filepath.Join(e.Path, rel), text); err != nil {
				return err
			}
		}
	case "pando-unit":
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o777); err != nil {
			return err
		}
		// A unit the owner edited by hand is kept as a backup first.
		if _, err := os.Lstat(e.Path); err == nil && !isOurUnit(e.Path) {
			if err := backupOnce(e.Path, backed); err != nil {
				return err
			}
		}
		if _, err := wardenroot.WriteFile(e.Path, e.Content); err != nil {
			return err
		}
		if UnitHook != nil {
			_ = UnitHook("enable", PandoUnitName)
		}
	case "pando-unit-remove":
		if UnitHook != nil {
			_ = UnitHook("disable", PandoUnitName)
		}
		_, err := clearPath(e.Path)
		if err == nil && UnitHook != nil {
			_ = UnitHook("reload", PandoUnitName)
		}
		return err
	}
	return nil
}

// pandoInstallSteps plans the layer's edits for one runtime, in order.
func pandoInstallSteps(rt Runtime, home, self string, ch *Change) []func() (*Edit, error) {
	j := func(rel string) string { return wardenroot.Join(home, rel) }
	var steps []func() (*Edit, error)
	add := func(s func() (*Edit, error)) { steps = append(steps, s) }
	switch rt.Key {
	case "claude":
		add(func() (*Edit, error) {
			return planMCPNamed(PandoMCPName, j(".claude.json"), false, []string{"mcpServers"}, pandoMCPEntry(true, self),
				"pando MCP registration", priorOf(ch, j(".claude.json")))
		})
		settings := j(".claude/settings.json")
		add(func() (*Edit, error) {
			e, err := PlanPandoClaudeHooks(settings, priorOf(ch, settings), self)
			if err == nil && e != nil && e.Warning == "" {
				err = missingDir(settings)
			}
			return e, err
		})
	case "codex":
		toml := j(".codex/config.toml")
		add(func() (*Edit, error) { return PlanPandoCodexMCP(toml, self, priorOf(ch, toml)) })
	case "hermes":
		for _, d := range hermesHomes(home) {
			cfg := filepath.Join(d, "config.yaml")
			add(func() (*Edit, error) { return PlanPandoHermesConfig(cfg, self, priorOf(ch, cfg)) })
			add(func() (*Edit, error) { return PlanPandoHermesPlugin(d, self), nil })
		}
	}
	if rt.Key == "claude" || rt.Key == "hermes" {
		add(func() (*Edit, error) { return PlanPandoUnit(home, self) })
	}
	return steps
}

// pandoReverseSteps plans the layer's removal for one runtime.
func pandoReverseSteps(rt Runtime, home string, ch *Change, gone map[string]bool) []func() (*Edit, error) {
	j := func(rel string) string { return wardenroot.Join(home, rel) }
	var steps []func() (*Edit, error)
	add := func(s func() (*Edit, error)) { steps = append(steps, s) }
	switch rt.Key {
	case "claude":
		add(func() (*Edit, error) {
			return planPopMCPNamed(PandoMCPName, j(".claude.json"), "mcpServers", priorOf(ch, j(".claude.json")))
		})
		settings := j(".claude/settings.json")
		add(func() (*Edit, error) { return popHook(settings, isPandoHook, pandoHookEvents, priorOf(ch, settings)) })
	case "codex":
		toml := j(".codex/config.toml")
		add(func() (*Edit, error) { return PlanPandoCodexMCPRemove(toml, priorOf(ch, toml)) })
	case "hermes":
		for _, d := range hermesHomes(home) {
			cfg := filepath.Join(d, "config.yaml")
			add(func() (*Edit, error) { return PlanPandoHermesReverse(cfg, priorOf(ch, cfg)) })
			add(func() (*Edit, error) { return PlanRemove(filepath.Join(d, "plugins", "pando")), nil })
		}
	}
	if rt.Key == "claude" || rt.Key == "hermes" {
		add(func() (*Edit, error) {
			p := PandoUnitPath(home)
			if gone[p] {
				return nil, nil
			}
			if _, err := os.Lstat(p); err != nil || !isOurUnit(p) {
				return nil, nil // not there, or the owner's own file: leave it
			}
			gone[p] = true
			return &Edit{Path: p, Kind: "pando-unit-remove", Listed: true}, nil
		})
	}
	return steps
}

// pandoPlanSteps are the dry-run lines of the layer for one runtime.
func pandoPlanSteps(rt Runtime, home string) []Step {
	j := func(rel string) string { return wardenroot.Join(home, rel) }
	var out []Step
	add := func(desc, target string) {
		out = append(out, Step{Layer: LayerPando, Description: desc, Target: target, Supported: true})
	}
	switch rt.Key {
	case "claude":
		add(`Register MCP server "pando" (the memory: recall, zoom, date, view)`, j(".claude.json"))
		add("Add pando memory hooks (SessionStart view; UserPromptSubmit, Stop and SessionEnd log the chat)", j(".claude/settings.json"))
	case "codex":
		add(`Register MCP server "pando"`, j(".codex/config.toml"))
	case "hermes":
		for _, d := range hermesHomes(home) {
			add(`Register MCP server "pando" and enable the pando plugin (memory view in every turn)`, filepath.Join(d, "config.yaml"))
			add("Install the pando plugin", filepath.Join(d, "plugins", "pando"))
		}
	}
	if rt.Key == "claude" || rt.Key == "hermes" {
		add("Add the pando follow user service (live log of every chat, background summaries); enable and start it", PandoUnitPath(home))
	}
	return out
}
