// Package layout is the workspace, tab and pane tree. Ids look like "w1:p3":
// pane numbers are workspace-scoped and never reused, because a client holds an
// id across a close and must not silently start talking to a different process.
package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrNoWorkspace = errors.New("no such workspace")
	ErrNoTab       = errors.New("no such tab")
	ErrNoPane      = errors.New("no such pane")
	ErrNoTask      = errors.New("no such task")
	// ErrCycle is returned when a move would make a task its own ancestor.
	ErrCycle = errors.New("a task cannot be moved under itself or one of its descendants")
)

// schemaVersion is the layout.json schema version this package reads and
// writes. Load normalizes a missing or zero "v" to this value, so a re-Save
// records the schema it is actually using instead of writing "v": 0 forever.
const schemaVersion = 1

type PaneKind string

const (
	KindPTY      PaneKind = "pty"
	KindHeadless PaneKind = "headless"
)

type Pane struct {
	ID               string            `json:"id"`
	Workspace        string            `json:"workspace"`
	Tab              string            `json:"tab"`
	Label            string            `json:"label"`
	Cwd              string            `json:"cwd"`
	Argv             []string          `json:"argv"`
	Env              map[string]string `json:"env,omitempty"`
	Kind             PaneKind          `json:"kind"`
	Harness          string            `json:"harness,omitempty"`
	Cols             int               `json:"cols"`
	Rows             int               `json:"rows"`
	HarnessSessionID string            `json:"harness_session_id,omitempty"`
	Closed           bool              `json:"closed"`
	ExitCode         *int              `json:"exit_code,omitempty"`
	// EndedAt is the unix second a process that ended on its own, or a
	// restart that found no process behind the record, set Closed. It is
	// the sort key and age test for Recent. An operator pane.close never
	// sets it: that record leaves the tree at once instead of landing here.
	EndedAt int64 `json:"ended_at,omitempty"`
	// ExtraArgs is the request's own "args" list, the ones appended after
	// a [harness.<name>] table's fixed command and args when a pty pane
	// is created by naming a harness instead of cmd_argv. It is empty for
	// a pane created with cmd_argv directly. pane.resume's pty path
	// appends this after a harness's resume_args, so a resumed pane keeps
	// whatever the pane was originally started with (a task prompt, say),
	// not only the harness's own resume flags.
	ExtraArgs []string `json:"extra_args,omitempty"`
	// ParentPane is the id of the pane this one was forked from, or empty.
	// A pane keeps it after the parent closes. It is a fact about how the
	// pane began, not a live link.
	ParentPane string `json:"parent_pane,omitempty"`
	// ForkPending is true from the fork until the adapter reports a session
	// id that differs from the borrowed one in HarnessSessionID. While it is
	// set, a restart repeats the fork from the parent's session instead of
	// resuming the parent's session in this pane, and the pane cannot be
	// forked again.
	ForkPending bool `json:"fork_pending,omitempty"`
	// TaskID is the task this pane works for, or empty.
	TaskID string `json:"task_id,omitempty"`
}

// Task is the unit of work above the pane. A task with Parent set is a
// child. A task with children and no pane of its own is a team. Worktree is
// the directory of the task's git worktree, or empty. Cwd is where a pane
// born into the task runs when there is no worktree, or empty.
type Task struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Cwd      string `json:"cwd,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Parent   string `json:"parent,omitempty"`
	Model    string `json:"model,omitempty"`
	// Foreman is the id of the pane that hears this task's asks first, or
	// empty. Only the operator sets it.
	Foreman  string   `json:"foreman,omitempty"`
	Children []string `json:"children"`
}

type Tab struct {
	ID        string   `json:"id"`
	Workspace string   `json:"workspace"`
	Label     string   `json:"label"`
	PaneIDs   []string `json:"pane_ids"`
}

type Workspace struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Cwd       string   `json:"cwd"`
	TabIDs    []string `json:"tab_ids"`
	NextPane  int      `json:"next_pane"`
	NextTabNo int      `json:"next_tab"`
}

type snapshot struct {
	V          int                   `json:"v"`
	Workspaces map[string]*Workspace `json:"workspaces"`
	Tabs       map[string]*Tab       `json:"tabs"`
	Panes      map[string]*Pane      `json:"panes"`
	Tasks      map[string]*Task      `json:"tasks"`
	NextWS     int                   `json:"next_workspace"`
	NextTask   int                   `json:"next_task"`
	CurrentWS  string                `json:"current_workspace"`
	CurrentTab string                `json:"current_tab"`
}

type Tree struct {
	mu   sync.RWMutex
	snap snapshot
}

func New() *Tree {
	return &Tree{snap: snapshot{
		V:          schemaVersion,
		Workspaces: map[string]*Workspace{},
		Tabs:       map[string]*Tab{},
		Panes:      map[string]*Pane{},
		Tasks:      map[string]*Task{},
		NextWS:     1,
		NextTask:   1,
	}}
}

func (t *Tree) CreateWorkspace(label, cwd string) Workspace {
	t.mu.Lock()
	defer t.mu.Unlock()
	ws := &Workspace{
		ID: fmt.Sprintf("w%d", t.snap.NextWS), Label: label, Cwd: cwd,
		NextPane: 1, NextTabNo: 1,
	}
	t.snap.NextWS++
	t.snap.Workspaces[ws.ID] = ws
	if t.snap.CurrentWS == "" {
		t.snap.CurrentWS = ws.ID
	}
	return *ws
}

func (t *Tree) CreateTab(wsID, label string) (Tab, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ws, ok := t.snap.Workspaces[wsID]
	if !ok {
		return Tab{}, fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	tab := &Tab{ID: fmt.Sprintf("%s:t%d", ws.ID, ws.NextTabNo), Workspace: ws.ID, Label: label}
	ws.NextTabNo++
	ws.TabIDs = append(ws.TabIDs, tab.ID)
	t.snap.Tabs[tab.ID] = tab
	if t.snap.CurrentTab == "" {
		t.snap.CurrentTab = tab.ID
	}
	return *tab, nil
}

func (t *Tree) CreatePane(wsID, tabID string, p Pane) (Pane, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ws, ok := t.snap.Workspaces[wsID]
	if !ok {
		return Pane{}, fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	tab, ok := t.snap.Tabs[tabID]
	if !ok || tab.Workspace != wsID {
		return Pane{}, fmt.Errorf("%w: %s", ErrNoTab, tabID)
	}
	p.ID = fmt.Sprintf("%s:p%d", ws.ID, ws.NextPane)
	ws.NextPane++
	p.Workspace = ws.ID
	p.Tab = tab.ID
	if p.Cols == 0 {
		p.Cols = 120
	}
	if p.Rows == 0 {
		p.Rows = 40
	}
	tab.PaneIDs = append(tab.PaneIDs, p.ID)
	// Store a clone, not p itself: p.Env/Argv/ExitCode came in by reference
	// from the caller, and aliasing them into the tree would make CreatePane
	// a second mutator, exactly what UpdatePane is supposed to be the only
	// one of. Return a further clone so the caller's copy and the stored
	// record never share memory either.
	stored := clonePane(&p)
	t.snap.Panes[p.ID] = &stored
	return clonePane(&stored), nil
}

// clonePane deep-copies the one field that is a reference type, so a caller
// cannot reach back into the tree through the map it was handed.
func clonePane(p *Pane) Pane {
	out := *p
	if p.Env != nil {
		out.Env = make(map[string]string, len(p.Env))
		for k, v := range p.Env {
			out.Env[k] = v
		}
	}
	if p.Argv != nil {
		out.Argv = append([]string(nil), p.Argv...)
	}
	if p.ExtraArgs != nil {
		out.ExtraArgs = append([]string(nil), p.ExtraArgs...)
	}
	if p.ExitCode != nil {
		c := *p.ExitCode
		out.ExitCode = &c
	}
	return out
}

// Pane returns a COPY. Handing out the live record would let a handler mutate
// it while Save marshals the same pointer under RLock, which is a data race.
func (t *Tree) Pane(id string) (Pane, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return Pane{}, false
	}
	return clonePane(p), true
}

// UpdatePane is the only way to change a pane. The callback runs under the
// write lock, so no reader sees a half-applied change.
func (t *Tree) UpdatePane(id string, f func(*Pane)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoPane, id)
	}
	f(p)
	return nil
}

func (t *Tree) Workspace(id string) (Workspace, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	w, ok := t.snap.Workspaces[id]
	if !ok {
		return Workspace{}, false
	}
	out := *w
	out.TabIDs = append([]string(nil), w.TabIDs...)
	return out, true
}

func (t *Tree) Tab(id string) (Tab, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	tb, ok := t.snap.Tabs[id]
	if !ok {
		return Tab{}, false
	}
	out := *tb
	out.PaneIDs = append([]string(nil), tb.PaneIDs...)
	return out, true
}

func (t *Tree) Panes() []Pane {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Pane, 0, len(t.snap.Panes))
	for _, p := range t.snap.Panes {
		out = append(out, clonePane(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *Tree) Workspaces() []Workspace {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Workspace, 0, len(t.snap.Workspaces))
	for _, w := range t.snap.Workspaces {
		c := *w
		c.TabIDs = append([]string(nil), w.TabIDs...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *Tree) Tabs(wsID string) ([]Tab, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ws, ok := t.snap.Workspaces[wsID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	out := make([]Tab, 0, len(ws.TabIDs))
	for _, id := range ws.TabIDs {
		if tb, ok := t.snap.Tabs[id]; ok {
			c := *tb
			c.PaneIDs = append([]string(nil), tb.PaneIDs...)
			out = append(out, c)
		}
	}
	return out, nil
}

func (t *Tree) Current() (string, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snap.CurrentWS, t.snap.CurrentTab
}

func (t *Tree) SetCurrent(wsID, tabID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.snap.Workspaces[wsID]; !ok {
		return fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	tab, ok := t.snap.Tabs[tabID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTab, tabID)
	}
	// Each id existing on its own is not enough: a tab from a different
	// workspace would otherwise become "current" for a workspace it was
	// never created under.
	if tab.Workspace != wsID {
		return fmt.Errorf("%w: %s belongs to workspace %s, not %s", ErrNoTab, tabID, tab.Workspace, wsID)
	}
	t.snap.CurrentWS, t.snap.CurrentTab = wsID, tabID
	return nil
}

// ClosePane marks a pane ended: its process is gone, on its own or because a
// restart found nothing behind the record. It keeps the record, so it can
// list under `ended: true` until pane.forget, pane.resume or the seven-day
// sweep takes it. endedAt is a unix second; it is stamped onto the record
// only the first time this runs for it, so a later call (watchExit racing a
// stale tick, say) cannot push a record's Recent age forward. An operator
// pane.close does not come through here at all - see RemovePane.
func (t *Tree) ClosePane(id string, exit *int, endedAt int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoPane, id)
	}
	p.Closed = true
	if p.EndedAt == 0 {
		p.EndedAt = endedAt
	}
	// Copy, don't alias: p.ExitCode = exit would let the caller's own *int
	// keep changing the tree's record after ClosePane returns, the same bug
	// CreatePane had with its Env/Argv/ExitCode inputs.
	if exit != nil {
		c := *exit
		p.ExitCode = &c
	}
	return nil
}

// RemovePane drops a record outright: a spawn the server could never start a
// process behind, or a pane an operator forgot, resumed or closed. ClosePane
// is different - it keeps the record so it can answer pane_closed, or list
// under `ended: true`, until something removes it. Once this runs, a stale
// id for this pane answers no_such_pane, same as one that never existed. The
// workspace counter is left alone, so the removed number is never handed out
// again.
func (t *Tree) RemovePane(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoPane, id)
	}
	if tab, ok := t.snap.Tabs[p.Tab]; ok {
		kept := tab.PaneIDs[:0]
		for _, pid := range tab.PaneIDs {
			if pid != id {
				kept = append(kept, pid)
			}
		}
		tab.PaneIDs = kept
	}
	delete(t.snap.Panes, id)
	return nil
}

// Save writes the tree atomically at mode 0600. A half-written layout after a
// crash would restore a tree that never existed. The snapshot is marshaled
// under RLock, then written to a temp file in the same directory and synced
// to disk before the rename, so a crash mid-write never leaves layout.json
// holding a partial write in place of the old, valid one.
func (t *Tree) Save(path string) error {
	t.mu.RLock()
	b, err := json.MarshalIndent(t.snap, "", "  ")
	t.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".layout-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// The rename itself must reach disk too, or a crash right after it can
	// still leave the directory entry pointing at the old file (or nothing)
	// on some filesystems. Fsync-ing the parent directory is how POSIX makes
	// a rename durable; there's nothing left to clean up here on failure,
	// since the rename already succeeded and the new content is in place.
	dirF, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirF.Close()
	return dirF.Sync()
}

// maxNumericSuffix returns the highest N found across ids of the form
// "<prefix><N>" (e.g. prefix "w1:p" over the set {"w1:p1", "w1:p2", "w2:p1"}
// finds 2, from "w1:p2"), and whether any id in the set matched the prefix at
// all. An id that doesn't parse as "<prefix><digits>" is ignored rather than
// treated as an error: a malformed file should recover a usable counter for
// the ids that do parse, not fail Load outright.
func maxNumericSuffix(ids []string, prefix string) (max int, found bool) {
	for _, id := range ids {
		rest, ok := strings.CutPrefix(id, prefix)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		found = true
		if n > max {
			max = n
		}
	}
	return max, found
}

func Load(path string) (*Tree, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("layout file is not readable: %w", err)
	}
	if s.Workspaces == nil {
		s.Workspaces = map[string]*Workspace{}
	}
	if s.Tabs == nil {
		s.Tabs = map[string]*Tab{}
	}
	if s.Panes == nil {
		s.Panes = map[string]*Pane{}
	}
	if s.Tasks == nil {
		s.Tasks = map[string]*Task{}
	}
	if s.NextWS == 0 {
		s.NextWS = len(s.Workspaces) + 1
	}
	taskIDs := make([]string, 0, len(s.Tasks))
	for id := range s.Tasks {
		taskIDs = append(taskIDs, id)
	}
	if max, found := maxNumericSuffix(taskIDs, "t"); found && s.NextTask <= max {
		s.NextTask = max + 1
	} else if s.NextTask == 0 {
		s.NextTask = 1
	}
	if s.V == 0 {
		s.V = schemaVersion
	}
	if s.V > schemaVersion {
		return nil, fmt.Errorf(
			"layout file is schema v%d, newer than this build's v%d. Update coppice, or move the file aside and start fresh",
			s.V, schemaVersion)
	}
	// A normal Save always persists counters past every id already handed
	// out, so this only matters for a hand-edited or otherwise malformed
	// file. A zero or stale counter must never be defaulted to 1 when panes
	// (or tabs) already exist for the workspace: that would make CreatePane
	// hand out an id that already exists, silently clobbering the record it
	// names. The repair scans for the true highest existing suffix and only
	// raises the counter past it. A counter already higher than every
	// existing id is left alone.
	paneIDs := make([]string, 0, len(s.Panes))
	for id := range s.Panes {
		paneIDs = append(paneIDs, id)
	}
	tabIDs := make([]string, 0, len(s.Tabs))
	for id := range s.Tabs {
		tabIDs = append(tabIDs, id)
	}
	for _, ws := range s.Workspaces {
		if max, found := maxNumericSuffix(paneIDs, ws.ID+":p"); found && ws.NextPane <= max {
			ws.NextPane = max + 1
		} else if ws.NextPane == 0 {
			ws.NextPane = 1
		}
		if max, found := maxNumericSuffix(tabIDs, ws.ID+":t"); found && ws.NextTabNo <= max {
			ws.NextTabNo = max + 1
		} else if ws.NextTabNo == 0 {
			ws.NextTabNo = 1
		}
	}
	return &Tree{snap: s}, nil
}

// cloneTask copies the one reference field, so a caller cannot reach back
// into the tree through the children slice it was handed.
func cloneTask(tk *Task) Task {
	out := *tk
	out.Children = append([]string{}, tk.Children...)
	return out
}

// CreateTask stores t under a fresh id. Parent must name an existing task or
// be empty. Children on the way in are ignored: a task starts with none.
func (t *Tree) CreateTask(tk Task) (Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var parent *Task
	if tk.Parent != "" {
		p, ok := t.snap.Tasks[tk.Parent]
		if !ok {
			return Task{}, fmt.Errorf("%w: %s", ErrNoTask, tk.Parent)
		}
		parent = p
	}
	tk.ID = fmt.Sprintf("t%d", t.snap.NextTask)
	t.snap.NextTask++
	tk.Children = []string{}
	stored := cloneTask(&tk)
	t.snap.Tasks[tk.ID] = &stored
	if parent != nil {
		parent.Children = append(parent.Children, tk.ID)
	}
	return cloneTask(&stored), nil
}

// Task returns a copy of one task.
func (t *Tree) Task(id string) (Task, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	tk, ok := t.snap.Tasks[id]
	if !ok {
		return Task{}, false
	}
	return cloneTask(tk), true
}

// Tasks returns copies of every task, sorted by id.
func (t *Tree) Tasks() []Task {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Task, 0, len(t.snap.Tasks))
	for _, tk := range t.snap.Tasks {
		out = append(out, cloneTask(tk))
	}
	sort.Slice(out, func(i, j int) bool { return taskLess(out[i].ID, out[j].ID) })
	return out
}

// taskLess orders task ids by number, so t10 sorts after t9.
func taskLess(a, b string) bool {
	na, errA := strconv.Atoi(strings.TrimPrefix(a, "t"))
	nb, errB := strconv.Atoi(strings.TrimPrefix(b, "t"))
	if errA != nil || errB != nil {
		return a < b
	}
	return na < nb
}

// isDescendant reports whether id is under root. Caller holds the lock.
// seen stops the walk on a file whose children form a cycle.
func (t *Tree) isDescendant(root, id string) bool {
	return t.descends(root, id, map[string]bool{})
}

func (t *Tree) descends(root, id string, seen map[string]bool) bool {
	if seen[root] {
		return false
	}
	seen[root] = true
	tk, ok := t.snap.Tasks[root]
	if !ok {
		return false
	}
	for _, c := range tk.Children {
		if c == id || t.descends(c, id, seen) {
			return true
		}
	}
	return false
}

// dropChild removes id from the parent's children. Caller holds the lock.
func (t *Tree) dropChild(parentID, id string) {
	p, ok := t.snap.Tasks[parentID]
	if !ok {
		return
	}
	kept := p.Children[:0]
	for _, c := range p.Children {
		if c != id {
			kept = append(kept, c)
		}
	}
	p.Children = kept
}

// SetForeman names the pane that hears task id's asks first. An empty
// pane clears it.
func (t *Tree) SetForeman(id, pane string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.snap.Tasks[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTask, id)
	}
	tk.Foreman = pane
	return nil
}

// DropForeman clears Foreman on every task that names pane, for a pane
// that closed.
func (t *Tree) DropForeman(pane string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tk := range t.snap.Tasks {
		if tk.Foreman == pane {
			tk.Foreman = ""
		}
	}
}

// MoveTask puts a task under a new parent. An empty parent detaches it.
// Moving a task under itself or one of its descendants is ErrCycle.
func (t *Tree) MoveTask(id, parent string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.snap.Tasks[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTask, id)
	}
	if parent != "" {
		if _, ok := t.snap.Tasks[parent]; !ok {
			return fmt.Errorf("%w: %s", ErrNoTask, parent)
		}
		if parent == id || t.isDescendant(id, parent) {
			return fmt.Errorf("%w: %s under %s", ErrCycle, id, parent)
		}
	}
	if tk.Parent == parent {
		return nil
	}
	if tk.Parent != "" {
		t.dropChild(tk.Parent, id)
	}
	tk.Parent = parent
	if parent != "" {
		p := t.snap.Tasks[parent]
		p.Children = append(p.Children, id)
	}
	return nil
}

// CloseTask removes a task and every descendant, and clears TaskID on each
// pane that worked for them. The panes themselves stay: closing a task is
// the server's job, and it closes panes before it calls this.
func (t *Tree) CloseTask(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.snap.Tasks[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTask, id)
	}
	if tk.Parent != "" {
		t.dropChild(tk.Parent, id)
	}
	gone := map[string]bool{}
	t.collect(id, gone)
	for tid := range gone {
		delete(t.snap.Tasks, tid)
	}
	for _, p := range t.snap.Panes {
		if gone[p.TaskID] {
			p.TaskID = ""
		}
	}
	return nil
}

// collect marks id and every descendant in gone. Caller holds the lock.
func (t *Tree) collect(id string, gone map[string]bool) {
	gone[id] = true
	tk, ok := t.snap.Tasks[id]
	if !ok {
		return
	}
	for _, c := range tk.Children {
		if !gone[c] {
			t.collect(c, gone)
		}
	}
}

// Subtree returns copies of the task and every descendant, the task first,
// then each child's subtree in order.
func (t *Tree) Subtree(id string) ([]Task, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.snap.Tasks[id]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoTask, id)
	}
	var out []Task
	seen := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		tk, ok := t.snap.Tasks[id]
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, cloneTask(tk))
		for _, c := range tk.Children {
			walk(c)
		}
	}
	walk(id)
	return out, nil
}
