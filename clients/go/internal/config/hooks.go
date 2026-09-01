package config

import (
	"errors"
	"os"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
)

// GateHookMarker is the first word of every hook command this binary
// writes. Python's config.gate_hook_args accepts it as the Go entry point.
const GateHookMarker = "DAISUGI_GATE_HOOK=opendaisugi.gate"

// InstalledHookMode is config.installed_hook_mode: the --mode of the
// first gate hook in a settings.json PreToolUse list, "" when none, and
// "unknown" when a gate hook is in a form not read and no read hook
// enforces. err is ErrUnsupported where Python would raise instead of
// answering.
func InstalledHookMode(settingsPath string) (string, error) {
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		// read_text raises OSError: no hook.
		return "", nil
	}
	if !utf8.Valid(raw) {
		return "", nil // UnicodeDecodeError is a ValueError: no hook
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		if errors.Is(err, pyjson.ErrUnsupported) {
			return "", ErrUnsupported
		}
		// Invalid UTF-8 and invalid JSON are ValueErrors: no hook.
		return "", nil
	}
	data, isObj := v.(*pyjson.Object)
	if !isObj {
		return "", ErrUnsupported // data.get raises AttributeError
	}
	hooksV := data.Value("hooks")
	if !pyjson.Truthy(hooksV) {
		return "", nil
	}
	hooks, isObj := hooksV.(*pyjson.Object)
	if !isObj {
		return "", ErrUnsupported
	}
	preV := hooks.Value("PreToolUse")
	if !pyjson.Truthy(preV) {
		return "", nil
	}
	pre, isList := preV.([]any)
	if !isList {
		return "", ErrUnsupported
	}
	first, unknown := "", false
	for _, e := range pre {
		entry, isObj := e.(*pyjson.Object)
		if !isObj {
			return "", ErrUnsupported
		}
		hsV := entry.Value("hooks")
		if !pyjson.Truthy(hsV) {
			continue
		}
		hs, isList := hsV.([]any)
		if !isList {
			return "", ErrUnsupported
		}
		for _, h := range hs {
			hook, isObj := h.(*pyjson.Object)
			if !isObj {
				return "", ErrUnsupported
			}
			command := hook.Value("command")
			if GateHookKind(command) == KindUnknown {
				unknown = true
			}
			if mode := GateHookMode(command); mode != "" && first == "" {
				first = mode
			}
		}
	}
	// A gate hook in a form not read may enforce: only a read enforce hook
	// outranks it, never a read shadow one.
	if first == "enforce" || !unknown {
		return first, nil
	}
	return KindUnknown, nil
}

// EffectiveHook is config.EffectiveHook.
type EffectiveHook struct {
	Mode, CwdMode, GlobalMode string
}

// EffectiveHookMode is config.effective_hook_mode: the stricter of the
// machine-global hook and the one in cwd's .claude/settings.json. home
// and cwd are pathlib strings (already normalized).
func EffectiveHookMode(home, cwd string) (EffectiveHook, error) {
	globalPath := home + "/.claude/settings.json"
	cwdPath := cwd + "/.claude/settings.json"
	if home == "/" {
		globalPath = "/.claude/settings.json"
	}
	if cwd == "/" {
		cwdPath = "/.claude/settings.json"
	}
	g, err := InstalledHookMode(globalPath)
	if err != nil {
		return EffectiveHook{}, err
	}
	var c string
	if cwdPath != globalPath {
		if c, err = InstalledHookMode(cwdPath); err != nil {
			return EffectiveHook{}, err
		}
	}
	eff := EffectiveHook{CwdMode: c, GlobalMode: g}
	// max() keeps the first of equal keys: cwd before global.
	// "unknown" is a gate hook in a form not read: it may enforce.
	rank := map[string]int{"enforce": 3, KindUnknown: 2, "shadow": 1}
	for _, m := range []string{c, g} {
		if m != "" && (eff.Mode == "" || rank[m] > rank[eff.Mode]) {
			eff.Mode = m
		}
	}
	return eff, nil
}

// SourceLabel is config.hook_source_label.
func SourceLabel(eff EffectiveHook) string {
	switch {
	case eff.CwdMode != "" && eff.GlobalMode != "":
		return "project+global"
	case eff.CwdMode != "":
		return "project"
	case eff.GlobalMode != "":
		return "global"
	}
	return ""
}
