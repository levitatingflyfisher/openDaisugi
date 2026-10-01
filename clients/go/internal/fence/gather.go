package fence

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Options is what the command line and the environment say.
type Options struct {
	Harness   string   // "" picks it from the command's name
	Workspace string   // "" is Cwd
	Allow     []string // PATH or PATH:rw
	Workdirs  []string // more dirs to read and write, checked like the workspace
	Envelope  string   // an envelope file; "" looks in the gate root
	GateRoot  string
	Tmpdir    string   // the private temp dir a caller made (coppice); "" makes one
	Sockets   []string // the coppice socket, connect only
	Cmd       []string
	Environ   []string // KEY=VALUE
	Cwd       string
	SelfExe   string // this binary's path, "" if unknown
	PwHome    string // the password entry's home; "" when unknown
	Pid       int    // the pid the harness will have (this one: exec keeps it)
}

func envMap(environ []string) map[string]string {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// real is the path with its symlinks followed, or the path itself when it
// does not exist.
func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// lookPath finds name on the PATH in env, as the shell would.
func lookPath(name string, env map[string]string, cwd string) string {
	if strings.Contains(name, "/") {
		if !strings.HasPrefix(name, "/") {
			name = filepath.Join(cwd, name)
		}
		return name
	}
	for _, d := range strings.Split(env["PATH"], ":") {
		if !strings.HasPrefix(d, "/") {
			continue
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// HarnessName is the table key for a command: the base name of its path.
func HarnessName(cmd0 string) string {
	b := filepath.Base(cmd0)
	if _, ok := Harness[b]; ok {
		return b
	}
	return ""
}

// Gather reads the environment and the disk and builds the pure input. It
// resolves every symlink first, so the rule list names the real paths, and
// it passes the other spellings of each protected path along. HOME is the
// one the harness sees; the password entry's home, when it differs, keeps
// its secrets shut too (FN-R-15).
func Gather(o Options) (Input, string, error) {
	env := envMap(o.Environ)
	home := env["HOME"]
	if !strings.HasPrefix(home, "/") {
		home = o.PwHome
	}
	if !strings.HasPrefix(home, "/") {
		return Input{}, "", os.ErrInvalid
	}
	home = real(filepath.Clean(home))
	ws := o.Workspace
	if ws == "" {
		ws = o.Cwd
	}
	if !strings.HasPrefix(ws, "/") {
		ws = filepath.Join(o.Cwd, ws)
	}
	ws = real(filepath.Clean(ws))

	in := Input{Home: home, Workspace: ws, Harness: o.Harness, OtherHomes: []string{},
		Env: map[string]string{}, PathDirs: []string{}, ExeDirs: []string{}, Allow: []string{},
		Workdirs: []string{}, Sockets: []string{}, Real: map[string]string{},
		Envelope: Envelope{FileRead: []string{}, FileWrite: []string{}}, NeverExtra: []string{}, StateExtra: []string{}}
	if strings.HasPrefix(o.PwHome, "/") {
		if pw := real(filepath.Clean(o.PwHome)); pw != home {
			in.OtherHomes = append(in.OtherHomes, pw)
		}
	}
	for _, k := range []string{"OPENDAISUGI_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
		"XDG_RUNTIME_DIR", "COPPICE_DATA_DIR"} {
		if v, ok := env[k]; ok {
			in.Env[k] = v
		}
	}

	exe := ""
	if len(o.Cmd) > 0 {
		exe = lookPath(o.Cmd[0], env, o.Cwd)
	}
	if in.Harness == "" && len(o.Cmd) > 0 {
		in.Harness = HarnessName(o.Cmd[0])
	}
	for _, d := range strings.Split(env["PATH"], ":") {
		if strings.HasPrefix(d, "/") {
			if r, err := filepath.EvalSymlinks(d); err == nil {
				in.PathDirs = append(in.PathDirs, r)
			}
		}
	}
	for _, e := range []string{exe, o.SelfExe} {
		if e != "" {
			if r, err := filepath.EvalSymlinks(e); err == nil {
				in.ExeDirs = append(in.ExeDirs, filepath.Dir(r))
			}
		}
	}

	tmpOptOut := false
	for _, spec := range o.Allow {
		p, suffix := spec, ""
		for _, s := range []string{":rw", ":ro"} {
			if strings.HasSuffix(spec, s) {
				p, suffix = spec[:len(spec)-3], s
			}
		}
		rp := real(Expand(p, home, ws))
		if suffix == ":rw" && rp == real("/tmp") {
			tmpOptOut = true
		}
		in.Allow = append(in.Allow, rp+suffix)
	}
	for _, d := range o.Workdirs {
		in.Workdirs = append(in.Workdirs, real(Expand(d, home, ws)))
	}
	for _, sck := range o.Sockets {
		if strings.HasPrefix(sck, "/") {
			sck = filepath.Clean(sck)
			in.Sockets = append(in.Sockets, filepath.Join(real(filepath.Dir(sck)), filepath.Base(sck)))
		}
	}
	if o.GateRoot != "" {
		in.GateRoot = real(Expand(o.GateRoot, home, ws))
	}
	switch {
	case o.Tmpdir != "":
		in.Tmpdir = real(Expand(o.Tmpdir, home, ws))
	case !tmpOptOut:
		in.Tmpdir = OwnTmpdir(env, home, o.Pid)
	}

	// Other spellings: a protected path that is a link, as it resolves.
	var secrets, state []string
	for _, h := range Homes(home, in.OtherHomes) {
		for _, s := range Secrets {
			secrets = append(secrets, norm(h+"/"+s))
		}
		secrets = append(secrets, CoppiceDirs(in.Env, h)...)
		state = append(state, DataDirs(in.Env, h)...)
	}
	for _, p := range secrets {
		if r := real(p); r != p {
			in.NeverExtra = append(in.NeverExtra, r)
		}
	}
	for _, p := range state {
		if r := real(p); r != p {
			in.StateExtra = append(in.StateExtra, r)
		}
	}
	// The harness table's roots, as they resolve (FN-R-16).
	for _, h := range Harness[in.Harness] {
		p := norm(home + "/" + h.rel)
		if r := real(p); r != p {
			in.Real[p] = r
		}
	}

	envPath := o.Envelope
	if envPath == "" {
		roots := DataDirs(in.Env, home)
		if in.GateRoot != "" {
			roots = []string{filepath.Dir(in.GateRoot)}
		}
		for _, d := range roots {
			c := d + "/gate/envelopes/default.json"
			if in.GateRoot != "" {
				c = in.GateRoot + "/envelopes/default.json"
			}
			if _, err := os.Stat(c); err == nil {
				envPath = c
				break
			}
		}
	}
	if envPath != "" {
		if raw, err := os.ReadFile(envPath); err == nil {
			var doc struct {
				Permissions Envelope `json:"permissions"`
			}
			if json.Unmarshal(raw, &doc) == nil {
				if doc.Permissions.FileRead != nil {
					in.Envelope.FileRead = doc.Permissions.FileRead
				}
				if doc.Permissions.FileWrite != nil {
					in.Envelope.FileWrite = doc.Permissions.FileWrite
				}
			}
		}
	}
	// Each envelope prefix, as it resolves.
	for _, pats := range [][]string{in.Envelope.FileRead, in.Envelope.FileWrite} {
		for _, pat := range pats {
			p := GlobPrefix(pat, home, ws)
			if r := real(p); r != p {
				in.Real[p] = r
			}
		}
	}
	return in, exe, nil
}

// OwnTmpdir is a standalone fence's private temp dir:
// $XDG_DATA_HOME/opendaisugi/fence-tmp/<pid>-<random>, on disk, not the
// shared /tmp (FN-R-14). Each run gets a fresh leaf, so a reused pid never
// finds an earlier run's files (FN-R-23); the pid lets the next run sweep
// the leaves of dead ones.
func OwnTmpdir(env map[string]string, home string, pid int) string {
	data := env["XDG_DATA_HOME"]
	if !strings.HasPrefix(data, "/") {
		data = home + "/.local/share"
	}
	return filepath.Join(real(filepath.Clean(data)), "opendaisugi", "fence-tmp", strconv.Itoa(pid)+"-"+Nonce())
}

// Nonce is 16 random hex digits: a temp leaf name no earlier run used.
func Nonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// No randomness: the time still makes the name new, and Mkdir
		// refuses a leaf that is there.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// MakeTmpdir makes the private temp dir 0700, fresh: it refuses a path
// that is already there, so no run reuses another run's files (FN-R-23).
// sweep also removes the leaves of dead runs next to it: a standalone fence
// execs, so nothing is left to remove its own dir at exit.
func MakeTmpdir(dir string, sweep bool) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s is already there; each run needs a fresh one", dir)
		}
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a directory of this user", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if sweep {
		parent := filepath.Dir(dir)
		ents, _ := os.ReadDir(parent)
		for _, e := range ents {
			pid, _, _ := strings.Cut(e.Name(), "-")
			n, err := strconv.Atoi(pid)
			if err != nil || filepath.Join(parent, e.Name()) == dir {
				continue
			}
			if _, err := os.Stat("/proc/" + strconv.Itoa(n)); os.IsNotExist(err) {
				_ = os.RemoveAll(filepath.Join(parent, e.Name()))
			}
		}
	}
	return nil
}

// EnsureHarnessDirs makes each read-write directory of the harness table
// under home that is missing while its parent is there: the parent is read
// only in the fence, so the harness could not make it later. Files
// (.json, .jsonl) are left alone; an empty one could break the harness.
// For claude it makes ~/.claude/projects and this workspace's project dir
// in it the same way (FN-R-31).
func EnsureHarnessDirs(harness, home, workspace string) {
	if harness == "claude" {
		if name, ok := ClaudeProjectName(workspace); ok {
			projects := home + "/.claude/projects"
			for _, p := range []string{projects, projects + "/" + name} {
				if _, err := os.Lstat(p); err == nil {
					continue
				}
				if st, err := os.Stat(filepath.Dir(p)); err == nil && st.IsDir() {
					_ = os.Mkdir(p, 0o700)
				}
			}
		}
	}
	for _, h := range Harness[harness] {
		if h.mode != "rw" || strings.HasSuffix(h.rel, ".json") || strings.HasSuffix(h.rel, ".jsonl") {
			continue
		}
		p := home + "/" + h.rel
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if st, err := os.Stat(filepath.Dir(p)); err == nil && st.IsDir() {
			_ = os.Mkdir(p, 0o700)
		}
	}
}

// GateRoots are the gate roots this plan connects to: --gate-root, else
// each data dir's gate.
func GateRoots(in Input) []string {
	if in.GateRoot != "" {
		return []string{in.GateRoot}
	}
	var out []string
	for _, d := range DataDirs(in.Env, in.Home) {
		out = append(out, d+"/gate")
	}
	return out
}

// GateOn is true when a gate root is in use: its envelopes dir is there or
// cannot be read. Only a missing envelopes dir reads as off (FN-R-17).
func GateOn(root string) bool {
	_, err := os.Stat(root + "/envelopes")
	return !errors.Is(err, os.ErrNotExist)
}

// StripEnv drops the variables that point the harness at a daemon outside
// the fence (FN-R-10): the fence stops the connect, and this saves the
// harness from trying. It also drops any DAISUGI_FENCED the caller set.
func StripEnv(environ []string) []string {
	drop := map[string]bool{"SSH_AUTH_SOCK": true, "GPG_AGENT_INFO": true, "DBUS_SESSION_BUS_ADDRESS": true,
		"DISPLAY": true, "WAYLAND_DISPLAY": true, "DAISUGI_FENCED": true}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return out
}
