// Package install writes and removes the gate's hooks in agent harnesses,
// the gate parts of opendaisugi.install: the PreToolUse hook in Claude
// Code's settings.json and Codex's hooks.json, and the pi and OpenCode
// gate extensions. Every file it writes is the file Python writes, apart
// from the hook command, which runs this binary instead of Python.
package install

import (
	"math"
	"os"
	"path/filepath"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// MarkerEnv is the first word of every hook command this binary writes:
// an assignment the shell drops, which Python's config.gate_hook_args and
// config.GateHookArgs here both read as the Go gate entry, so both CLIs
// see, report and remove the same hook.
const MarkerEnv = config.GateHookMarker

// HookOptions are gate.gate_settings_json's arguments.
type HookOptions struct {
	Mode          string // shadow | enforce
	Root          string
	Format        string // claude by default
	HookTimeoutS  int    // 30 by default
	VerifyTimeout float64
	CapturesRoot  string
	Session       *string
	Ask           bool
	AskTimeoutS   float64 // 90 by default
}

// Defaults fills the zero values gate_settings_json defaults.
func (o HookOptions) Defaults() HookOptions {
	if o.Format == "" {
		o.Format = "claude"
	}
	if o.HookTimeoutS == 0 {
		o.HookTimeoutS = 30
	}
	if o.VerifyTimeout == 0 {
		o.VerifyTimeout = 10
	}
	if o.AskTimeoutS == 0 {
		o.AskTimeoutS = 90
	}
	return o
}

// Self is the path this binary was run by, made absolute but with its
// symlinks kept, the program an installed hook runs. A package manager
// that swaps the file behind a stable symlink (~/.local/bin/daisugi) then
// leaves the hook working. The resolved file is the last resort.
func Self() (string, error) {
	return self(os.Args[0], os.Getenv("PATH"))
}

func self(arg0, path string) (string, error) {
	if strings.Contains(arg0, "/") {
		return filepath.Abs(arg0)
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, arg0)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return filepath.Abs(p)
		}
	}
	return os.Executable()
}

// HookEntry is the PreToolUse entry gate_settings_json builds, with the
// command running `<self> gate check` instead of
// `python -m opendaisugi.gate_client`. The flags after it are the same.
func HookEntry(self string, o HookOptions) *pyjson.Object {
	o = o.Defaults()
	q := verify.ShlexQuote
	inner := math.Min(o.VerifyTimeout, math.Max(1.0, float64(o.HookTimeoutS)-5.0))
	cmd := MarkerEnv + " " + q(self) + " gate check" +
		" --mode " + q(o.Mode) +
		" --root " + q(o.Root) +
		" --format " + q(o.Format) +
		" --verify-timeout " + pyjson.FloatRepr(inner)
	if o.CapturesRoot != "" {
		cmd += " --captures-root " + q(o.CapturesRoot)
	}
	if o.Session != nil {
		cmd += " --session " + q(*o.Session)
	}
	timeout := o.HookTimeoutS
	if o.Ask {
		cmd += " --ask --ask-timeout " + pyjson.Dumps(int(o.AskTimeoutS), false)
		if t := int(o.AskTimeoutS + o.VerifyTimeout + 5); t > timeout {
			timeout = t
		}
	}
	// Default-deny at the process boundary: on Claude Code a hook exit
	// other than 2 does not block, so in enforce mode every other nonzero
	// exit of the gate is made a deny. In shadow mode no exit is turned
	// into a deny; the gate itself still exits 2 in shadow for a call it
	// cannot decide.
	if o.Format == "claude" && o.Mode == "enforce" {
		cmd += " || exit 2"
	}
	return pyjson.NewObject().
		Set("matcher", "*").
		Set("hooks", []any{pyjson.NewObject().
			Set("type", "command").
			Set("command", cmd).
			Set("timeout", timeout)})
}

// SettingsJSON is gate_settings_json: the hooks settings document, as
// json.dumps writes it.
func SettingsJSON(self string, o HookOptions) string {
	doc := pyjson.NewObject().Set("hooks", pyjson.NewObject().
		Set("PreToolUse", []any{HookEntry(self, o)}))
	return pyjson.Dumps(doc, true)
}
