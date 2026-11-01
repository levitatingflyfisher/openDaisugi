package install

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/wardenroot"
)

// Runtime is one agent harness install knows, by its display name.
type Runtime struct {
	Key, Name string
}

// Runtimes is install._ALL_RUNTIMES, in order.
var Runtimes = []Runtime{
	{"claude", "Claude Code"},
	{"codex", "Codex"},
	{"hermes", "Hermes"},
	{"openclaw", "OpenClaw"},
}

// NotWired is the warden gap Hermes and OpenClaw report.
const NotWired = "Not wired: external/JS-shim warden not yet wired — follow-up"

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Detect is install.detect_runtimes: Claude Code, Hermes and OpenClaw by
// their home directory, Codex by its directory or `codex` on PATH.
func Detect(home string, lookPath func(string) (string, error)) []Runtime {
	var out []Runtime
	for _, rt := range Runtimes {
		dir := wardenroot.Join(home, "."+rt.Key)
		found := isDir(dir)
		if rt.Key == "codex" && !found {
			_, err := lookPath("codex")
			found = err == nil
		}
		if found {
			out = append(out, rt)
		}
	}
	return out
}

// LookPath is shutil.which over the PATH in env.
func LookPath(env map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, dir := range strings.Split(env["PATH"], ":") {
			if dir == "" {
				dir = "."
			}
			p := dir + "/" + name
			st, err := os.Stat(p)
			if err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return p, nil
			}
		}
		return "", exec.ErrNotFound
	}
}

// SelectError is _select_runtimes' ValueError.
type SelectError struct{ Msg string }

func (e *SelectError) Error() string { return e.Msg }

// Select is install._select_runtimes: each fragment by exact key or
// unique prefix, deduplicated, in the order first named.
func Select(names []string) ([]Runtime, error) {
	var out []Runtime
	seen := map[string]bool{}
	for _, raw := range names {
		frag := strings.ToLower(pyStripASCII(raw))
		var matches []Runtime
		for _, rt := range Runtimes {
			if rt.Key == frag {
				matches = []Runtime{rt}
				break
			}
		}
		if matches == nil {
			for _, rt := range Runtimes {
				if strings.HasPrefix(rt.Key, frag) {
					matches = append(matches, rt)
				}
			}
		}
		if len(matches) != 1 {
			return nil, &SelectError{Msg: fmt.Sprintf("--runtime %s matched %d runtimes; use one of: claude, codex, hermes, openclaw",
				pyjson.Repr(raw), len(matches))}
		}
		// dict insertion: a repeat keeps its first position.
		if !seen[matches[0].Name] {
			seen[matches[0].Name] = true
			out = append(out, matches[0])
		}
	}
	return out, nil
}

func pyStripASCII(s string) string {
	return strings.Trim(s, " \t\n\v\f\r\x1c\x1d\x1e\x1f")
}

// Step is an InstallStep of the warden layer.
type Step struct {
	Layer       string // "warden" or "base_url"
	Description string
	Target      string // "" for a gap note
	Supported   bool
}

// Plan is Runtime.plan(home, {WARDEN}, enforce=..., ask=...).
func Plan(rt Runtime, home string, enforce, ask bool) []Step {
	mode := "audit"
	if enforce {
		mode = "ENFORCE"
	}
	// A plain install never lowers an installed enforce hook.
	keptMode := func(file string) string {
		if !enforce {
			if m, err := config.InstalledHookMode(file); err == nil && m == "enforce" {
				return "ENFORCE, kept"
			}
		}
		return mode
	}
	switch rt.Key {
	case "claude":
		suffix := ""
		if ask {
			suffix = " + operator ask"
		}
		return []Step{{LayerWarden, "Install fail-closed warden PreToolUse hook (" + keptMode(wardenroot.Join(home, ".claude/settings.json")) + suffix + ")",
			wardenroot.Join(home, ".claude/settings.json"), true}}
	case "codex":
		return []Step{{LayerWarden, "Install warden PreToolUse hook (" + keptMode(wardenroot.Join(home, ".codex/hooks.json")) + ") — Codex hooks fail OPEN on " +
			"hook crash/timeout: deny works, a dead warden does not block",
			wardenroot.Join(home, ".codex/hooks.json"), true}}
	}
	return []Step{{LayerWarden, NotWired, "", false}}
}

// Change is what applying the warden layer to one runtime does: directories
// made first, then edits in order. Failed is a runtime Python's apply
// raises for; it writes nothing and is skipped.
type Change struct {
	Runtime Runtime
	Mkdirs  []string
	Edits   []*Edit
	Failed  bool
	Why     string // why it failed
	// Partial is a runtime that failed part way: the edits planned before
	// the step that raises are still written, as Python's apply wrote
	// them before it raised.
	Partial bool
}

// Written is the paths the change reports, in order.
func (c *Change) Written() []string {
	var out []string
	for _, e := range c.Edits {
		if e != nil && e.Warning == "" && !e.NoteOnly {
			out = append(out, e.Path)
		}
	}
	return out
}

// popStep is one _pop_json_hook call of a runtime's reverse.
type popStep struct {
	path   string
	match  func(any) bool
	events []string
}

func isAnyRecordHook(c any) bool { return config.IsRecordHook(c, "") }

// unknownWardenHook is install._refuse_unknown_warden_hooks: the reason a
// reverse must not touch a settings file holding a PreToolUse hook with the
// warden module in a form not read, or "" when there is none.
func unknownWardenHook(settingsPath string) string {
	v, st, err := readJSON(settingsPath)
	if err != nil || st != readOK {
		return ""
	}
	data, _ := v.(*pyjson.Object)
	hooks, _ := data.Value("hooks").(*pyjson.Object)
	entries, _ := hooks.Value("PreToolUse").([]any)
	for _, e := range entries {
		entry, _ := e.(*pyjson.Object)
		inner, _ := entry.Value("hooks").([]any)
		for _, h := range inner {
			hook, _ := h.(*pyjson.Object)
			command := hook.Value("command")
			if config.WardenHookKind(command) == config.KindUnknown {
				return fmt.Sprintf("%s holds a warden hook in a form this CLI does not read, so it is "+
					"left in place: %s. Remove it by hand, then run this again", settingsPath, command)
			}
		}
	}
	return ""
}

// ApplyChange makes the directories and applies the edits in order.
// followed lists "<path> -> <file>" for each write that went through a
// symlink, for the caller to say so.
func ApplyChange(ch *Change) (followed []string, err error) {
	for _, d := range ch.Mkdirs {
		if err := os.MkdirAll(d, 0o777); err != nil {
			return nil, err
		}
	}
	if ch.Failed && !ch.Partial {
		return nil, nil
	}
	backed := map[string]bool{}
	// A file an uninstall ends by removing gets no backup on the way: all
	// it holds is daisugi's own text.
	last := map[string]*Edit{}
	for _, e := range ch.Edits {
		if e != nil && (e.Kind == "" || e.Kind == "md-unpatch") {
			last[e.Path] = e
		}
	}
	for p, e := range last {
		if e.Undo && emptyContent(e.Content) {
			backed[p] = true
		}
	}
	for _, e := range ch.Edits {
		f, err := applyEdit(e, backed)
		if err != nil {
			return followed, err
		}
		if f != "" {
			followed = append(followed, e.Path+" -> "+f)
		}
	}
	return followed, nil
}

// Wires is whether a runtime wires a layer (Runtime.unsupported_layers
// leaves it out). Hermes wires neither the warden nor the router base_url;
// OpenClaw wires no warden.
func Wires(rt Runtime, layer string) bool {
	if layer == LayerPando && rt.Key == "openclaw" {
		return false // no pando hooks or plugin for OpenClaw yet
	}
	switch rt.Key {
	case "hermes":
		return layer != LayerWarden && layer != LayerBaseURL
	case "openclaw":
		return layer != LayerWarden
	}
	return true
}

// LayerPresent is install.layer_present: whether an uninstall of this one
// layer would change something now. It plans the reverse and applies
// nothing. A layer whose uninstall is refused counts as present, so a
// caller never claims to have added what it could not read.
func LayerPresent(rt Runtime, home, layer string) bool {
	ch, err := PlanReverseAll(rt, home, map[string]bool{}, map[string]bool{layer: true})
	if err != nil || ch.Failed {
		return true
	}
	return len(ch.Written()) > 0
}
