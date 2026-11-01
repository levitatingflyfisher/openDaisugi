package config

import (
	"os"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
)

// WardenHookProgram is config.warden_hook_program: the program of a warden hook
// in the `daisugi warden check` form (the form the Go and Rust installs
// write, under any file name), or "" for any other command.
func WardenHookProgram(command any) string {
	s, isStr := command.(string)
	if !isStr {
		return ""
	}
	tokens, ok := CommandTokens(s)
	if !ok {
		return ""
	}
	for _, seg := range segments(tokens) {
		if _, ok := binaryWardenArgs(seg); ok {
			return skipEnv(seg)[0]
		}
	}
	return ""
}

// GoneHook is one warden hook whose program is not there.
type GoneHook struct{ Program, Mode string }

// MissingHookPrograms is config.missing_hook_programs: each warden hook in
// the settings file whose program is an absolute path that is not there,
// such as a binary a package manager removed on upgrade. That hook fails
// on every call. Anything that is not the usual shape is skipped.
func MissingHookPrograms(settingsPath string) []GoneHook {
	raw, err := os.ReadFile(settingsPath)
	if err != nil || !utf8.Valid(raw) {
		return nil
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil
	}
	var out []GoneHook
	data, _ := v.(*pyjson.Object)
	if data == nil {
		return nil
	}
	hooks, _ := data.Value("hooks").(*pyjson.Object)
	if hooks == nil {
		return nil
	}
	pre, _ := hooks.Value("PreToolUse").([]any)
	for _, e := range pre {
		entry, _ := e.(*pyjson.Object)
		if entry == nil {
			continue
		}
		hs, _ := entry.Value("hooks").([]any)
		for _, h := range hs {
			hook, _ := h.(*pyjson.Object)
			if hook == nil {
				continue
			}
			command := hook.Value("command")
			prog := WardenHookProgram(command)
			if prog == "" || !strings.HasPrefix(prog, "/") {
				continue
			}
			if _, err := os.Stat(prog); err == nil {
				continue
			}
			g := GoneHook{Program: prog, Mode: WardenHookMode(command)}
			dup := false
			for _, o := range out {
				dup = dup || o == g
			}
			if !dup {
				out = append(out, g)
			}
		}
	}
	return out
}

// MissingHookWarning is config.missing_hook_warning: what warden status says
// about a warden hook whose program is gone.
func MissingHookWarning(settingsPath string, g GoneHook) string {
	if g.Mode == "enforce" {
		return "warning: the warden hook in " + settingsPath + " runs " + g.Program +
			", which does not exist, so every call is denied. Run: daisugi install --enforce"
	}
	return "warning: the warden hook in " + settingsPath + " runs " + g.Program +
		", which does not exist, so the warden sees no call. Run: daisugi install"
}

// StaleModeHooks is config.stale_mode_hooks: whether the settings file
// holds a PreToolUse warden hook that passes --mode shadow. Anything that is
// not the usual shape is skipped.
func StaleModeHooks(settingsPath string) bool {
	raw, err := os.ReadFile(settingsPath)
	if err != nil || !utf8.Valid(raw) {
		return false
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return false
	}
	data, _ := v.(*pyjson.Object)
	if data == nil {
		return false
	}
	hooks, _ := data.Value("hooks").(*pyjson.Object)
	if hooks == nil {
		return false
	}
	pre, _ := hooks.Value("PreToolUse").([]any)
	for _, e := range pre {
		entry, _ := e.(*pyjson.Object)
		if entry == nil {
			continue
		}
		hs, _ := entry.Value("hooks").([]any)
		for _, h := range hs {
			if hook, _ := h.(*pyjson.Object); hook != nil && IsStaleModeHook(hook.Value("command")) {
				return true
			}
		}
	}
	return false
}

// StaleModeWarning is config.stale_mode_warning: what warden status says
// about a warden hook that passes --mode shadow.
func StaleModeWarning(settingsPath string) string {
	return "warning: the warden hook in " + settingsPath + " passes --mode shadow, which is now " +
		"--mode audit, so the warden refuses every call. Run: daisugi install --uninstall, " +
		"then daisugi install"
}
