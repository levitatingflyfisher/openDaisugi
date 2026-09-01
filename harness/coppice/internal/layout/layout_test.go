package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var osStat = os.Stat

func statFile(path string) (uint32, error) {
	fi, err := osStat(path)
	if err != nil {
		return 0, err
	}
	return uint32(fi.Mode().Perm()), nil
}

func TestPaneIDsAreWorkspaceScopedAndStable(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, err := tr.CreateTab(ws.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	p1, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID != "w1" || p1.ID != "w1:p1" || p2.ID != "w1:p2" {
		t.Fatalf("ids = %q %q %q, want w1 / w1:p1 / w1:p2", ws.ID, p1.ID, p2.ID)
	}
	// A closed pane must not free its number. A client holding "w1:p1" would
	// otherwise start talking to a different process.
	if err := tr.ClosePane(p1.ID, nil, 1000); err != nil {
		t.Fatal(err)
	}
	p3, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if p3.ID != "w1:p3" {
		t.Fatalf("a reused pane number: got %q, want w1:p3", p3.ID)
	}
}

func TestSecondWorkspaceGetsItsOwnPaneNumbering(t *testing.T) {
	tr := New()
	a := tr.CreateWorkspace("a", "/a")
	b := tr.CreateWorkspace("b", "/b")
	ta, _ := tr.CreateTab(a.ID, "t")
	tb, _ := tr.CreateTab(b.ID, "t")
	pa, _ := tr.CreatePane(a.ID, ta.ID, Pane{Cwd: "/a", Kind: KindPTY})
	pb, _ := tr.CreatePane(b.ID, tb.ID, Pane{Cwd: "/b", Kind: KindPTY})
	if pa.ID != "w1:p1" || pb.ID != "w2:p1" {
		t.Fatalf("ids = %q %q, want w1:p1 / w2:p1", pa.ID, pb.ID)
	}
}

func TestUnknownIDsReturnTypedErrors(t *testing.T) {
	tr := New()
	if _, err := tr.CreateTab("w9", "x"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("CreateTab on a missing workspace = %v, want ErrNoWorkspace", err)
	}
	ws := tr.CreateWorkspace("main", "/repo")
	if _, err := tr.CreatePane(ws.ID, "t9", Pane{}); !errors.Is(err, ErrNoTab) {
		t.Fatalf("CreatePane on a missing tab = %v, want ErrNoTab", err)
	}
	if err := tr.ClosePane("w1:p9", nil, 1000); !errors.Is(err, ErrNoPane) {
		t.Fatalf("ClosePane on a missing pane = %v, want ErrNoPane", err)
	}
}

func TestCurrentDefaultsToTheFirstWorkspaceAndTab(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	gotWS, gotTab := tr.Current()
	if gotWS != ws.ID || gotTab != tab.ID {
		t.Fatalf("Current() = %q %q, want %q %q", gotWS, gotTab, ws.ID, tab.ID)
	}
}

func TestSaveAndLoadRestoreTheTreeAndTheCounters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")

	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{
		Cwd: "/repo/sub", Label: "auth fix", Argv: []string{"claude"},
		Env: map[string]string{"FOO": "bar"}, Kind: KindPTY, Cols: 120, Rows: 40,
		HarnessSessionID: "abc",
	})
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.Pane(p.ID)
	if !ok {
		t.Fatalf("pane %s did not survive the round trip", p.ID)
	}
	if got.Cwd != "/repo/sub" || got.Label != "auth fix" || got.HarnessSessionID != "abc" ||
		got.Cols != 120 || got.Rows != 40 || got.Env["FOO"] != "bar" {
		t.Fatalf("restored pane = %+v, want the saved fields", got)
	}
	// The counter must survive too, or a restart hands out an id a client
	// already holds.
	next, err := back.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:p2" {
		t.Fatalf("post-restore pane id = %q, want w1:p2", next.ID)
	}
}

// The tree hands out copies. A caller that mutates what it got must not be
// able to change the tree, or Save marshals records while a handler edits them
// and go test -race fails. Label/Cwd are plain strings and would pass even
// with a shallow `*p` copy; Env, Argv, ExitCode and the id slices are the
// reference-typed fields where a shallow copy would actually leak.
func TestPaneAccessorsReturnCopies(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	exit := 0
	inArgv := []string{"sh"}
	inEnv := map[string]string{"FOO": "bar"}
	created, err := tr.CreatePane(ws.ID, tab.ID, Pane{
		Cwd: "/repo", Kind: KindPTY, Label: "before",
		Argv: inArgv, Env: inEnv, ExitCode: &exit,
	})
	if err != nil {
		t.Fatal(err)
	}
	// CreatePane must not alias the caller's own Env/Argv/ExitCode into the
	// tree either: mutating the map/slice/pointer the caller passed in, after
	// the call returns, must not reach the tree.
	inEnv["FOO"] = "leaked-via-create-input"
	inArgv[0] = "leaked-via-create-input"
	exit = 99
	if leaked, _ := tr.Pane(created.ID); leaked.Env["FOO"] != "bar" || leaked.Argv[0] != "sh" || *leaked.ExitCode != 0 {
		t.Fatalf("CreatePane aliased the caller's input: %+v", leaked)
	}

	p := created
	got, _ := tr.Pane(p.ID)
	got.Label = "mutated"
	got.Env["FOO"] = "mutated"
	got.Argv[0] = "mutated"
	*got.ExitCode = 99
	again, _ := tr.Pane(p.ID)
	if again.Label != "before" {
		t.Fatalf("mutating the returned pane changed the tree: label is %q", again.Label)
	}
	if again.Env["FOO"] != "bar" {
		t.Fatalf("mutating the returned pane's Env changed the tree: Env[FOO] is %q", again.Env["FOO"])
	}
	if again.Argv[0] != "sh" {
		t.Fatalf("mutating the returned pane's Argv changed the tree: Argv[0] is %q", again.Argv[0])
	}
	if *again.ExitCode != 0 {
		t.Fatalf("mutating the returned pane's ExitCode changed the tree: ExitCode is %d", *again.ExitCode)
	}

	list := tr.Panes()
	list[0].Cwd = "/elsewhere"
	list[0].Env["FOO"] = "mutated-via-list"
	if back, _ := tr.Pane(p.ID); back.Cwd != "/repo" || back.Env["FOO"] != "bar" {
		t.Fatalf("mutating a Panes() entry changed the tree: cwd is %q, Env[FOO] is %q", back.Cwd, back.Env["FOO"])
	}

	gotWS, _ := tr.Workspace(ws.ID)
	gotWS.TabIDs[0] = "mutated"
	if backWS, _ := tr.Workspace(ws.ID); backWS.TabIDs[0] != tab.ID {
		t.Fatalf("mutating Workspace().TabIDs changed the tree: %q", backWS.TabIDs[0])
	}

	gotTab, _ := tr.Tab(tab.ID)
	gotTab.PaneIDs[0] = "mutated"
	if backTab, _ := tr.Tab(tab.ID); backTab.PaneIDs[0] != p.ID {
		t.Fatalf("mutating Tab().PaneIDs changed the tree: %q", backTab.PaneIDs[0])
	}
}

// ClosePane must copy the exit code, the same way CreatePane copies its
// input: a caller mutating *exit after ClosePane returns must not reach the
// tree, or it races with Save's marshal under RLock.
func TestClosePaneCopiesTheExitCode(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})

	exit := 7
	if err := tr.ClosePane(p.ID, &exit, 1000); err != nil {
		t.Fatal(err)
	}
	exit = 99 // mutate the caller's own variable after ClosePane returns

	got, _ := tr.Pane(p.ID)
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("ClosePane aliased the caller's exit code pointer: got %v, want 7", got.ExitCode)
	}
}

// ClosePane stamps EndedAt the first time it runs for a record, and never
// again: a later call must not push a record's Recent age forward, or a
// pane that ended hours ago could keep dodging the seven-day sweep every
// time something re-closes an already-closed record.
func TestClosePaneStampsEndedAtOnlyOnce(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})

	if err := tr.ClosePane(p.ID, nil, 1000); err != nil {
		t.Fatal(err)
	}
	if err := tr.ClosePane(p.ID, nil, 2000); err != nil {
		t.Fatal(err)
	}
	got, _ := tr.Pane(p.ID)
	if got.EndedAt != 1000 {
		t.Fatalf("EndedAt = %d, want 1000 (the first close), not the second", got.EndedAt)
	}
	if !got.Closed {
		t.Fatal("ClosePane did not mark the record closed")
	}
}

func TestUpdatePaneIsTheOnlyWayToChangeOne(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})

	if err := tr.UpdatePane(p.ID, func(x *Pane) {
		x.Cols, x.Rows = 200, 60
		x.HarnessSessionID = "sess-9"
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := tr.Pane(p.ID)
	if got.Cols != 200 || got.Rows != 60 || got.HarnessSessionID != "sess-9" {
		t.Fatalf("UpdatePane did not take: %+v", got)
	}
	if err := tr.UpdatePane("w9:p9", func(*Pane) {}); !errors.Is(err, ErrNoPane) {
		t.Fatalf("UpdatePane on a missing pane = %v, want ErrNoPane", err)
	}
}

// Saving while handlers mutate panes is the shape the race detector catches.
func TestConcurrentUpdateAndSaveIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})

	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < 200; i++ {
			_ = tr.UpdatePane(p.ID, func(x *Pane) { x.Cols = 80 + i%40 })
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 200; i++ {
			_ = tr.Save(filepath.Join(dir, "layout.json"))
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	tr := New()
	tr.CreateWorkspace("main", "/repo")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := statFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info != 0o600 {
		t.Fatalf("layout file mode = %o, want 600", info)
	}

	// A successful Save must not leave its temp file behind.
	assertOnlyLayoutFileIn(t, dir, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory would not block the write, so the failure path below can't be exercised")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	tr.CreateWorkspace("second", "/second")
	if err := tr.Save(path); err == nil {
		t.Fatal("Save into a read-only directory unexpectedly succeeded")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a failed Save changed the previous layout.json")
	}
	assertOnlyLayoutFileIn(t, dir, path)
}

func assertOnlyLayoutFileIn(t *testing.T, dir, path string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatalf("directory has a stray file: %s (want only %s)", e.Name(), filepath.Base(path))
		}
	}
}

// Load repairs a missing (zero) per-workspace pane counter from the highest
// pane id actually present, not by defaulting to 1 - a default of 1 would
// hand out "w1:p1" again and silently overwrite the existing pane.
func TestLoadRepairsAMissingCounterFromTheHighestExistingId(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	s := snapshot{
		V: schemaVersion,
		Workspaces: map[string]*Workspace{
			"w1": {ID: "w1", Label: "main", Cwd: "/repo", TabIDs: []string{"w1:t1"}, NextPane: 0, NextTabNo: 2},
		},
		Tabs: map[string]*Tab{
			"w1:t1": {ID: "w1:t1", Workspace: "w1", Label: "work", PaneIDs: []string{"w1:p1", "w1:p2"}},
		},
		Panes: map[string]*Pane{
			"w1:p1": {ID: "w1:p1", Workspace: "w1", Tab: "w1:t1", Cwd: "/repo", Kind: KindPTY, Cols: 120, Rows: 40},
			"w1:p2": {ID: "w1:p2", Workspace: "w1", Tab: "w1:t1", Cwd: "/repo", Kind: KindPTY, Cols: 120, Rows: 40},
		},
		NextWS:     2,
		CurrentWS:  "w1",
		CurrentTab: "w1:t1",
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	tr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := tr.CreatePane("w1", "w1:t1", Pane{Cwd: "/repo", Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:p3" {
		t.Fatalf("next pane id = %q, want w1:p3 (repaired from the highest existing pane id)", next.ID)
	}
}

// A stale counter that is merely lower than the highest existing id (not
// zero) must be repaired the same way. A counter higher than the max is a
// different case and must be left alone (covered by
// TestSaveAndLoadRestoreTheTreeAndTheCounters's normal round trip).
func TestLoadRepairsAStaleCounterBelowTheHighestExistingId(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	panes := map[string]*Pane{}
	paneIDs := []string{}
	for i := 1; i <= 5; i++ {
		id := "w1:p" + string(rune('0'+i))
		panes[id] = &Pane{ID: id, Workspace: "w1", Tab: "w1:t1", Cwd: "/repo", Kind: KindPTY, Cols: 120, Rows: 40}
		paneIDs = append(paneIDs, id)
	}
	s := snapshot{
		V: schemaVersion,
		Workspaces: map[string]*Workspace{
			// A next_pane of 2 is stale: p1..p5 already exist.
			"w1": {ID: "w1", Label: "main", Cwd: "/repo", TabIDs: []string{"w1:t1"}, NextPane: 2, NextTabNo: 2},
		},
		Tabs: map[string]*Tab{
			"w1:t1": {ID: "w1:t1", Workspace: "w1", Label: "work", PaneIDs: paneIDs},
		},
		Panes:      panes,
		NextWS:     2,
		CurrentWS:  "w1",
		CurrentTab: "w1:t1",
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	tr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := tr.CreatePane("w1", "w1:t1", Pane{Cwd: "/repo", Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:p6" {
		t.Fatalf("next pane id = %q, want w1:p6 (repaired past the highest existing pane id, not the stale counter)", next.ID)
	}
}

// Load must normalize a missing or zero schema version, or a re-Save writes
// "v": 0 forever instead of ever recording the schema it is actually using.
func TestLoadNormalizesAMissingSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	s := snapshot{
		// V left at its zero value: a file saved before "v" existed, or one
		// hand-edited without it.
		Workspaces: map[string]*Workspace{"w1": {ID: "w1", Label: "main", Cwd: "/repo", NextPane: 1, NextTabNo: 1}},
		Tabs:       map[string]*Tab{},
		Panes:      map[string]*Pane{},
		NextWS:     2,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	tr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back snapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.V != schemaVersion {
		t.Fatalf("re-saved schema version = %d, want %d", back.V, schemaVersion)
	}
}

func TestSetCurrentChangesCurrentAndValidatesIDs(t *testing.T) {
	tr := New()
	wsA := tr.CreateWorkspace("a", "/a")
	tabA, _ := tr.CreateTab(wsA.ID, "ta")
	wsB := tr.CreateWorkspace("b", "/b")
	tabB, _ := tr.CreateTab(wsB.ID, "tb")

	if err := tr.SetCurrent(wsB.ID, tabB.ID); err != nil {
		t.Fatal(err)
	}
	gotWS, gotTab := tr.Current()
	if gotWS != wsB.ID || gotTab != tabB.ID {
		t.Fatalf("Current() = %q %q, want %q %q", gotWS, gotTab, wsB.ID, tabB.ID)
	}

	if err := tr.SetCurrent("w9", tabA.ID); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("SetCurrent with an unknown workspace = %v, want ErrNoWorkspace", err)
	}
	if err := tr.SetCurrent(wsA.ID, "w1:t9"); !errors.Is(err, ErrNoTab) {
		t.Fatalf("SetCurrent with an unknown tab = %v, want ErrNoTab", err)
	}
	// A rejected SetCurrent must not have partially applied.
	gotWS, gotTab = tr.Current()
	if gotWS != wsB.ID || gotTab != tabB.ID {
		t.Fatalf("a rejected SetCurrent changed Current(): got %q %q", gotWS, gotTab)
	}
}

// Workspaces() and Tabs() are the plural accessors: they must return copies,
// same as their singular counterparts, and in a deterministic order.
func TestWorkspacesAndTabsReturnCopiesAndAreSorted(t *testing.T) {
	tr := New()
	wsB := tr.CreateWorkspace("b", "/b") // w1
	wsA := tr.CreateWorkspace("a", "/a") // w2
	tab1, _ := tr.CreateTab(wsB.ID, "one")
	tab2, _ := tr.CreateTab(wsB.ID, "two")

	list := tr.Workspaces()
	if len(list) != 2 || list[0].ID != wsB.ID || list[1].ID != wsA.ID {
		t.Fatalf("Workspaces() = %+v, want sorted [%s %s]", list, wsB.ID, wsA.ID)
	}
	list[0].TabIDs[0] = "mutated"
	list[0].Label = "mutated"
	if back, _ := tr.Workspace(wsB.ID); back.TabIDs[0] != tab1.ID || back.Label != "b" {
		t.Fatalf("mutating a Workspaces() entry changed the tree: %+v", back)
	}

	tabs, err := tr.Tabs(wsB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 2 || tabs[0].ID != tab1.ID || tabs[1].ID != tab2.ID {
		t.Fatalf("Tabs() = %+v, want [%s %s] in creation order", tabs, tab1.ID, tab2.ID)
	}
	p, _ := tr.CreatePane(wsB.ID, tab1.ID, Pane{Cwd: "/b", Kind: KindPTY})
	tabs, err = tr.Tabs(wsB.ID)
	if err != nil {
		t.Fatal(err)
	}
	tabs[0].PaneIDs[0] = "mutated"
	tabs[0].Label = "mutated"
	if back, _ := tr.Tab(tab1.ID); back.PaneIDs[0] != p.ID || back.Label != "one" {
		t.Fatalf("mutating a Tabs() entry changed the tree: %+v", back)
	}

	if _, err := tr.Tabs("w9"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("Tabs on an unknown workspace = %v, want ErrNoWorkspace", err)
	}
}

// Load must refuse a layout.json written by a schema version this build
// does not know, the same rule a corrupt file already gets: return an
// error and touch nothing. Loading it anyway risks a re-Save that silently
// drops whatever a newer format added.
func TestLoadRefusesANewerSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	s := snapshot{
		V:          schemaVersion + 1,
		Workspaces: map[string]*Workspace{},
		Tabs:       map[string]*Tab{},
		Panes:      map[string]*Pane{},
		NextWS:     1,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a newer schema version, want a refusal")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(b) {
		t.Fatal("Load rewrote a layout.json it refused to load")
	}
}

// SetCurrent must refuse a tab id that exists but belongs to a different
// workspace than the one named: both ids passing their own independent
// existence check is not the same thing as the pair being a real
// workspace/tab combination.
func TestSetCurrentRefusesATabThatBelongsToAnotherWorkspace(t *testing.T) {
	tr := New()
	wsA := tr.CreateWorkspace("a", "/a")
	wsB := tr.CreateWorkspace("b", "/b")
	tabB, err := tr.CreateTab(wsB.ID, "only tab")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SetCurrent(wsA.ID, tabB.ID); err == nil {
		t.Fatalf("SetCurrent(%s, %s) succeeded, want a refusal: %s belongs to %s, not %s",
			wsA.ID, tabB.ID, tabB.ID, wsB.ID, wsA.ID)
	}
}

// The pane-counter repair test above (TestLoadRepairsAStaleCounterBelowTheHighestExistingId)
// has no tab-counter equivalent: this is that test, for NextTabNo instead
// of NextPane.
func TestLoadRepairsAStaleTabCounterBelowTheHighestExistingId(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	tabs := map[string]*Tab{
		"w1:t1": {ID: "w1:t1", Workspace: "w1", Label: "one"},
		"w1:t2": {ID: "w1:t2", Workspace: "w1", Label: "two"},
		"w1:t3": {ID: "w1:t3", Workspace: "w1", Label: "three"},
	}
	s := snapshot{
		V: schemaVersion,
		Workspaces: map[string]*Workspace{
			// A next_tab of 1 is stale: t1..t3 already exist.
			"w1": {ID: "w1", Label: "main", Cwd: "/repo", TabIDs: []string{"w1:t1", "w1:t2", "w1:t3"},
				NextPane: 1, NextTabNo: 1},
		},
		Tabs:       tabs,
		Panes:      map[string]*Pane{},
		NextWS:     2,
		CurrentWS:  "w1",
		CurrentTab: "w1:t1",
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	tr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := tr.CreateTab("w1", "four")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:t4" {
		t.Fatalf("next tab id = %q, want w1:t4 (repaired past the highest existing tab id, not the stale counter)", next.ID)
	}
}

// maxNumericSuffix parses the numeric tail with strconv.Atoi, so a
// double-digit id must compare as the number it is, not as a string ("w1:p9"
// sorting after "w1:p10" would repair the counter to the wrong value).
func TestLoadRepairsACounterPastADoubleDigitId(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	panes := map[string]*Pane{}
	var paneIDs []string
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("w1:p%d", i)
		panes[id] = &Pane{ID: id, Workspace: "w1", Tab: "w1:t1", Cwd: "/repo", Kind: KindPTY, Cols: 120, Rows: 40}
		paneIDs = append(paneIDs, id)
	}
	s := snapshot{
		V: schemaVersion,
		Workspaces: map[string]*Workspace{
			// A next_pane of 2 is stale and, read as a string, "w1:p9" would
			// wrongly sort after "w1:p10" - the repair must still land past 10.
			"w1": {ID: "w1", Label: "main", Cwd: "/repo", TabIDs: []string{"w1:t1"}, NextPane: 2, NextTabNo: 2},
		},
		Tabs: map[string]*Tab{
			"w1:t1": {ID: "w1:t1", Workspace: "w1", Label: "work", PaneIDs: paneIDs},
		},
		Panes:      panes,
		NextWS:     2,
		CurrentWS:  "w1",
		CurrentTab: "w1:t1",
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	tr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := tr.CreatePane("w1", "w1:t1", Pane{Cwd: "/repo", Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:p11" {
		t.Fatalf("next pane id = %q, want w1:p11 (past the double-digit id, numerically)", next.ID)
	}
}
