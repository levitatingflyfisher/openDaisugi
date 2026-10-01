// Package fence is `daisugi fence`: a Landlock ruleset around any agent
// harness, applied in this process before it executes the harness, so every
// child inherits it and none can lift it.
//
// plan.go is opendaisugi/fence.py: the pure rule list. It reads no file and
// asks the kernel nothing; clients/fixtures/fence holds the oracle's golden
// cases and the test reads them. landlock_linux.go applies a list.
package fence

import (
	"path"
	"sort"
	"strings"
)

// Rule is one grant: a path and a mode. Mode "ro" is read and execute,
// "rw" is every file-system right but the unix-socket connect right,
// "unix" is only that right, "sock" is "ro" and "unix", and "rw+unix" is
// "rw" and "unix".
type Rule struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// Envelope is the part of the gate's envelope the fence reads.
type Envelope struct {
	FileRead  []string `json:"file_read"`
	FileWrite []string `json:"file_write"`
}

// Input is fence.plan's input.
type Input struct {
	Harness    string            `json:"harness"`
	Workspace  string            `json:"workspace"`
	Home       string            `json:"home"`
	OtherHomes []string          `json:"other_homes"`
	Tmpdir     string            `json:"tmpdir"`
	Env        map[string]string `json:"env"`
	PathDirs   []string          `json:"path_dirs"`
	ExeDirs    []string          `json:"exe_dirs"`
	GateRoot   string            `json:"gate_root"`
	Envelope   Envelope          `json:"envelope"`
	Allow      []string          `json:"allow"`
	Workdirs   []string          `json:"workdirs"`
	Sockets    []string          `json:"sockets"`
	Real       map[string]string `json:"real"`
	NeverExtra []string          `json:"never_extra"`
	StateExtra []string          `json:"state_extra"`
}

// Output is fence.plan's answer. Any refusal means the harness must not
// start under this plan.
type Output struct {
	Rules    []Rule   `json:"rules"`
	Notes    []string `json:"notes"`
	Refusals []string `json:"refusals"`
}

// Secrets are the paths under HOME that hold keys and tokens (fence.SECRETS).
var Secrets = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gh", ".config/gcloud",
	".config/rclone", ".config/hub", ".config/doctl", ".config/op", ".config/Bitwarden",
	".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".config/Code",
	".config/git/credentials", ".config/sops", ".config/age", ".config/huggingface", ".config/tailscale",
	".mozilla", ".thunderbird", ".age", ".pki", ".ollama", ".cache/huggingface/token",
	".netrc", ".npmrc", ".pypirc", ".git-credentials", ".password-store", ".local/share/keyrings",
	".cargo/credentials", ".cargo/credentials.toml", ".gem/credentials", ".vault-token",
	".terraform.d/credentials.tfrc.json",
}

type homePath struct{ rel, mode string }

// Harness is what a harness keeps for itself under HOME (fence.HARNESS):
// its config read only, and read and write on the parts it writes.
// ~/.claude.json and OpenCode's config, binaries and plugin packages are
// read only (FN-R-22). OpenCode's snapshot repos are read only, and of
// Claude's projects dir only this workspace's own project dir is read and
// write (Plan, FN-R-31).
var Harness = map[string][]homePath{
	"claude": {
		{".claude", "ro"}, {".claude/todos", "rw"},
		{".claude/shell-snapshots", "rw"}, {".claude/statsig", "rw"}, {".claude/session-env", "rw"},
		{".claude/file-history", "rw"}, {".claude/plans", "rw"}, {".claude/debug", "rw"},
		{".claude/ide", "rw"}, {".claude/.credentials.json", "rw"}, {".claude.json", "ro"},
		{".config/claude", "ro"}, {".cache/claude", "rw"}, {".local/state/claude", "rw"},
		{".local/share/claude", "ro"},
	},
	"codex": {
		{".codex", "ro"}, {".codex/sessions", "rw"}, {".codex/log", "rw"},
		{".codex/history.jsonl", "rw"}, {".codex/auth.json", "rw"},
	},
	"pi": {{".pi", "ro"}, {".pi/agent/sessions", "rw"}},
	"opencode": {
		{".config/opencode", "ro"}, {".local/share/opencode", "ro"}, {".local/share/opencode/storage", "rw"},
		{".local/share/opencode/log", "rw"},
		{".local/share/opencode/project", "rw"}, {".local/share/opencode/auth.json", "rw"},
		{".local/state/opencode", "rw"}, {".cache/opencode", "ro"},
	},
	"sprig": {{".local/share/sprig", "ro"}},
}

// ClaudeProjectNameMax is fence.CLAUDE_PROJECT_NAME_MAX: past it Claude
// Code shortens the name and adds a hash the fence cannot know.
const ClaudeProjectNameMax = 200

// ClaudeProjectName is fence.claude_project_name: Claude Code's dir name
// for workspace under ~/.claude/projects, every character but an ASCII
// letter or digit made "-". false when the fence cannot name it (not
// ASCII, or longer than ClaudeProjectNameMax).
func ClaudeProjectName(workspace string) (string, bool) {
	if len(workspace) > ClaudeProjectNameMax {
		return "", false
	}
	b := []byte(workspace)
	for i, c := range b {
		if c >= 0x80 {
			return "", false
		}
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b), true
}

var systemRO = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt", "/proc", "/sys",
	"/run/systemd/resolve",
}

var devices = []string{
	"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/ptmx", "/dev/pts",
}

// socketWide is fence.SOCKET_WIDE: a socket dir at or above one of these
// never takes the connect right.
var socketWide = []string{"/tmp", "/run", "/var/run", "/dev"}

// modeRights is fence.MODES: r read, w write, u connect to a socket.
var modeRights = map[string]string{"ro": "r", "rw": "rw", "unix": "u", "sock": "ru", "rw+unix": "rwu"}

var modeOrder = []string{"ro", "rw", "unix", "sock", "rw+unix"}

func rightSet(m string) map[rune]bool {
	out := map[rune]bool{}
	for _, c := range modeRights[m] {
		out[c] = true
	}
	return out
}

// merge is fence.merge: the mode with the rights of both.
func merge(a, b string) string {
	want := rightSet(a)
	for c := range rightSet(b) {
		want[c] = true
	}
	for _, m := range modeOrder {
		got := rightSet(m)
		if len(got) != len(want) {
			continue
		}
		same := true
		for c := range want {
			if !got[c] {
				same = false
			}
		}
		if same {
			return m
		}
	}
	return a
}

func norm(p string) string { return path.Clean(p) }

// Expand is fence.expand: ~ and ~/ to home, a relative path joined to base.
func Expand(p, home, base string) string { return expand(p, home, base) }

func expand(p, home, base string) string {
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") {
		p = home + p[1:]
	}
	if !strings.HasPrefix(p, "/") {
		p = base + "/" + p
	}
	return norm(p)
}

func covers(a, b string) bool {
	if a == b {
		return true
	}
	if !strings.HasSuffix(a, "/") {
		a += "/"
	}
	return strings.HasPrefix(b, a)
}

// GlobPrefix is fence.glob_prefix: the longest literal directory of an
// envelope glob.
func GlobPrefix(pattern, home, base string) string {
	p := pattern
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") {
		p = home + p[1:]
	}
	if !strings.HasPrefix(p, "/") {
		p = base + "/" + p
	}
	var keep []string
	for _, part := range strings.Split(p, "/") {
		if strings.ContainsAny(part, "*?[{") {
			break
		}
		keep = append(keep, part)
	}
	j := strings.Join(keep, "/")
	if j == "" {
		j = "/"
	}
	return norm(j)
}

func appendUniq(out []string, p string) []string {
	for _, x := range out {
		if x == p {
			return out
		}
	}
	return append(out, p)
}

// DataDirs is fence.data_dirs (datahome.guarded_data_dirs).
func DataDirs(env map[string]string, home string) []string {
	usable := func(v string) string {
		if v == "" {
			return ""
		}
		if v == "~" {
			v = home
		} else if strings.HasPrefix(v, "~/") {
			v = home + v[1:]
		}
		if strings.HasPrefix(v, "/") {
			return v
		}
		return ""
	}
	out := []string{norm(home + "/.opendaisugi")}
	if d := usable(env["OPENDAISUGI_HOME"]); d != "" {
		out = appendUniq(out, norm(d))
	}
	if x := usable(env["XDG_DATA_HOME"]); x != "" {
		out = appendUniq(out, norm(x+"/opendaisugi"))
	}
	return out
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// CoppiceDirs is fence.coppice_dirs: tokens and keys live there.
func CoppiceDirs(env map[string]string, home string) []string {
	cfg := orDefault(env["XDG_CONFIG_HOME"], home+"/.config")
	state := orDefault(env["XDG_STATE_HOME"], home+"/.local/state")
	data := orDefault(env["XDG_DATA_HOME"], home+"/.local/share")
	var out []string
	for _, p := range []string{cfg + "/coppice", state + "/coppice", data + "/coppice"} {
		out = appendUniq(out, norm(p))
	}
	if c := env["COPPICE_DATA_DIR"]; strings.HasPrefix(c, "/") {
		out = appendUniq(out, norm(c))
	}
	return out
}

// TmpParents is fence.tmp_parents: the directories whose fence-tmp/<leaf>
// the fence makes its temp dir in.
func TmpParents(env map[string]string, homes []string) []string {
	var out []string
	for _, h := range homes {
		for _, d := range DataDirs(env, h) {
			out = appendUniq(out, d)
		}
		for _, d := range CoppiceDirs(env, h) {
			out = appendUniq(out, d)
		}
		out = appendUniq(out, norm(orDefault(env["XDG_DATA_HOME"], h+"/.local/share")+"/opendaisugi"))
	}
	for i := range out {
		out[i] += "/fence-tmp"
	}
	return out
}

// Homes is the home list fence.plan uses: home first, then each other
// absolute one, once.
func Homes(home string, others []string) []string {
	out := []string{norm(home)}
	for _, h := range others {
		if strings.HasPrefix(h, "/") {
			out = appendUniq(out, norm(h))
		}
	}
	return out
}

func dominates(o, r string) bool {
	have := rightSet(o)
	for c := range rightSet(r) {
		if !have[c] {
			return false
		}
	}
	return true
}

func dirname(p string) string { return path.Dir(p) }

// Plan is fence.plan.
func Plan(in Input) Output {
	home := norm(in.Home)
	homes := Homes(home, in.OtherHomes)
	workspace := norm(in.Workspace)
	env := in.Env
	if env == nil {
		env = map[string]string{}
	}
	out := Output{Rules: []Rule{}, Notes: []string{}, Refusals: []string{}}

	var homeSecrets []string
	for _, h := range homes {
		for _, s := range Secrets {
			homeSecrets = appendUniq(homeSecrets, norm(h+"/"+s))
		}
	}
	for _, x := range in.NeverExtra {
		homeSecrets = appendUniq(homeSecrets, norm(x))
	}
	secrets := append([]string{}, homeSecrets...)
	var stateDirs []string
	for _, h := range homes {
		for _, s := range CoppiceDirs(env, h) {
			secrets = appendUniq(secrets, s)
		}
		for _, d := range DataDirs(env, h) {
			stateDirs = appendUniq(stateDirs, d)
		}
	}
	state := append([]string{}, stateDirs...)
	if in.GateRoot != "" {
		state = appendUniq(state, norm(in.GateRoot))
	}
	for _, x := range in.StateExtra {
		state = appendUniq(state, norm(x))
	}
	sort.Strings(secrets)
	sort.Strings(homeSecrets)
	sort.Strings(state)

	resolved := func(p string) string {
		if r, ok := in.Real[p]; ok {
			return norm(r)
		}
		return norm(p)
	}

	grants := map[string]string{}
	put := func(p, mode string) {
		if old, ok := grants[p]; ok {
			grants[p] = merge(old, mode)
		} else {
			grants[p] = mode
		}
	}
	opensUnnamed := func(p string) string {
		for _, s := range secrets {
			if covers(p, s) && p != s {
				return s
			}
		}
		return ""
	}
	secretHit := func(p string) string {
		for _, s := range secrets {
			if covers(p, s) || covers(s, p) {
				return s
			}
		}
		return ""
	}
	stateHit := func(p string) string {
		for _, s := range state {
			if covers(p, s) || covers(s, p) {
				return s
			}
		}
		return ""
	}
	type allowSpec struct{ path, mode string }
	var allowSpecs []allowSpec
	for _, spec := range in.Allow {
		p, mode := spec, "ro"
		if strings.HasSuffix(spec, ":rw") {
			p, mode = spec[:len(spec)-3], "rw"
		} else if strings.HasSuffix(spec, ":ro") {
			p = spec[:len(spec)-3]
		}
		allowSpecs = append(allowSpecs, allowSpec{expand(p, home, workspace), mode})
	}
	underAllow := func(p, mode string) bool {
		for _, a := range allowSpecs {
			if !covers(a.path, p) || !dominates(a.mode, mode) || opensUnnamed(a.path) != "" {
				continue
			}
			if a.mode == "rw" && stateHit(a.path) != "" {
				continue
			}
			return true
		}
		return false
	}
	strict := func(p, mode, who string) {
		if underAllow(p, mode) {
			put(p, mode)
			return
		}
		if s := secretHit(p); s != "" {
			out.Refusals = append(out.Refusals, who+" "+p+" would reach the secret "+s+
				". Only --allow opens a secret. Name another directory, or run with --no-fence.")
			return
		}
		if mode != "ro" {
			if s := stateHit(p); s != "" {
				out.Refusals = append(out.Refusals, who+" "+p+" would let the agent write the gate's state under "+s+
					". Name another directory, or run with --no-fence.")
				return
			}
		}
		put(p, mode)
	}
	allowed := func(p, mode string) {
		if s := opensUnnamed(p); s != "" {
			out.Refusals = append(out.Refusals, "--allow "+p+" would also open "+s+
				". Name a narrower directory, or run with --no-fence.")
			return
		}
		if mode == "rw" {
			if s := stateHit(p); s != "" {
				out.Refusals = append(out.Refusals, "--allow "+p+" would let the agent write the gate's state under "+s+
					". Name another directory, or run with --no-fence.")
				return
			}
		}
		put(p, mode)
	}
	ambient := func(p, mode string) {
		if secretHit(p) != "" || (mode == "rw" && stateHit(p) != "") {
			return
		}
		put(p, mode)
	}

	for _, p := range systemRO {
		ambient(p, "ro")
	}
	for _, p := range devices {
		put(p, "rw")
	}
	strict(workspace, "rw", "the workspace")
	for _, d := range in.Workdirs {
		strict(expand(d, home, workspace), "rw", "the work dir")
	}
	if in.Tmpdir != "" {
		tmp := norm(in.Tmpdir)
		own := false
		for _, tp := range TmpParents(env, homes) {
			if dirname(tmp) == tp {
				own = true
			}
		}
		if own && opensUnnamed(tmp) == "" {
			put(tmp, "rw")
		} else {
			strict(tmp, "rw", "the temp dir")
		}
	}
	for _, h := range Harness[in.Harness] {
		strict(resolved(norm(home+"/"+h.rel)), h.mode, "the harness path")
	}
	if in.Harness == "claude" {
		projects := resolved(norm(home + "/.claude/projects"))
		if name, ok := ClaudeProjectName(workspace); ok {
			strict(projects+"/"+name, "rw", "the harness path")
		} else {
			strict(projects, "rw", "the harness path")
			out.Notes = append(out.Notes, "the fence cannot name Claude's project dir for this workspace; "+
				"~/.claude/projects stays writable, the memory of every project with it.")
		}
	}
	for _, d := range in.PathDirs {
		if strings.HasPrefix(d, "/") {
			ambient(norm(d), "ro")
		}
	}
	for _, d := range in.ExeDirs {
		if strings.HasPrefix(d, "/") {
			ambient(norm(d), "ro")
		}
	}
	for _, d := range stateDirs {
		put(d+"/gate", "sock")
		put(d+"/config.yaml", "ro")
	}
	if in.GateRoot != "" {
		put(norm(in.GateRoot), "sock")
	}

	wide := append(append([]string{}, socketWide...), homes...)
	if rt := env["XDG_RUNTIME_DIR"]; strings.HasPrefix(rt, "/") {
		wide = append(wide, norm(rt))
	}
sockets:
	for _, sock := range in.Sockets {
		if !strings.HasPrefix(sock, "/") {
			continue
		}
		d := dirname(norm(sock))
		for _, w := range wide {
			if covers(d, w) {
				out.Notes = append(out.Notes, "socket dir "+d+": too wide for the fence; dropped.")
				continue sockets
			}
		}
		for _, s := range homeSecrets {
			if covers(d, s) || covers(s, d) {
				out.Notes = append(out.Notes, "socket dir "+d+": would reach a secret directory; dropped.")
				continue sockets
			}
		}
		put(d, "unix")
	}

	for _, kind := range []struct {
		key, mode string
		pats      []string
	}{{"file_read", "ro", in.Envelope.FileRead}, {"file_write", "rw", in.Envelope.FileWrite}} {
	pats:
		for _, pat := range kind.pats {
			pre := resolved(GlobPrefix(pat, home, workspace))
			if covers(workspace, pre) {
				continue
			}
			wideEnv := pre == "/"
			for _, h := range homes {
				if covers(pre, h) {
					wideEnv = true
				}
			}
			switch {
			case wideEnv:
				out.Notes = append(out.Notes, "envelope "+kind.key+" "+pat+": too wide for the fence; dropped.")
			case secretHit(pre) != "":
				out.Notes = append(out.Notes, "envelope "+kind.key+" "+pat+": would reach a secret directory; dropped.")
			case kind.mode == "rw" && stateHit(pre) != "":
				out.Notes = append(out.Notes, "envelope "+kind.key+" "+pat+": would reach the gate's state; dropped.")
			default:
				put(pre, kind.mode)
				continue pats
			}
		}
	}

	for _, a := range allowSpecs {
		allowed(a.path, a.mode)
	}

	paths := make([]string, 0, len(grants))
	for p := range grants {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var rules []Rule
	for _, p := range paths {
		rules = append(rules, Rule{p, grants[p]})
	}
	for _, r := range rules {
		redundant := false
		for _, o := range rules {
			if o.Path == r.Path || !covers(o.Path, r.Path) {
				continue
			}
			if dominates(o.Mode, r.Mode) {
				redundant = true
				break
			}
		}
		if !redundant {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

// Guard holds the plan's protected paths for the apply-time check.
type Guard struct{ secrets, state []string }

// NewGuard is the protected-path half of Plan for in.
func NewGuard(in Input) *Guard {
	env := in.Env
	if env == nil {
		env = map[string]string{}
	}
	g := &Guard{}
	for _, h := range Homes(in.Home, in.OtherHomes) {
		for _, s := range Secrets {
			g.secrets = appendUniq(g.secrets, norm(h+"/"+s))
		}
		g.secrets = append(g.secrets, CoppiceDirs(env, h)...)
		g.state = append(g.state, DataDirs(env, h)...)
	}
	for _, x := range in.NeverExtra {
		g.secrets = append(g.secrets, norm(x))
	}
	if in.GateRoot != "" {
		g.state = append(g.state, norm(in.GateRoot))
	}
	for _, x := range in.StateExtra {
		g.state = append(g.state, norm(x))
	}
	return g
}

// Check is the plan's test run again on the path a rule's root resolved
// to: why the rule must not stand, or "".
func (g *Guard) Check(real, mode, spelled string) string {
	for _, s := range g.secrets {
		if covers(real, s) || covers(s, real) {
			return "the rule " + spelled + " now resolves to " + real + ", which reaches the secret " + s +
				". Nothing was run."
		}
	}
	if mode == "rw" || mode == "rw+unix" {
		for _, s := range g.state {
			if covers(real, s) || covers(s, real) {
				return "the rule " + spelled + " now resolves to " + real + ", inside the gate's state under " + s +
					". Nothing was run."
			}
		}
	}
	return ""
}
