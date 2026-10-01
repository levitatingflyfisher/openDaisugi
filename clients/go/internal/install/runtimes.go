package install

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
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

// NotWired is the gate gap Hermes and OpenClaw report.
const NotWired = "Not wired: external/JS-shim gate not yet wired — follow-up"

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Detect is install.detect_runtimes: Claude Code, Hermes and OpenClaw by
// their home directory, Codex by its directory or `codex` on PATH.
func Detect(home string, lookPath func(string) (string, error)) []Runtime {
	var out []Runtime
	for _, rt := range Runtimes {
		dir := gateroot.Join(home, "."+rt.Key)
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

// Step is an InstallStep of the gate layer.
type Step struct {
	Layer       string // "gate" or "base_url"
	Description string
	Target      string // "" for a gap note
	Supported   bool
}

// Plan is Runtime.plan(home, {GATE}, enforce=..., ask=...).
func Plan(rt Runtime, home string, enforce, ask bool) []Step {
	mode := "audit"
	if enforce {
		mode = "ENFORCE"
	}
	switch rt.Key {
	case "claude":
		suffix := ""
		if ask {
			suffix = " + operator ask"
		}
		return []Step{{LayerGate, "Install fail-closed gate PreToolUse hook (" + mode + suffix + ")",
			gateroot.Join(home, ".claude/settings.json"), true}}
	case "codex":
		return []Step{{LayerGate, "Install gate PreToolUse hook (" + mode + ") — Codex hooks fail OPEN on " +
			"hook crash/timeout: deny works, a dead gate does not block",
			gateroot.Join(home, ".codex/hooks.json"), true}}
	}
	return []Step{{LayerGate, NotWired, "", false}}
}

// Change is what applying the gate layer to one runtime does: directories
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
		if e != nil && e.Warning == "" {
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

// unknownGateHook is install._refuse_unknown_gate_hooks: the reason a
// reverse must not touch a settings file holding a PreToolUse hook with the
// gate module in a form not read, or "" when there is none.
func unknownGateHook(settingsPath string) string {
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
			if config.GateHookKind(command) == config.KindUnknown {
				return fmt.Sprintf("%s holds a gate hook in a form this CLI does not read, so it is "+
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
	for _, e := range ch.Edits {
		f, err := Apply(e)
		if err != nil {
			return followed, err
		}
		if f != "" {
			followed = append(followed, e.Path+" -> "+f)
		}
	}
	return followed, nil
}
