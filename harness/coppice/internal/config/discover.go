package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Found is one harness Discover found on PATH. Command is what the config
// runs: the harness name, or the real binary when the one on PATH is a
// mise wrapper script (see resolveMiseWrapper). Note says so, or is empty.
type Found struct {
	Name, Path, State string
	Command, Note     string
}

// errNoHome is why mise was not asked: with no home dir, `mise which` would
// run in the caller's folder, where a mise.toml could change the answer.
var errNoHome = errors.New("the home dir is unknown")

// MiseWhich asks mise for the real binary of a tool. A test replaces it.
var MiseWhich = func(name string) (string, error) {
	cmd, err := miseWhichCmd(name)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// miseWhichCmd runs in the home dir, so a mise.toml in the caller's folder
// cannot change the answer. With no home dir it is an error: mise is not
// run at all.
func miseWhichCmd(name string) (*exec.Cmd, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, errNoHome
	}
	cmd := exec.Command("mise", "which", name)
	cmd.Dir = home
	return cmd, nil
}

// miseExec matches the exec line of a mise wrapper: `exec mise x NAME --
// NAME "$@"` (or `mise exec`), with an optional tool@version before the --.
var miseExec = regexp.MustCompile(`^exec\s+mise\s+(?:x|exec)\s+(?:\S+\s+)*?--\s+([A-Za-z0-9._-]+)(?:\s+"\$@")?\s*$`)

// maxWrapperSize is the largest file taken for a wrapper script. Real
// binaries are larger; wrappers are three lines.
const maxWrapperSize = 2048

// resolveMiseWrapper is the real binary behind path when path is a small
// shell script that execs `mise x ... -- NAME`. The fence leaves mise no
// place to write its state, so such a wrapper dies at once in a fenced
// pane. Detection reads the file and never runs it; the one command run is
// `mise which NAME`, and its answer must be an absolute path to a regular
// file that is not the wrapper itself. ok is false for anything else.
func resolveMiseWrapper(path string) (real string, ok bool) {
	real, ok, _ = resolveMise(path)
	return real, ok
}

// resolveMise is resolveMiseWrapper that also says when path is a mise
// wrapper that was left as it is because the home dir is unknown.
func resolveMise(path string) (real string, ok, noHome bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxWrapperSize {
		return "", false, false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() || !regexp.MustCompile(`^#!\s*(?:/usr/bin/env\s+)?(?:\S*/)?(?:ba|da|z)?sh\b`).MatchString(sc.Text()) {
		return "", false, false
	}
	tool := ""
	for sc.Scan() {
		if m := miseExec.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			tool = m[1]
		}
	}
	if tool == "" || tool != filepath.Base(path) {
		return "", false, false
	}
	out, err := MiseWhich(tool)
	if errors.Is(err, errNoHome) {
		return "", false, true
	}
	if err != nil || !filepath.IsAbs(out) {
		return "", false, false
	}
	rs, err := os.Stat(out)
	if err != nil || !rs.Mode().IsRegular() {
		return "", false, false
	}
	if a, e1 := filepath.EvalSymlinks(out); e1 == nil {
		if b, e2 := filepath.EvalSymlinks(path); e2 == nil && a == b {
			return "", false, false
		}
	}
	return out, true, false
}

// FoundNotes are the lines to print for the harnesses whose command
// differs from their name.
func FoundNotes(found []Found) []string {
	var out []string
	for _, f := range found {
		if f.Note != "" {
			out = append(out, f.Note)
		}
	}
	return out
}

// probe is one harness coppice knows how to own, in the order Discover
// looks for it. State names how coppice learns the harness state.
type probe struct {
	name, state string
}

// resumeArgs are the resume_args a first-run table gets, for each harness
// whose resume flag is known. claude --resume takes a session id.
var resumeArgs = map[string][]string{
	"claude": {"--resume", "{session}"},
}

// probes is the probe order. The names match adapters.Names, so one name
// works for a pty pane and a headless one.
var probes = []probe{
	{"claude", "hooks"},
	{"codex", "hooks"},
	{"pi", "rpc"},
	{"sprig", "hooks"},
	{"opencode", "sse"},
}

// appOnlyNote is what Discover teaches when PATH holds no harness.
const appOnlyNote = "No harness on PATH. Codex Desktop, Cursor, and Antigravity are apps, " +
	"and coppice cannot own their panes. Install claude, codex, pi, sprig, or opencode."

// Discover looks up each known harness with lookPath and returns the ones
// it finds, in probe order.
func Discover(lookPath func(string) (string, error)) []Found {
	var found []Found
	for _, p := range probes {
		path, err := lookPath(p.name)
		if err != nil || path == "" {
			continue
		}
		f := Found{Name: p.name, Path: path, State: p.state}
		if real, ok, noHome := resolveMise(path); ok {
			f.Command = real
			f.Note = fmt.Sprintf("%s on PATH is a mise wrapper (%s). The config runs %s instead, because mise cannot write its state under the fence.", p.name, path, real)
		} else if noHome {
			f.Note = fmt.Sprintf("%s on PATH is a mise wrapper (%s), and the home dir is unknown, so mise was not asked for the real binary. The config runs %s as it is; under the fence mise may not start.", p.name, path, p.name)
		}
		found = append(found, f)
	}
	return found
}

// AppOnlyNote is the line to print when found is empty. It is empty when
// found holds at least one harness.
func AppOnlyNote(found []Found) string {
	if len(found) > 0 {
		return ""
	}
	return appOnlyNote
}

// FromFound builds a Config with one harness table per found harness and
// def as the default.
func FromFound(found []Found, def string) Config {
	c := Config{Default: def, Harness: map[string]Harness{}}
	for _, f := range found {
		h := Harness{Command: f.Name, State: f.State}
		if f.Command != "" {
			h.Command = f.Command
		}
		if ra := resumeArgs[f.Name]; ra != nil {
			h.ResumeArgs = append([]string{}, ra...)
		}
		c.Harness[f.Name] = h
	}
	return c
}
