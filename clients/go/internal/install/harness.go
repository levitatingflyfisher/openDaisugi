package install

import (
	_ "embed"
	"errors"
	"os"
	"strings"

	"daisugi-verify/internal/gateroot"
)

// The gate extensions, byte for byte the files Python ships in
// harness_pi/extension/index.ts and harness_opencode/plugin/daisugi-gate.ts.
// A test fails when they drift.
var (
	//go:embed assets/pi-index.ts
	PiExtension string
	//go:embed assets/opencode-daisugi-gate.ts
	OpenCodePlugin string
)

// Harnesses is install.SUPPORTED_HARNESSES.
var Harnesses = []string{"pi", "opencode"}

// PiDir is install.pi_extension_dir.
func PiDir(home string) string { return gateroot.Join(home, ".pi/agent/extensions/daisugi-gate") }

// OpenCodePath is install.opencode_plugin_path: under $XDG_CONFIG_HOME
// when it is absolute, else ~/.config.
func OpenCodePath(home string, env map[string]string) string {
	base := gateroot.Join(home, ".config")
	if x := env["XDG_CONFIG_HOME"]; x != "" && strings.HasPrefix(x, "/") {
		base = gateroot.PathStr(x)
	}
	return gateroot.Join(base, "opencode/plugins/daisugi-gate.ts")
}

// Target is install.harness_extension_target.
func Target(name, home string, env map[string]string) string {
	if name == "opencode" {
		return OpenCodePath(home, env)
	}
	return gateroot.Join(PiDir(home), "index.ts")
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// HarnessError is the ValueError _install_opencode_plugin raises.
type HarnessError struct{ Msg string }

func (e *HarnessError) Error() string { return e.Msg }

// InstallHarness is install_harness_extension: the file written unless
// it already holds the same text; a symlink at the file is replaced, never
// written through.
func InstallHarness(name, home string, env map[string]string) ([]string, error) {
	out := Target(name, home, env)
	text := PiExtension
	if name == "opencode" {
		text = OpenCodePlugin
		parent := gateroot.Parent(out)
		if isSymlink(parent) {
			return nil, &HarnessError{Msg: parent + " is a symlink, so the plugin would land somewhere else. " +
				"Replace it with a directory, then run this again."}
		}
	}
	if err := os.MkdirAll(gateroot.Parent(out), 0o777); err != nil {
		return nil, err
	}
	if isSymlink(out) {
		if err := os.Remove(out); err != nil {
			return nil, err
		}
	}
	if cur, err := os.ReadFile(out); err == nil && string(cur) == text {
		return nil, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := gateroot.ReplaceFile(out, text); err != nil {
		return nil, err
	}
	return []string{out}, nil
}

// UninstallHarness is uninstall_harness_extension: pi's whole extension
// directory, or OpenCode's one plugin file (its directory may hold the
// user's own plugins).
func UninstallHarness(name, home string, env map[string]string) ([]string, error) {
	if name == "pi" {
		d := PiDir(home)
		if !isSymlink(d) && !pathExists(d) {
			return nil, nil
		}
		if isDirNoFollow(d) {
			return []string{d}, os.RemoveAll(d)
		}
		return []string{d}, os.Remove(d)
	}
	out := OpenCodePath(home, env)
	if isSymlink(out) || pathExists(out) {
		return []string{out}, os.Remove(out)
	}
	return nil, nil
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDirNoFollow(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.IsDir()
}
