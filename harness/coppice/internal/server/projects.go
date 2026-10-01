package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/textwidth"
)

// maxRecentDirs is how many of the directories panes most recently started
// in a server remembers, newest first.
const maxRecentDirs = 10

// recentDirsPath is where a server keeps the directory list, so it
// survives a restart the same way layout.json does.
func recentDirsPath(dataDir string) string {
	return filepath.Join(dataDir, "recent-dirs.json")
}

// loadRecentDirs reads the recent directory list, newest first. A missing
// or unreadable file answers an empty list rather than an error: losing
// this list only costs a guess at the most recent project, never a pane
// that already exists.
func loadRecentDirs(dataDir string) []string {
	b, err := os.ReadFile(recentDirsPath(dataDir))
	if err != nil {
		return nil
	}
	var dirs []string
	if err := json.Unmarshal(b, &dirs); err != nil {
		return nil
	}
	return dirs
}

// saveRecentDirs writes dirs the same way config.Save writes coppice.toml:
// a temp file in the same directory, then a rename, so a reader never sees
// a half-written file.
func saveRecentDirs(dataDir string, dirs []string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(dirs)
	if err != nil {
		return err
	}
	path := recentDirsPath(dataDir)
	tmp, err := os.CreateTemp(dataDir, "recent-dirs.json.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// recordRecentDir puts dir at the front of the recent directory list,
// drops any earlier copy of it, and keeps only the newest maxRecentDirs.
// handlePaneCreate calls this once a pane has actually started, so a
// directory a pane failed to start in never crowds out one that worked.
func (s *Server) recordRecentDir(dir string) {
	// The foreman's own directory is its scratch space, not a project.
	if dir == "" || s.isForemanDir(dir) {
		return
	}
	// The same directory reached once through a symlink and once through
	// its real path must count as one recent entry, not two - a resolve
	// failure (the directory is already gone, say) keeps the given form
	// rather than losing the record entirely.
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	dirs := loadRecentDirs(s.cfg.DataDir)
	out := make([]string, 0, len(dirs)+1)
	out = append(out, dir)
	for _, d := range dirs {
		if d == dir {
			continue
		}
		out = append(out, d)
	}
	if len(out) > maxRecentDirs {
		out = out[:maxRecentDirs]
	}
	s.writeData(func() { _ = saveRecentDirs(s.cfg.DataDir, out) })
}

// pinnedProjects reads coppice.toml's projects list. A missing or
// unreadable file answers no pinned projects: project.list still has the
// recent directories to show, and a pane.create with no cwd still has the
// start directory as its last resort.
func pinnedProjects() []string {
	cfg, found, err := config.Load()
	if err != nil || !found {
		return nil
	}
	return cfg.Projects
}

// RegisterProjectCommands wires up project.list. New calls this beside
// RegisterPaneCommands.
func (s *Server) RegisterProjectCommands() {
	_ = s.Handle("project.list", s.handleProjectList)
	_ = s.Handle("project.remove", s.handleProjectRemove)
}

// handleProjectRemove takes one directory out of the project list: the
// pinned list in coppice.toml, and the recent list. It is how a row marked
// missing goes away. A path in neither list is refused by name. The role
// check is in operatorVerbs: only the operator edits the list. A named
// web token may not edit the config either, the same as floor.chat.
func (s *Server) handleProjectRemove(c *Client, r *proto.Request) proto.Response {
	if c.tokenNamed() {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, ConfigRefusal)
	}
	path, _ := r.Str("path")
	if path == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"project.remove needs path. Pass the project directory.")
	}
	if !filepath.IsAbs(path) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"project.remove needs an absolute path. Pass the full project directory.")
	}
	given := filepath.Clean(path)
	resolved := given
	if x, err := filepath.EvalSymlinks(given); err == nil {
		resolved = x
	}
	// The raw string as sent matches too, so a hand-edited "/src/x/" pin
	// can be removed.
	same := func(p string) bool { return p == path || p == given || p == resolved }
	found := false
	cfg, ok, err := config.Load()
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal,
			"cannot read "+config.Path()+": "+err.Error())
	}
	if ok {
		kept := make([]string, 0, len(cfg.Projects))
		for _, p := range cfg.Projects {
			if same(p) {
				found = true
				continue
			}
			kept = append(kept, p)
		}
		if found {
			cfg.Projects = kept
			if err := config.Save(cfg); err != nil {
				return proto.ErrResp(r.ID, proto.ErrInternal,
					"cannot write "+config.Path()+": "+err.Error())
			}
		}
	}
	s.recentMu.Lock()
	dirs := loadRecentDirs(s.cfg.DataDir)
	left := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if same(d) {
			found = true
			continue
		}
		left = append(left, d)
	}
	if len(left) != len(dirs) {
		s.writeData(func() { _ = saveRecentDirs(s.cfg.DataDir, left) })
	}
	s.recentMu.Unlock()
	if !found {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			resolved+" is not a project. Run: coppice project list")
	}
	return proto.OKResp(r.ID, map[string]any{"removed": resolved})
}

// handleProjectList answers the operator's pinned projects first, then the
// recently started-in directories that are not already pinned, each with
// its base name, and the default harness.
func (s *Server) handleProjectList(_ *Client, r *proto.Request) proto.Response {
	pinned := pinnedProjects()
	recent := recentNotPinned(s.cfg.DataDir, pinned)
	out := make([]map[string]any, 0, len(pinned)+len(recent))
	seen := make(map[string]bool, len(pinned))
	for _, p := range pinned {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, projectRow(p, true))
	}
	for _, d := range recent {
		out = append(out, projectRow(d, false))
	}
	// default is the harness a one-click create starts, or "" when the
	// config file names none. A client that picks the project itself must
	// name the harness, and this is the one it should name.
	def, _ := defaultHarnessName()
	return proto.OKResp(r.ID, map[string]any{"projects": out, "default": def})
}

// recentNotPinned is the recent directory list with anything already in
// pinned taken out, still newest first. It is project.list's own "recent"
// half - shown separately from the pinned rows above it, so a directory
// does not appear twice in that listing. defaultCwd, below, does NOT use
// this: it reads the raw recent list, unfiltered, since a directory being
// pinned should not cost it its place as the most recently used one.
func recentNotPinned(dataDir string, pinned []string) []string {
	in := make(map[string]bool, len(pinned))
	for _, p := range pinned {
		in[p] = true
	}
	dirs := loadRecentDirs(dataDir)
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if !in[d] {
			out = append(out, d)
		}
	}
	return out
}

func projectRow(path string, pinned bool) map[string]any {
	return map[string]any{"path": path, "name": filepath.Base(path), "pinned": pinned,
		"missing": !dirExists(path)}
}

// defaultCwd resolves the order a pane.create with no cwd and no cmd_argv
// falls back to: the near pane's own cwd; then the directories a pane most
// recently started in, newest first, the whole list walked for the first
// one that still exists on disk - a directory just used wins here even
// when it is also pinned, since this reads the raw recent list, not
// project.list's own "not already pinned" half, and a missing newest entry
// falls through to the next recent one before pinned projects are ever
// considered; then the pinned projects, in file order, the first that
// still exists; then this server's own start directory. It answers "" only
// when nothing at all resolves. why names the step that chose the directory:
// near, recent, pinned or start. The New reply carries it, so the screen can
// say where an agent started and why.
func (s *Server) defaultCwd(nearCwd string) (dir, why string) {
	if nearCwd != "" && dirExists(nearCwd) && !s.isForemanDir(nearCwd) {
		return nearCwd, "near"
	}
	for _, d := range loadRecentDirs(s.cfg.DataDir) {
		if dirExists(d) {
			return d, "recent"
		}
	}
	for _, p := range pinnedProjects() {
		if dirExists(p) {
			return p, "pinned"
		}
	}
	if dirExists(s.cfg.StartDir) {
		return s.cfg.StartDir, "start"
	}
	return "", ""
}

// dirExists is a plain, symlink-following directory check: empty, missing
// or not a directory all answer false.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// labelBaseName is filepath.Base(cwd), guarded against the degenerate
// answers Base gives for "/" or ".": a made-up word beats a label ending
// in a bare slash or dot.
func labelBaseName(cwd string) string {
	b := filepath.Base(cwd)
	switch b {
	case "", ".", "/":
		return "root"
	default:
		return b
	}
}

// defaultLabel is the name a pane gets when its request named none:
// harness-basename, then -2, -3 and on, the first time that exact label is
// not already on a live pane. An ended pane's label never forces
// numbering - it is not on the floor any more, and its name is free to
// reuse.
func (s *Server) defaultLabel(harness, cwd string) string {
	h := harness
	if h == "" {
		h = "shell"
	}
	// The base is cleaned and leaves room for a -N, so the label the
	// count makes is the label that is stored.
	base := strings.TrimSpace(textwidth.Printable(h+"-"+labelBaseName(cwd), labelMax-4))
	if isForemanLabel(base) {
		base += "-agent"
	}
	if !s.liveLabelTaken(base) {
		return base
	}
	for n := 2; ; n++ {
		candidate := base + "-" + strconv.Itoa(n)
		if !s.liveLabelTaken(candidate) {
			return candidate
		}
	}
}

// defaultHarnessName is the harness a pane.create with no harness and no
// cmd_argv of its own starts: coppice.toml's default line. ok is false
// when there is no config file, or the file has none set.
func defaultHarnessName() (name string, ok bool) {
	cfg, found, err := config.Load()
	if err != nil || !found || cfg.Default == "" {
		return "", false
	}
	return cfg.Default, true
}

func (s *Server) liveLabelTaken(label string) bool {
	for _, p := range s.tree.Panes() {
		if !p.Closed && p.Label == label {
			return true
		}
	}
	return false
}
