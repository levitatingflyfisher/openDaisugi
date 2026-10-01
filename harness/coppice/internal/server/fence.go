package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// fenceHarnesses are the harness names `daisugi fence` carries a table for.
var fenceHarnesses = map[string]bool{"claude": true, "codex": true, "pi": true, "opencode": true, "sprig": true}

// FenceRefusal is the message a pane, a plugin or a peer the server cannot
// place gets for turning a pane's fence off: a fenced agent must not get an
// unfenced process out of coppice (FN-R-15).
const FenceRefusal = "only the operator turns a pane's fence off."

// TypeFenceRefusal is the message a fenced pane gets for typing into a
// pane that runs with no fence.
const TypeFenceRefusal = "a fenced pane may type only into a fenced pane."

// gateRoots are the gate roots this floor's panes may use: the configured
// one, then the gate of each data dir openDaisugi may pick
// (datahome.guarded_data_dirs).
func (s *Server) gateRoots() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		for _, x := range out {
			if x == p {
				return
			}
		}
		out = append(out, p)
	}
	add(s.cfg.GateRoot)
	// The home is the one whose harness settings the floor reads. The
	// data-dir variables count only for the user's own home, so a server
	// with another home (a test's) never reads the user's gate.
	home := s.cfg.HookHome
	if home == "" {
		return out
	}
	userHome, _ := os.UserHomeDir()
	usable := func(v string) string {
		if home != userHome {
			return ""
		}
		switch {
		case v == "~":
			v = home
		case strings.HasPrefix(v, "~/"):
			v = home + v[1:]
		}
		if strings.HasPrefix(v, "/") {
			return v
		}
		return ""
	}
	if strings.HasPrefix(home, "/") {
		add(filepath.Join(home, ".opendaisugi", "gate"))
	}
	if d := usable(os.Getenv("OPENDAISUGI_HOME")); d != "" {
		add(filepath.Join(d, "gate"))
	}
	if x := usable(os.Getenv("XDG_DATA_HOME")); x != "" {
		add(filepath.Join(x, "opendaisugi", "gate"))
	}
	return out
}

// gateArmed is true when root holds envelopes and no DISARMED marker. Only a
// missing envelopes dir reads as off: a root the agent made unreadable
// (Landlock cannot stop chmod) still counts as on (FN-R-17).
func gateArmed(root string) bool {
	if _, err := os.Stat(filepath.Join(root, "envelopes")); errors.Is(err, fs.ErrNotExist) {
		return false
	}
	_, err := os.Lstat(filepath.Join(root, "DISARMED"))
	return err != nil
}

// gateOn is true when any gate root this floor's panes may use is armed.
func (s *Server) gateOn() bool {
	for _, root := range s.gateRoots() {
		if gateArmed(root) {
			return true
		}
	}
	return false
}

// fenceOn says whether a pane's harness starts under `daisugi fence`: the
// pane's own setting first, then the config's. Under "auto" it is the gate
// that decides. A plain shell pane is the operator's own and is fenced only
// when its own setting says on (a pane a fenced pane made, say).
func (s *Server) fenceOn(rec layout.Pane) bool {
	switch rec.Fence {
	case "off":
		return false
	case "on":
		return true
	}
	if rec.Harness == "" || rec.Harness == "shell" {
		return false
	}
	switch s.cfg.Fence {
	case "off":
		return false
	case "on":
		return true
	}
	return s.gateOn()
}

// paneFenced says whether the live pane id runs under the fence.
func (s *Server) paneFenced(id string) bool {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	lp, ok := s.live[id]
	return ok && lp.fenced
}

// callerFenced says whether a connection may be a fenced process: a pane
// that runs under the fence, a pane the server cannot name, a plugin, or a
// peer the kernel could not place. What such a caller starts is fenced,
// and it may type only into fenced panes (FN-R-15).
func (s *Server) callerFenced(ro role) bool {
	switch {
	case ro.unknown, ro.plugin != "":
		return true
	case ro.pane:
		return ro.paneID == "" || s.paneFenced(ro.paneID)
	}
	return false
}

// fenceGuard is the fence's part of guard: a typing verb from a caller that
// may be fenced reaches only a fenced pane.
func (s *Server) fenceGuard(ro role, cmd, target string) (string, bool) {
	if typingVerbs[cmd] && ro.noAllow && s.callerFenced(ro) && !s.paneFenced(target) {
		return TypeFenceRefusal, false
	}
	return "", true
}

// fenceTmp is a fresh private temp dir for one run of a fenced pane, on
// disk under the data dir (FN-R-14). Each start gets a new leaf, so no run
// finds an earlier run's files (FN-R-23). The fence makes it; the server
// keeps it on the live pane and removes it when the pane ends.
func (s *Server) fenceTmp(id string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return filepath.Join(s.cfg.DataDir, "fence-tmp",
		strings.NewReplacer("/", "_", ":", "_").Replace(id)+"-"+hex.EncodeToString(b[:]))
}

// pinnedEnvKeys are the variables a request may not set for a fenced pane:
// the fence reads HOME and the data dirs from them, and a pane picks its
// children's environment (FN-R-15). The server's own values stand.
var pinnedEnvKeys = []string{"HOME", "TMPDIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	"XDG_RUNTIME_DIR", "XDG_CACHE_HOME", "OPENDAISUGI_HOME"}

// pinFencedEnv drops from a fenced pane's extra env every pinned key and
// every DAISUGI_FENCE* variable.
func pinFencedEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if strings.HasPrefix(k, "DAISUGI_FENCE") {
			continue
		}
		pinned := false
		for _, p := range pinnedEnvKeys {
			if k == p {
				pinned = true
			}
		}
		if !pinned {
			out[k] = v
		}
	}
	return out
}

// fenceWrap is the command prefix that runs a pane's harness under
// `daisugi fence`, or nil when it runs bare. It is built at each start and
// never saved into the pane's argv, so a resume, a fork and a restore
// fence the same way without the layout changing. An error says why a
// fenced pane cannot start; the harness never runs unfenced by accident.
func (s *Server) fenceWrap(rec layout.Pane, tmp string) ([]string, error) {
	if !s.fenceOn(rec) {
		return nil, nil
	}
	bin, err := exec.LookPath(daisugiBin())
	if err != nil {
		return nil, fmt.Errorf("this pane starts under daisugi fence, and %s is not on PATH. "+
			"Install daisugi, or set fence = \"off\" in coppice.toml.", daisugiBin())
	}
	w := []string{bin, "fence"}
	if fenceHarnesses[rec.Harness] {
		w = append(w, "--harness", rec.Harness)
	}
	if rec.Cwd != "" {
		w = append(w, "--workspace", rec.Cwd)
	}
	if s.cfg.GateRoot != "" {
		w = append(w, "--gate-root", s.cfg.GateRoot)
	}
	w = append(w, "--tmpdir", tmp)
	if s.cfg.SocketPath != "" {
		w = append(w, "--coppice-socket", s.cfg.SocketPath)
	}
	// A sprig pane keeps its session in a directory it was given. It is a
	// work dir, checked like the workspace: the pane picks the path.
	for i, a := range rec.Argv {
		if a == "--session-dir" && i+1 < len(rec.Argv) {
			w = append(w, "--workdir", rec.Argv[i+1])
		} else if v, ok := strings.CutPrefix(a, "--session-dir="); ok {
			w = append(w, "--workdir", v)
		}
	}
	return append(w, "--"), nil
}

// fenceCmd is the prefix that runs a foreman worker or turn under the
// fence (FN-R-17): the same rules as a pane, the workspace its directory,
// and its own data dir read and write, named by --allow. keepFD3 keeps the
// key pipe. nil when the gate is off and the config does not say on.
func (s *Server) fenceCmd(harness, dir string, allow []string, keepFD3 bool) ([]string, error) {
	switch s.cfg.Fence {
	case "off":
		return nil, nil
	case "on":
	default:
		if !s.gateOn() {
			return nil, nil
		}
	}
	bin, err := exec.LookPath(daisugiBin())
	if err != nil {
		return nil, fmt.Errorf("the gate is on, so this runs under daisugi fence, and %s is not on PATH", daisugiBin())
	}
	w := []string{bin, "fence"}
	if harness != "" {
		w = append(w, "--harness", harness)
	}
	w = append(w, "--workspace", dir)
	if s.cfg.GateRoot != "" {
		w = append(w, "--gate-root", s.cfg.GateRoot)
	}
	for _, a := range allow {
		w = append(w, "--allow", a+":rw")
	}
	if s.cfg.SocketPath != "" {
		w = append(w, "--coppice-socket", s.cfg.SocketPath)
	}
	if keepFD3 {
		w = append(w, "--keep-fd", "3")
	}
	return append(w, "--"), nil
}

// sprigFence is the `daisugi fence` prefix (no "--") a foreman worker's
// sprig starts under, or nil when the gate is off and the config does not
// say on. The workspace is the worker's directory; the gate root is the
// worker's own, so the fence starts its resident gate and keeps it read
// only; the tree run adds sprig's session dir and its key descriptors
// (FN-R-25).
func (s *Server) sprigFence(dir, gateRoot string) ([]string, error) {
	w, err := s.fenceCmd("", dir, nil, false)
	if err != nil || w == nil {
		return nil, err
	}
	w = w[:len(w)-1] // the "--"
	for i := 0; i+1 < len(w); i++ {
		if w[i] == "--gate-root" {
			w = append(w[:i], w[i+2:]...)
			break
		}
	}
	return append(w, "--gate-root", gateRoot), nil
}

// claudeSettings is the --settings document a fenced claude pane starts
// with (FN-R-13). Precedence puts it over the user, project and local
// files, so a planted settings file cannot drop the gate hook or switch
// hooks off mid-session, and a ConfigChange hook refuses any change to
// those files. The gate hook covers every tool (matcher *) and a hook that
// fails blocks.
func (s *Server) claudeSettings(daisugi string) string {
	root := s.cfg.GateRoot
	if root == "" {
		for _, r := range s.gateRoots() {
			if gateArmed(r) {
				root = r
				break
			}
		}
	}
	cmd := "DAISUGI_GATE_HOOK=opendaisugi.gate " + shellQuote(daisugi) + " gate check"
	if root != "" {
		cmd += " --root " + shellQuote(root)
	}
	cmd += " --format claude --verify-timeout 10 --ask --ask-timeout 90 || exit 2"
	doc := map[string]any{
		"disableAllHooks": false,
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "*",
				"hooks": []any{map[string]any{
					"type": "command", "command": cmd, "timeout": 105, "onFailure": "block",
				}},
			}},
			"ConfigChange": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": "exit 2"}},
			}},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// fencedClaudeArgv adds the --settings document to a fenced claude pane's
// argv: after the program for a pty pane, in front for an adapter's.
func (s *Server) fencedClaudeArgv(rec layout.Pane, wrap []string, argv []string, pty bool) []string {
	if len(wrap) == 0 || rec.Harness != "claude" {
		return argv
	}
	settings := []string{"--settings", s.claudeSettings(wrap[0])}
	if pty {
		if len(argv) == 0 {
			return argv
		}
		return append(append([]string{argv[0]}, settings...), argv[1:]...)
	}
	return append(settings, argv...)
}

// workspaceWatched are the files in a workspace that run code outside any
// fence later: project MCP servers (.mcp.json), direnv (.envrc), git
// submodules, git config and hooks (of the git dir a gitfile points to,
// and of its common dir, too), and the project config of Claude, Codex and
// OpenCode. Landlock cannot cut them out of a granted tree, so the server
// hashes them when a fenced pane starts and ends (FN-R-13, FN-R-24).
func workspaceWatched(dir string) map[string]string {
	out := map[string]string{}
	if dir == "" {
		return out
	}
	key := func(p string) string {
		if rel, err := filepath.Rel(dir, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return rel
		}
		return p
	}
	budget := watchBudget
	var add func(p string, depth int)
	add = func(p string, depth int) {
		st, err := os.Lstat(p)
		if err != nil {
			return
		}
		if budget--; budget < 0 {
			out[watchTooMany] = "-"
			return
		}
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			out[key(p)] = "link:" + t
		case st.IsDir():
			if depth <= 0 {
				return
			}
			ents, _ := os.ReadDir(p)
			for _, e := range ents {
				add(filepath.Join(p, e.Name()), depth-1)
			}
		case st.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return
			}
			sum := sha256.Sum256(b)
			out[key(p)] = hex.EncodeToString(sum[:])
		}
	}
	for _, f := range []string{".mcp.json", ".envrc", ".gitmodules", "opencode.json", "opencode.jsonc"} {
		add(filepath.Join(dir, f), 0)
	}
	for _, d := range []string{".claude", ".codex", ".opencode"} {
		add(filepath.Join(dir, d), watchDepth)
	}
	gitDirWatched := func(gd string) {
		add(filepath.Join(gd, "config"), 0)
		add(filepath.Join(gd, "hooks"), 1)
	}
	git := filepath.Join(dir, ".git")
	st, err := os.Lstat(git)
	switch {
	case err != nil:
	case st.IsDir():
		gitDirWatched(git)
	case st.Mode().IsRegular():
		// A gitfile (a worktree, a submodule): the file itself, the git
		// dir it names and that dir's common dir.
		add(git, 0)
		if gd := gitfileDir(git); gd != "" {
			gitDirWatched(gd)
			if b, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
				cd := strings.TrimSpace(string(b))
				if !filepath.IsAbs(cd) {
					cd = filepath.Join(gd, cd)
				}
				gitDirWatched(filepath.Clean(cd))
			}
		}
	default:
		add(git, 0)
	}
	return out
}

// The watch reads at most watchBudget entries, watchDepth levels into a
// harness config dir; past the budget it records watchTooMany, which reads
// as a change whenever the count moves past it.
const (
	watchBudget  = 4096
	watchDepth   = 8
	watchTooMany = "(more files than the watch reads)"
)

// gitfileDir is the git dir a gitfile names ("gitdir: <path>"), resolved
// against the file's directory, or "".
func gitfileDir(gitfile string) string {
	b, err := os.ReadFile(gitfile)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	gd, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return ""
	}
	gd = strings.TrimSpace(gd)
	if gd == "" {
		return ""
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(filepath.Dir(gitfile), gd)
	}
	return filepath.Clean(gd)
}

// workspaceChanges are the watched files that differ between two hashes:
// added, removed or changed, sorted.
func workspaceChanges(before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		if before[k] != v {
			out = append(out, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// fenceOr is the fence setting of a pane started for a caller: "on" when
// the caller may be fenced, else the record's own.
func fenceOr(own string, fenced bool) string {
	if fenced {
		return "on"
	}
	return own
}

// childFence is the fence setting of a pane a fork starts for c.
func (s *Server) childFence(c *Client, own string) string {
	return fenceOr(own, s.callerFenced(roleOrOperator(c)))
}

// fencedPaneEnded is what the end of a fenced pane does: its private temp
// dir goes, and a change to a watched workspace file is kept on the
// record, where the rail shows it, and said in a note (FN-R-13).
func (s *Server) fencedPaneEnded(id string, lp *LivePane) {
	lp.endOnce.Do(func() { s.fencedPaneEndedOnce(id, lp) })
}

func (s *Server) fencedPaneEndedOnce(id string, lp *LivePane) {
	if lp.fenceTmp != "" {
		_ = os.RemoveAll(lp.fenceTmp)
	}
	now := workspaceWatched(lp.Info.Cwd)
	changed := workspaceChanges(lp.wsHash, now)
	if len(changed) == 0 {
		return
	}
	_ = s.tree.UpdatePane(id, func(p *layout.Pane) { p.WorkspaceChanged = changed })
	// The next start in this workspace waits for the operator's ack; a
	// pane that runs there now goes on (FN-R-24). A change a start already
	// held (and the operator may have acked) is not held again.
	s.holdMu.Lock()
	unseen := workspaceChanges(lp.wsSeen, now)
	lp.wsSeen = now
	s.holdMu.Unlock()
	if len(unseen) > 0 {
		s.addHold(lp.Info.Cwd, unseen)
	}
	s.Note("workspace config changed: a fenced run changed "+strings.Join(changed, ", ")+" in "+lp.Info.Cwd+
		". Check them, then run: coppice pane ack-changed "+lp.Info.Cwd, id)
}

// HoldRefusal is the start refusal in a workspace a fenced run changed,
// or in a dir under it: the files, then the held workspace twice.
const HoldRefusal = "workspace config changed: a fenced run changed %s in %s. Check them, then run: coppice pane ack-changed %s"

// AckRefusal is what a pane or a plugin gets for pane.ack_changed.
const AckRefusal = "only the operator acks a changed workspace."

// holdsPath is the file that keeps the workspaces that wait for an ack,
// so a hold outlives the pane record and a server restart.
func (s *Server) holdsPath() string { return filepath.Join(s.cfg.DataDir, "workspace-holds.json") }

// wsKey is a workspace as the holds key it: links resolved.
func wsKey(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

func (s *Server) readHolds() map[string][]string {
	m := map[string][]string{}
	if b, err := os.ReadFile(s.holdsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (s *Server) writeHolds(m map[string][]string) {
	b, _ := json.Marshal(m)
	tmp := s.holdsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.holdsPath())
	}
}

// addHold records that a fenced run changed files in dir.
func (s *Server) addHold(dir string, files []string) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	m := s.readHolds()
	k := wsKey(dir)
	seen := map[string]bool{}
	var all []string
	for _, f := range append(m[k], files...) {
		if !seen[f] {
			seen[f] = true
			all = append(all, f)
		}
	}
	sort.Strings(all)
	m[k] = all
	s.writeHolds(m)
}

// underDir is whether d is dir or below it, by path parts: /ws does not
// hold /wsx.
func underDir(d, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(d, "/")
	}
	return d == dir || strings.HasPrefix(d, strings.TrimRight(dir, "/")+"/")
}

// refreshHolds is the watch seeing a change before the pane ends (R2-4):
// each live fenced pane whose workspace is dir or above it is hashed
// again, and a change since the last look is held now.
func (s *Server) refreshHolds(dir string) {
	k := wsKey(dir)
	s.liveMu.RLock()
	var lps []*LivePane
	for _, lp := range s.live {
		if lp.fenced && lp.Info.Cwd != "" && underDir(k, wsKey(lp.Info.Cwd)) {
			lps = append(lps, lp)
		}
	}
	s.liveMu.RUnlock()
	for _, lp := range lps {
		s.holdMu.Lock()
		now := workspaceWatched(lp.Info.Cwd)
		changed := workspaceChanges(lp.wsSeen, now)
		lp.wsSeen = now
		s.holdMu.Unlock()
		if len(changed) > 0 {
			s.addHold(lp.Info.Cwd, changed)
			all := workspaceChanges(lp.wsHash, now)
			_ = s.tree.UpdatePane(lp.Info.ID, func(p *layout.Pane) { p.WorkspaceChanged = all })
		}
	}
}

// heldOver is the held workspace a start in dir waits on, and its changed
// files: a hold covers its workspace and every dir under it (R2-4).
func (s *Server) heldOver(dir string) (string, []string) {
	if dir == "" {
		return "", nil
	}
	s.refreshHolds(dir)
	k := wsKey(dir)
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	m := s.readHolds()
	keys := make([]string, 0, len(m))
	for h := range m {
		keys = append(keys, h)
	}
	sort.Strings(keys)
	for _, h := range keys {
		if len(m[h]) > 0 && underDir(k, h) {
			return h, m[h]
		}
	}
	return "", nil
}

// workspaceHold is the changed files a start in dir waits on, or nil.
func (s *Server) workspaceHold(dir string) []string {
	_, files := s.heldOver(dir)
	return files
}

// holdError is the start refusal for rec, or nil. It names the held
// workspace, which is what the operator acks.
func (s *Server) holdError(rec layout.Pane) error {
	if h, files := s.heldOver(rec.Cwd); len(files) > 0 {
		return fmt.Errorf(HoldRefusal, strings.Join(files, ", "), h, h)
	}
	return nil
}

// handleAckChanged is pane.ack_changed: the operator has checked the files
// a fenced run changed in cwd, and starts there go ahead again.
func (s *Server) handleAckChanged(c *Client, r *proto.Request) proto.Response {
	dir, _ := r.Str("cwd")
	if dir == "" || !filepath.IsAbs(dir) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "pane.ack_changed takes cwd, an absolute directory.")
	}
	s.holdMu.Lock()
	m := s.readHolds()
	k := wsKey(dir)
	files := m[k]
	delete(m, k)
	s.writeHolds(m)
	s.holdMu.Unlock()
	if files == nil {
		files = []string{}
	}
	return proto.OKResp(r.ID, map[string]any{"cwd": k, "acked": files})
}
