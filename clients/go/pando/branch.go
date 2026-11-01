package pando

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"daisugi-verify/internal/pandoskip"
)

// Round 8b: one tree, shaped like an org chart. A long-running agent (a
// project lead the steward starts) gets a branch: a store of its own inside
// the store above it, <store>/branches/<label>/, with the same layout. A
// lead's own long-runners are branches of its branch. Everything an agent
// says goes into its branch, never into the log above it. An agent sees its
// branch and everything below it; whoever is above sees down.
//
// A branch is not a second store (PD-R-2 stands): it lives inside one store,
// and no code reads two stores.

// BranchesDir is the dir under a store (or a branch) that holds its branches.
const BranchesDir = "branches"

// BranchFile marks a branch dir and holds its state.
const BranchFile = "branch.json"

// BranchVar names the branch of this process (see pandoskip.BranchVar).
const BranchVar = pandoskip.BranchVar

// MaxBranchDepth is the deepest branch path.
const MaxBranchDepth = 8

// Branch states.
const (
	BranchOpen   = "open"
	BranchClosed = "closed"
)

// BranchInfo is branch.json.
type BranchInfo struct {
	Path    string `json:"path"`
	Created string `json:"created"`
	State   string `json:"state"`
	Closed  string `json:"closed,omitempty"`
}

// ParseBranch splits a branch path ("trellis/ui") into its labels. It
// refuses an empty path, an absolute path, an empty, "." or ".." part and
// a label that is not a child label.
func ParseBranch(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("the branch is empty")
	}
	if strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("bad branch %q: a branch is a path of labels inside the store, never an absolute path", path)
	}
	parts := strings.Split(path, "/")
	if len(parts) > MaxBranchDepth {
		return nil, fmt.Errorf("bad branch %q: at most %d levels", path, MaxBranchDepth)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || !ValidChild(p) {
			return nil, fmt.Errorf("bad branch %q: each part is a label of letters, digits, dot, dash and underscore, 1 to 64, starting with a letter or digit", path)
		}
	}
	return parts, nil
}

// JoinBranch is the path of sub below base ("" is the store).
func JoinBranch(base, sub string) string {
	switch {
	case base == "":
		return sub
	case sub == "":
		return base
	}
	return base + "/" + sub
}

// BranchDir is the dir of the branch path below the store (or branch) in
// storeDir. Each level is checked: no part may be a symbolic link. It does
// not say whether the branch exists.
func BranchDir(storeDir, path string) (string, error) {
	parts, err := ParseBranch(path)
	if err != nil {
		return "", err
	}
	d := storeDir
	for _, p := range parts {
		if err := noLink(d, BranchesDir, p); err != nil {
			return "", err
		}
		d = filepath.Join(d, BranchesDir, p)
	}
	return d, nil
}

// ExistingBranch is BranchDir for a branch that exists: every level is a
// real dir that holds branch.json.
func ExistingBranch(storeDir, path string) (string, error) {
	d, err := BranchDir(storeDir, path)
	if err != nil {
		return "", err
	}
	parts, _ := ParseBranch(path)
	cur := storeDir
	for _, p := range parts {
		cur = filepath.Join(cur, BranchesDir, p)
		if !IsBranch(cur) {
			return "", fmt.Errorf("no branch %s", path)
		}
	}
	return d, nil
}

// IsBranch says whether dir is a branch: a real dir with a branch.json.
func IsBranch(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, BranchFile))
	return err == nil && st.Mode().IsRegular()
}

// ReadBranch reads branch.json.
func ReadBranch(dir string) (BranchInfo, error) {
	var bi BranchInfo
	b, err := readFile(filepath.Join(dir, BranchFile))
	if err != nil {
		return bi, err
	}
	if err := json.Unmarshal(b, &bi); err != nil {
		return bi, fmt.Errorf("pando: %s is not valid JSON: %w", filepath.Join(dir, BranchFile), err)
	}
	if bi.State == "" {
		bi.State = BranchOpen
	}
	return bi, nil
}

func writeBranch(dir string, bi BranchInfo) error {
	b, err := encodeLine(bi)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, BranchFile), b)
}

// EnsureBranch makes the branch and every level above it that is missing,
// each with its branch.json, and returns its dir. A closed branch stays
// closed: a message, a worker's transcript or a Hermes row goes into it
// without opening it again (fix 1, F5). Only a new long-running agent
// opens it again (ReopenBranch).
func EnsureBranch(storeDir, path string, now func() time.Time) (string, error) {
	return ensureBranch(storeDir, path, now, false)
}

// ReopenBranch is EnsureBranch that also opens a closed branch again: the
// steward starts a long-running agent in it.
func ReopenBranch(storeDir, path string, now func() time.Time) (string, error) {
	return ensureBranch(storeDir, path, now, true)
}

func ensureBranch(storeDir, path string, now func() time.Time, reopen bool) (string, error) {
	if now == nil {
		now = time.Now
	}
	parts, err := ParseBranch(path)
	if err != nil {
		return "", err
	}
	if _, err := BranchDir(storeDir, path); err != nil {
		return "", err
	}
	cur := storeDir
	for k, p := range parts {
		cur = filepath.Join(cur, BranchesDir, p)
		if err := os.MkdirAll(cur, dirMode); err != nil {
			return "", err
		}
		if err := noLink(filepath.Dir(cur), p); err != nil {
			return "", err
		}
		here := strings.Join(parts[:k+1], "/")
		bi, err := ReadBranch(cur)
		switch {
		case errors.Is(err, os.ErrNotExist):
			bi = BranchInfo{Path: here, Created: now().UTC().Format(DateLayout), State: BranchOpen}
		case err != nil:
			return "", err
		case bi.State == BranchOpen || !reopen:
			continue
		default:
			bi.State, bi.Closed = BranchOpen, ""
		}
		if err := writeBranch(cur, bi); err != nil {
			return "", err
		}
	}
	return cur, nil
}

// CloseBranch marks a branch closed. A closed branch keeps every message
// and stays searchable from above; the view above no longer names it. Its
// branches below are left as they are.
func CloseBranch(storeDir, path string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	d, err := ExistingBranch(storeDir, path)
	if err != nil {
		return err
	}
	bi, err := ReadBranch(d)
	if err != nil {
		return err
	}
	if bi.State == BranchClosed {
		return nil
	}
	bi.State, bi.Closed = BranchClosed, now().UTC().Format(DateLayout)
	return writeBranch(d, bi)
}

// ListBranches names the branches right below dir, sorted.
func ListBranches(dir string) []string {
	if noLink(dir, BranchesDir) != nil {
		return nil
	}
	ents, err := os.ReadDir(filepath.Join(dir, BranchesDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !ValidChild(e.Name()) {
			continue
		}
		if IsBranch(filepath.Join(dir, BranchesDir, e.Name())) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// BranchTree names every branch below dir, depth first, each as its path
// from dir, sorted at each level.
func BranchTree(dir string) []string {
	var out []string
	var walk func(d, base string, depth int)
	walk = func(d, base string, depth int) {
		if depth >= MaxBranchDepth {
			return
		}
		for _, b := range ListBranches(d) {
			p := JoinBranch(base, b)
			out = append(out, p)
			walk(filepath.Join(d, BranchesDir, b), p, depth+1)
		}
	}
	walk(dir, "", 0)
	return out
}

// SubStore is a store below a store: a worker's child store or a branch.
// Rel is how a command names it: "w-1/" for a child of the store itself,
// "branches/trellis/" for a branch, "branches/trellis/children/w-2/" for a
// worker of that branch.
type SubStore struct {
	Rel string
	Dir string
}

// SubStores lists every store below dir that tend builds: its children,
// then each branch and the children and branches below it, depth first.
func SubStores(dir string) []SubStore {
	var out []SubStore
	for _, ch := range ListChildren(dir) {
		d, err := ChildDir(dir, ch)
		if err == nil {
			out = append(out, SubStore{Rel: ch + "/", Dir: d})
		}
	}
	var walk func(d, rel string, depth int)
	walk = func(d, rel string, depth int) {
		if depth >= MaxBranchDepth {
			return
		}
		for _, b := range ListBranches(d) {
			bd := filepath.Join(d, BranchesDir, b)
			br := rel + BranchesDir + "/" + b + "/"
			if hasMain(bd) {
				out = append(out, SubStore{Rel: br, Dir: bd})
			}
			for _, ch := range ListChildren(bd) {
				cd, err := ChildDir(bd, ch)
				if err == nil {
					out = append(out, SubStore{Rel: br + ChildrenDir + "/" + ch + "/", Dir: cd})
				}
			}
			walk(bd, br, depth+1)
		}
	}
	walk(dir, "", 0)
	return out
}

func hasMain(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "main"))
	return err == nil && st.IsDir()
}

// ---- the scope of a process ----

// Scope is what a process may read: Dir and everything below it. Branch is
// its path from the store ("" for the store itself). Worker says the
// process is a short worker of the branch: search and zoom only, no view,
// and nothing logged. Closed says the process may read nothing.
type Scope struct {
	Store  string // the store dir
	Dir    string // the dir the process sees as its top
	Branch string
	Worker bool
	Closed bool
	Why    string // why it is closed, for a command line
}

// ScopeOf is the scope of a process with getenv over the store in
// storeDir. No marker is the store. A worker with no branch is closed. A
// branch marker that is bad, a link or not there is closed: the process
// never falls back to the store above it.
func ScopeOf(storeDir string, getenv func(string) string) Scope {
	sc := Scope{Store: storeDir, Dir: storeDir}
	if getenv == nil {
		return sc
	}
	skip := SkipOn(getenv)
	branch := strings.TrimSpace(getenv(BranchVar))
	if branch == "" {
		if skip {
			sc.Closed, sc.Why, sc.Dir = true, ClosedText, ""
		}
		return sc
	}
	sc.Branch, sc.Worker = branch, skip
	d, err := ExistingBranch(storeDir, branch)
	if err != nil {
		sc.Closed, sc.Why, sc.Dir = true, "The memory is closed to this process: its branch "+branch+" cannot be read ("+err.Error()+").", ""
		return sc
	}
	sc.Dir = d
	return sc
}

// Below resolves a branch path given relative to the scope ("" is the
// scope itself) to an existing dir inside it.
func (sc Scope) Below(sub string) (string, error) {
	if sub == "" {
		return sc.Dir, nil
	}
	return ExistingBranch(sc.Dir, sub)
}

// ---- the session map: which chats belong to which branch ----

// RouteFile is the session map of a store, <store>/branch.sessions: one
// "id<TAB>branch" line per session, a session id or a transcript path, or
// "hermes:<id>" for a Hermes session. The first line for an id wins; the
// file only grows. coppice writes a lead's minted session id here before
// the lead starts, and a lead's own hooks add theirs. The follow service,
// the hooks and the importer read it, so a branch chat never lands in the
// log above it.
const RouteFile = "branch.sessions"

// Routes is the session map.
type Routes map[string]string

// LoadRoutes reads the session map of a store. A missing file is empty.
func LoadRoutes(storeDir string) Routes {
	rt := Routes{}
	f, err := os.Open(filepath.Join(storeDir, RouteFile))
	if err != nil {
		return rt
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		id, br, ok := strings.Cut(sc.Text(), "\t")
		id, br = strings.TrimSpace(id), strings.TrimSpace(br)
		if !ok || id == "" || br == "" {
			continue
		}
		if _, err := ParseBranch(br); err != nil && br != RootClaim {
			continue
		}
		// The owner's claim wins over a branch line written before it
		// (review fix 3, H2); otherwise the first line for an id wins.
		if br == RootClaim && !strings.HasPrefix(id, DirRoutePrefix) {
			rt[id] = RootClaim
			continue
		}
		if _, have := rt[id]; !have {
			rt[id] = br
		}
	}
	return rt
}

// RootClaim is the branch field of a line that claims a session for the
// store itself: the owner's own SessionStart writes it, so no branch can
// map the session later (first line wins; review fix 2, G5).
const RootClaim = "-"

// ClaimRoot puts ids on the map as the store's own: the owner's unmarked
// SessionStart, whose working dir is cwd. A lead's hooks carry the marker,
// so an unmarked session is the owner's, and its claim wins over a branch
// line written for it before (a lead that read the id elsewhere; H2),
// unless cwd is under a branch's dir route: then nothing of the event is
// claimed (J5). A dir: id is never claimed (H5).
func ClaimRoot(storeDir, cwd string, ids ...string) error {
	have := LoadRoutes(storeDir)
	// Decided once per event (review fix 4, J5): a session whose cwd is
	// under a branch's dir route is that branch's, never claimed, so the
	// hooks and the follow service agree on every line of it.
	if have.ForCwd(cwd) != "" {
		return nil
	}
	var b strings.Builder
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || strings.ContainsAny(id, "\t\r\n") || strings.HasPrefix(id, DirRoutePrefix) {
			continue
		}
		if br, ok := have[id]; ok && br == RootClaim {
			continue
		}
		have[id] = RootClaim
		b.WriteString(id + "\t" + RootClaim + "\n")
	}
	if b.Len() == 0 {
		return nil
	}
	return appendRoutes(storeDir, b.String())
}

func appendRoutes(storeDir, text string) error {
	if err := os.MkdirAll(storeDir, dirMode); err != nil {
		return err
	}
	if err := noLink(storeDir, RouteFile); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(storeDir, RouteFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// DirRoutePrefix starts a dir route: "dir:<abs dir>" maps every Claude
// session whose cwd is that dir or inside it (fix 1, F3). coppice writes one
// for a long-running agent's own worktree, so a `claude -p` or a model call
// the agent starts there goes to its branch too.
const DirRoutePrefix = "dir:"

// ForCwd is the branch of the dir route that holds cwd, the longest dir
// first, or "".
func (rt Routes) ForCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	best, bestLen := "", -1
	for k, b := range rt {
		d, ok := strings.CutPrefix(k, DirRoutePrefix)
		if !ok || d == "" || !filepath.IsAbs(d) || b == RootClaim {
			continue
		}
		if underAny(cwd, []string{d}) && len(d) > bestLen {
			best, bestLen = b, len(d)
		}
	}
	return best
}

// Dirs are the dirs of the dir routes, for the importer.
func (rt Routes) Dirs() []string {
	var out []string
	for k, b := range rt {
		if d, ok := strings.CutPrefix(k, DirRoutePrefix); ok && filepath.IsAbs(d) && b != RootClaim {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// RootHolds says whether the store in storeDir (its own log, not its
// branches) already holds messages of the session id names: a Claude
// session id or transcript path, or "hermes:<id>". Such a session is the
// owner's: it is never mapped to a branch, and a line that maps it is not
// followed (fix 1, F2).
func RootHolds(storeDir, id string) bool {
	var needles [][]byte
	if sid, ok := strings.CutPrefix(id, "hermes:"); ok {
		q, _ := json.Marshal(":" + sid + ":")
		needles = append(needles, []byte(`"act":"hermes:`), []byte(string(q[1:len(q)-1])))
	} else {
		sid := id
		if strings.Contains(id, "/") {
			sid = strings.TrimSuffix(filepath.Base(id), ".jsonl")
		}
		if sid == "" {
			return false
		}
		q, _ := json.Marshal(sid)
		whole := []byte(`"chat":` + string(q))
		forked := []byte(`"chat":` + string(q[:len(q)-1]) + "~")
		return scanMain(storeDir, func(line []byte) bool {
			return bytes.Contains(line, whole) || bytes.Contains(line, forked)
		})
	}
	return scanMain(storeDir, func(line []byte) bool {
		return bytes.Contains(line, needles[0]) && bytes.Contains(line, needles[1])
	})
}

// scanMain says whether a line of the store's day files matches.
func scanMain(storeDir string, match func([]byte) bool) bool {
	files, err := dayFiles(filepath.Join(storeDir, "main"))
	if err != nil {
		return false
	}
	for _, f := range files {
		data, err := readFile(filepath.Join(storeDir, "main", f))
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(line) > 0 && match(line) {
				return true
			}
		}
	}
	return false
}

// ErrRootHolds is the answer to a route for a session the store above
// already holds.
type ErrRootHolds struct{ IDs []string }

func (e *ErrRootHolds) Error() string {
	return "the store already holds these sessions, so they stay the owner's: " + strings.Join(e.IDs, ", ")
}

// For is the branch of a session, by its id, its transcript path or the
// file name stem of the path, or "".
func (rt Routes) For(sessionID, path string) string {
	if len(rt) == 0 {
		return ""
	}
	if path != "" {
		if b := rt[path]; b != "" {
			return b
		}
		if b := rt[strings.TrimSuffix(filepath.Base(path), ".jsonl")]; b != "" {
			return b
		}
	}
	if sessionID != "" {
		return rt[sessionID]
	}
	return ""
}

// AddRoute puts ids on the session map of the store, for branch, in one
// O_APPEND write. An id that is on the map already is not written again.
func AddRoute(storeDir, branch string, ids ...string) error {
	if _, err := ParseBranch(branch); err != nil {
		return err
	}
	have := LoadRoutes(storeDir)
	var b strings.Builder
	var held []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || strings.ContainsAny(id, "\t\r\n") {
			continue
		}
		if _, ok := have[id]; ok {
			continue
		}
		// A session the store above already holds is the owner's: a
		// process in a branch can never claim it (F2).
		if !strings.HasPrefix(id, DirRoutePrefix) && RootHolds(storeDir, id) {
			held = append(held, id)
			continue
		}
		have[id] = branch
		b.WriteString(id + "\t" + branch + "\n")
	}
	var refused error
	if len(held) > 0 {
		refused = &ErrRootHolds{IDs: held}
	}
	if b.Len() == 0 {
		return refused
	}
	if err := appendRoutes(storeDir, b.String()); err != nil {
		return err
	}
	return refused
}

// RemoveRoute takes every line for id off the session map of the store,
// by writing the file again. It is the owner's fix for a wrong line;
// nothing in a branch can call it. found is false when id had no line.
func RemoveRoute(storeDir, id string) (found bool, err error) {
	p := filepath.Join(storeDir, RouteFile)
	if err := noLink(storeDir, RouteFile); err != nil {
		return false, err
	}
	data, err := readFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var keep bytes.Buffer
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		k, _, _ := bytes.Cut(line, []byte{'\t'})
		if string(bytes.TrimSpace(k)) == id {
			found = true
			continue
		}
		keep.Write(line)
		keep.WriteByte('\n')
	}
	if !found {
		return false, nil
	}
	return true, writeAtomic(p, keep.Bytes())
}

// RouteRows lists the session map in file order: id and branch, the first
// line for an id only.
func RouteRows(storeDir string) [][2]string {
	data, err := readFile(filepath.Join(storeDir, RouteFile))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out [][2]string
	for _, line := range strings.Split(string(data), "\n") {
		id, br, ok := strings.Cut(line, "\t")
		id, br = strings.TrimSpace(id), strings.TrimSpace(br)
		if !ok || id == "" || br == "" || seen[id] {
			continue
		}
		if _, err := ParseBranch(br); err != nil && br != RootClaim {
			continue
		}
		seen[id] = true
		out = append(out, [2]string{id, br})
	}
	// Show what is in force: a claim wins over an earlier branch line.
	eff := LoadRoutes(storeDir)
	for i, r := range out {
		if b, ok := eff[r[0]]; ok {
			out[i][1] = b
		}
	}
	return out
}

// NewBranchSession mints a session id (a version 4 uuid) and puts it on the
// session map of the store for branch before it returns. coppice passes it
// to a lead's `claude --session-id`, so the first line of its transcript
// already belongs to the branch.
func NewBranchSession(storeDir, branch string) (string, error) {
	id, err := pandoskip.NewUUID()
	if err != nil {
		return "", err
	}
	if err := AddRoute(storeDir, branch, id); err != nil {
		return "", err
	}
	return id, nil
}

// Router sends each session's messages to its branch. The follow service
// and the hooks of a chat with no branch use it over the store they hold
// open; a branch chat is never routed again.
type Router struct {
	root   *Chat
	routes Routes
	open   map[string]*Chat
	held   map[string]bool
	// Wait is how long it waits for another writer of a branch.
	Wait time.Duration
}

// NewRouter is the router of the store c holds. For a chat that is a
// branch or a child, it routes nothing.
func NewRouter(c *Chat) *Router {
	r := &Router{root: c, open: map[string]*Chat{}, Wait: 3 * time.Second}
	if c != nil && !IsBranch(c.cfg.Dir) {
		r.routes = LoadRoutes(c.cfg.Dir)
	}
	return r
}

// Branch is the branch of a session, or "".
func (r *Router) Branch(sessionID, path string) string {
	if r == nil {
		return ""
	}
	return r.routes.For(sessionID, path)
}

// Target is the branch a session's lines go to: its own line on the map,
// else the dir route that holds its cwd. "" (the store) when nothing maps
// it, or when the store already holds the session (F2: a line that claims
// an owner's chat is not followed).
func (r *Router) Target(key, path, cwd string) string {
	if r == nil || len(r.routes) == 0 {
		return ""
	}
	br := r.routes.For(key, path)
	if br == RootClaim {
		return ""
	}
	// A dir route is coppice's own: no hook can write one (K2). A session
	// whose cwd is under it is that branch's, even when the store above
	// took its first lines before the route was followed (a follow service
	// still on an older binary when the lead started, or a race). The
	// RootHolds veto below guards only the lines a hook writes (F2).
	if d := r.routes.ForCwd(cwd); d != "" {
		return d
	}
	if br == "" {
		return ""
	}
	if r.held == nil {
		r.held = map[string]bool{}
	}
	h, ok := r.held[key]
	if !ok {
		h = RootHolds(r.root.cfg.Dir, key)
		r.held[key] = h
	}
	if h {
		return ""
	}
	return br
}

// Chat is the chat that takes the messages of branch: the root for "", else
// the branch, made when missing and opened for writing once per router.
func (r *Router) Chat(ctx context.Context, branch string) (*Chat, error) {
	if branch == "" {
		return r.root, nil
	}
	if c := r.open[branch]; c != nil {
		return c, nil
	}
	d, err := EnsureBranch(r.root.cfg.Dir, branch, r.root.cfg.Now)
	if err != nil {
		return nil, err
	}
	c, err := OpenWait(ctx, Config{Dir: d, Now: r.root.cfg.Now}, r.Wait)
	if err != nil {
		return nil, err
	}
	r.open[branch] = c
	return c, nil
}

// Close lets go of every branch the router opened.
func (r *Router) Close() {
	if r == nil {
		return
	}
	for k, c := range r.open {
		_ = c.Close()
		delete(r.open, k)
	}
}

// RouteKeys is every id on the session map, for the importer: a branch's
// chat is never imported into the store above it.
func (rt Routes) Keys() []string {
	out := make([]string, 0, len(rt))
	for k := range rt {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// skipWithRoutes is the skip list of a store with its session map added.
func skipWithRoutes(storeDir string) SkipSet {
	s := LoadSkip(SkipListPath(storeDir))
	for k, b := range LoadRoutes(storeDir) {
		if !strings.HasPrefix(k, DirRoutePrefix) && b != RootClaim {
			s[k] = true
		}
	}
	return s
}

// routeDirs are the dir routes of a store, for the importer's skip dirs.
func routeDirs(storeDir string) []string { return LoadRoutes(storeDir).Dirs() }

// BranchLineBytes caps the text of one branch line in a view.
const BranchLineBytes = 200

// BranchLinesHead starts the branch lines of a view.
const BranchLinesHead = "Branches below (the newest summary of each live branch; recall searches them, zoom with branch reads them):\n"

// BranchLines is the part of a view that names the live branches right
// below dir, one line each: the label and the summary of its newest root
// node (the newest message, clipped, while that node waits for a model).
// "" when no branch is open. A closed branch is left out; recall still
// finds it.
func BranchLines(dir string) string {
	var b strings.Builder
	for _, name := range ListBranches(dir) {
		d := filepath.Join(dir, BranchesDir, name)
		bi, err := ReadBranch(d)
		if err != nil || bi.State != BranchOpen {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(BranchLinesHead)
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, branchNewest(d))
	}
	return b.String()
}

// branchNewest is the one line a view gives a branch.
func branchNewest(dir string) string {
	if !hasMain(dir) {
		return "(no message yet)"
	}
	c, err := OpenReadOnly(dir)
	if err != nil || c.Count() == 0 {
		return "(no message yet)"
	}
	roots := c.Roots()
	if n, ok := c.nodes[roots[len(roots)-1]]; ok {
		return clip(flat(n.Text), BranchLineBytes)
	}
	m, err := c.Message(c.Count() - 1)
	if err != nil {
		return "(no message yet)"
	}
	return clip(flat(m.Text), BranchLineBytes)
}

// BranchRow is one line of `daisugi pando branches`.
type BranchRow struct {
	Path     string `json:"path"`
	State    string `json:"state"`
	Messages int64  `json:"messages"`
	Newest   string `json:"newest"`
}

// BranchRows lists every branch below dir, depth first.
func BranchRows(dir string) []BranchRow {
	var out []BranchRow
	for _, p := range BranchTree(dir) {
		d, err := ExistingBranch(dir, p)
		if err != nil {
			continue
		}
		bi, _ := ReadBranch(d)
		row := BranchRow{Path: p, State: bi.State, Newest: branchNewest(d)}
		if hasMain(d) {
			if c, err := OpenReadOnly(d); err == nil {
				row.Messages = c.Count()
			}
		}
		out = append(out, row)
	}
	return out
}

func cliBranches(env CLIEnv, dir string, f cliFlags) int {
	rows := BranchRows(dir)
	if f.asJSON {
		if rows == nil {
			rows = []BranchRow{}
		}
		return printJSON(env, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Out, "no branch")
		return 0
	}
	for _, r := range rows {
		fmt.Fprintf(env.Out, "%s\t%s\t%d\t%s\n", r.Path, r.State, r.Messages, r.Newest)
	}
	return 0
}

// RunBranchHook is RunHook for a long-running agent whose process carries a
// branch marker. SessionStart puts its session on the session map (so the
// follow service logs its transcript into the branch, not above it) and
// prints the branch's view. The logging events write the transcript into
// the branch. A marker that is bad or a link gives nothing; nothing ever
// goes to the store above. A process that cannot write (a fenced pane) only
// reads: coppice put its session on the map before it started.
func RunBranchHook(ctx context.Context, storeDir, branch string, ev HookEvent, skip []string, now func() time.Time) (out string) {
	if _, err := BranchDir(storeDir, branch); err != nil {
		return ""
	}
	ids := routeIDs(ev)
	// The folder decides (round 9): a session under this branch's own dir
	// route is the branch's by that route, so it needs no line of its own.
	if LoadRoutes(storeDir).ForCwd(ev.Cwd) == branch {
		ids = nil
	}
	switch ev.Event {
	case "SessionStart":
		_ = AddRoute(storeDir, branch, ids...)
		d, err := ExistingBranch(storeDir, branch)
		if err != nil {
			return ""
		}
		return RunHook(ctx, d, ev, skip, now)
	case "UserPromptSubmit", "Stop", "SessionEnd", "PreCompact":
		if ev.TranscriptPath == "" {
			return ""
		}
		_ = AddRoute(storeDir, branch, ids...)
		d, err := EnsureBranch(storeDir, branch, now)
		if err != nil {
			return ""
		}
		return RunHook(ctx, d, ev, skip, now)
	}
	return ""
}

// BranchEnviron is environ with BranchVar set to path; "" takes it out.
// coppice uses it for the workers it spawns.
func BranchEnviron(environ []string, path string) []string {
	return pandoskip.WithBranch(environ, path)
}

// Dir is the dir of the store c holds.
func (c *Chat) Dir() string { return c.cfg.Dir }

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidSessionID says whether id can be a plain session id on the map:
// letters, digits, dot, underscore, colon and dash (coppice's session id
// shape, which covers Claude's uuids and Hermes' state.db ids), no path,
// and never a "dir:" route.
func ValidSessionID(id string) bool {
	return sessionIDRe.MatchString(id) && !strings.HasPrefix(id, DirRoutePrefix)
}

// routeIDs are the ids of a hook event a branch hook may put on the map
// (review fix 5, K2): a plain session id, and a transcript path that is an
// absolute .jsonl file. A "dir:" id, or anything else, is dropped: only
// coppice writes a dir route.
func routeIDs(ev HookEvent) []string {
	var out []string
	if ValidSessionID(ev.SessionID) {
		out = append(out, ev.SessionID)
	}
	p := ev.TranscriptPath
	if p != "" && !strings.HasPrefix(p, DirRoutePrefix) && filepath.IsAbs(p) && filepath.Clean(p) == p && strings.HasSuffix(p, ".jsonl") {
		out = append(out, p)
	}
	return out
}
