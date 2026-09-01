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
	NextWS     int                   `json:"next_workspace"`
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
		NextWS:     1,
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

// ClosePane marks a pane closed. It keeps the record, so `pane list` can still
// show what ran and a client's stale id gets pane_closed rather than silence.
func (t *Tree) ClosePane(id string, exit *int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoPane, id)
	}
	p.Closed = true
	// Copy, don't alias: p.ExitCode = exit would let the caller's own *int
	// keep changing the tree's record after ClosePane returns, the same bug
	// CreatePane had with its Env/Argv/ExitCode inputs.
	if exit != nil {
		c := *exit
		p.ExitCode = &c
	}
	return nil
}

// RemovePane drops a record the server could not start a process behind.
// ClosePane keeps a record so a stale id answers pane_closed; a record that
// never had a process is different, since nothing ever ran and no client
// was ever told the id worked. The workspace counter is left alone, so the
// removed number is never handed out again.
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
	if s.NextWS == 0 {
		s.NextWS = len(s.Workspaces) + 1
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
