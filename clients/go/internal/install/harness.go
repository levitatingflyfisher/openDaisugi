package install

import (
	_ "embed"
	"errors"
	"os"
	"strings"

	"daisugi-verify/internal/wardenroot"
)

// The warden extensions, byte for byte the files Python ships in
// harness_pi/extension/index.ts and harness_opencode/plugin/daisugi-warden.ts.
// A test fails when they drift.
var (
	//go:embed assets/pi-index.ts
	PiExtension string
	//go:embed assets/opencode-daisugi-warden.ts
	OpenCodePlugin string
)

// Harnesses is install.SUPPORTED_HARNESSES.
var Harnesses = []string{"pi", "opencode"}

// PiDir is install.pi_extension_dir.
func PiDir(home string) string { return wardenroot.Join(home, ".pi/agent/extensions/daisugi-warden") }

// OpenCodePath is install.opencode_plugin_path: under $XDG_CONFIG_HOME
// when it is absolute, else ~/.config.
func OpenCodePath(home string, env map[string]string) string {
	base := wardenroot.Join(home, ".config")
	if x := env["XDG_CONFIG_HOME"]; x != "" && strings.HasPrefix(x, "/") {
		base = wardenroot.PathStr(x)
	}
	return wardenroot.Join(base, "opencode/plugins/daisugi-warden.ts")
}

// Target is install.harness_extension_target.
func Target(name, home string, env map[string]string) string {
	if name == "opencode" {
		return OpenCodePath(home, env)
	}
	return wardenroot.Join(PiDir(home), "index.ts")
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
		parent := wardenroot.Parent(out)
		if isSymlink(parent) {
			return nil, &HarnessError{Msg: parent + " is a symlink, so the plugin would land somewhere else. " +
				"Replace it with a directory, then run this again."}
		}
	}
	if err := os.MkdirAll(wardenroot.Parent(out), 0o777); err != nil {
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
	if err := wardenroot.ReplaceFile(out, text); err != nil {
		return nil, err
	}
	return []string{out}, nil
}

// UninstallHarness is uninstall_harness_extension: pi's whole extension
// directory, or OpenCode's one plugin file (its directory may hold the
// user's own plugins). The directory that held it goes too when it is left
// empty, since install made it; the harness's own root stays.
func UninstallHarness(name, home string, env map[string]string) ([]string, error) {
	target := PiDir(home)
	if name != "pi" {
		target = OpenCodePath(home, env)
	}
	if !isSymlink(target) && !pathExists(target) {
		return nil, nil
	}
	var err error
	if name == "pi" && isDirNoFollow(target) {
		err = os.RemoveAll(target)
	} else {
		err = os.Remove(target)
	}
	if err == nil {
		removeEmptyDir(wardenroot.Parent(target)) // a real, empty dir only
	}
	return []string{target}, err
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDirNoFollow(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.IsDir()
}
