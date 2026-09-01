package install

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/lazyre"
)

// A binary that mise installed runs from a versioned path, such as
// ~/.local/share/mise/installs/github-.../0.44.0/daisugi. A hook that
// names that path breaks when mise installs the next version and removes
// the old one: enforce mode then denies every call, shadow mode stops
// seeing them. mise's shim for daisugi, or the stub Omarchy writes in
// ~/.local/bin, stays in one place and runs whichever version is current,
// so the hook names it instead, when there is one and it runs this very
// binary.

// MiseDirs are mise's data and shims directories, as mise documents them
// (https://mise.jdx.dev/directories.html): MISE_DATA_DIR, else
// $XDG_DATA_HOME/mise, else ~/.local/share/mise; the shims in
// MISE_SHIMS_DIR, else <data>/shims.
func MiseDirs(env map[string]string) (data, shims string) {
	data = env["MISE_DATA_DIR"]
	if data == "" {
		if x := env["XDG_DATA_HOME"]; x != "" {
			data = filepath.Join(x, "mise")
		} else if h := env["HOME"]; h != "" {
			data = filepath.Join(h, ".local", "share", "mise")
		}
	}
	shims = env["MISE_SHIMS_DIR"]
	if shims == "" && data != "" {
		shims = filepath.Join(data, "shims")
	}
	return data, shims
}

// underDir reports whether p is inside dir, both with symlinks resolved.
func underDir(p, dir string) bool {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	return strings.HasPrefix(p, d+string(filepath.Separator))
}

// HookPath is the program a hook should run for self. For a binary under
// mise's installs directory it is, in order: mise's daisugi shim, when it
// exists and `mise which daisugi` names this same binary; else a daisugi
// stub on PATH in $XDG_BIN_HOME or ~/.local/bin that execs `mise x <tool>
// -- daisugi` (what omarchy-mise-install writes), when `mise which
// daisugi`, or `mise which --tool <tool> daisugi` for the stub's own tool,
// names this same binary. versioned is true when self is under mise's
// installs and neither was found, so the hook keeps the versioned path
// and must be written again after an upgrade. which runs `mise which
// daisugi` with --tool when tool is set; nil runs the mise on PATH.
func HookPath(self string, env map[string]string, which func(tool string) (string, error)) (path string, versioned bool) {
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		return self, false
	}
	data, shims := MiseDirs(env)
	if data == "" || !underDir(real, filepath.Join(data, "installs")) {
		return self, false
	}
	if which == nil {
		which = func(tool string) (string, error) { return MiseWhich(env, tool) }
	}
	same := func(tool string) bool {
		got, err := which(tool)
		if err != nil {
			return false
		}
		target, err := filepath.EvalSymlinks(strings.TrimSpace(got))
		return err == nil && target == real
	}
	shim := filepath.Join(shims, "daisugi")
	if _, err := os.Stat(shim); err == nil && same("") {
		return shim, false
	}
	if stub, tool := MiseStub(env); stub != "" && (same("") || (tool != "" && same(tool))) {
		return stub, false
	}
	return self, true
}

// stubExec matches the exec line of a mise stub: `mise x <tool> --
// daisugi`, or `mise exec`, the tool maybe quoted.
var stubExec = lazyre.New(`(?m)\bmise["']?\s+(?:x|exec)\s+["']?([^\s"']+)["']?\s+--\s+["']?daisugi\b`)

// MiseStub finds a daisugi stub that runs daisugi through mise: a script
// named daisugi in $XDG_BIN_HOME or ~/.local/bin, in a directory on PATH,
// with a `mise x <tool> -- daisugi` line. It returns the stub's path and
// the tool, or "" when there is none.
func MiseStub(env map[string]string) (stub, tool string) {
	var dirs []string
	if x := env["XDG_BIN_HOME"]; x != "" {
		dirs = append(dirs, filepath.Clean(x))
	}
	if h := env["HOME"]; h != "" {
		dirs = append(dirs, filepath.Join(h, ".local", "bin"))
	}
	onPath := map[string]bool{}
	for _, d := range strings.Split(env["PATH"], ":") {
		if d != "" {
			onPath[filepath.Clean(d)] = true
		}
	}
	for _, d := range dirs {
		if !onPath[d] {
			continue
		}
		p := filepath.Join(d, "daisugi")
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		head := make([]byte, 64<<10)
		n, _ := io.ReadFull(f, head)
		_ = f.Close()
		text := string(head[:n])
		if !strings.HasPrefix(text, "#!") {
			continue
		}
		if m := stubExec().FindStringSubmatch(text); m != nil {
			return p, m[1]
		}
	}
	return "", ""
}

// MiseWhich runs `mise which daisugi`, with `--tool tool` when tool is
// set, using the mise on env's PATH, for at most five seconds.
func MiseWhich(env map[string]string, tool string) (string, error) {
	mise, err := LookPath(env)("mise")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{"which"}
	if tool != "" {
		args = append(args, "--tool", tool)
	}
	cmd := exec.CommandContext(ctx, mise, append(args, "daisugi")...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	return string(out), err
}

// VersionedNote is the one line install prints when the hook keeps a
// versioned mise path.
func VersionedNote(path string) string {
	return "note: " + path + " is a versioned mise install. Run daisugi install --gate again after mise upgrades daisugi."
}
