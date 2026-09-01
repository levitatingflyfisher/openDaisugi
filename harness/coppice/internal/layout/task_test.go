package layout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustTask creates a task or fails the test.
func mustTask(t *testing.T, tr *Tree, task Task) Task {
	t.Helper()
	got, err := tr.CreateTask(task)
	if err != nil {
		t.Fatalf("CreateTask(%+v): %v", task, err)
	}
	return got
}

// mustPane creates a pane that belongs to the task and returns its id. The
// first call on a tree makes one workspace and one tab for all later calls.
func mustPane(t *testing.T, tr *Tree, taskID string) string {
	t.Helper()
	wsID, tabID := tr.Current()
	if wsID == "" {
		ws := tr.CreateWorkspace("w", "/")
		tab, err := tr.CreateTab(ws.ID, "main")
		if err != nil {
			t.Fatal(err)
		}
		wsID, tabID = ws.ID, tab.ID
	}
	p, err := tr.CreatePane(wsID, tabID, Pane{Label: "p", Cwd: "/", Kind: KindHeadless, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func TestNeedsYouBeatsWorkingWhenBubbling(t *testing.T) {
	if Worst([]string{"working", "blocked", "done"}) != "blocked" {
		t.Fatal("blocked must win")
	}
	if Worst([]string{"done", "idle"}) != "idle" {
		t.Fatal("idle beats done")
	}
	if Worst([]string{"done", "unknown"}) != "unknown" {
		t.Fatal("unknown beats done")
	}
	if Worst([]string{"unknown", "idle"}) != "idle" {
		t.Fatal("idle beats unknown")
	}
	if Worst(nil) != "" {
		t.Fatal("nothing folds to the empty state")
	}
}

func TestATeamsStateIsItsChildrensWorst(t *testing.T) {
	tr := New()
	team := mustTask(t, tr, Task{Label: "review-team"})
	a := mustTask(t, tr, Task{Label: "a", Parent: team.ID})
	b := mustTask(t, tr, Task{Label: "b", Parent: team.ID})
	pa := mustPane(t, tr, a.ID)
	pb := mustPane(t, tr, b.ID)
	st := map[string]string{pa: "working", pb: "blocked"}
	look := func(id string) string { return st[id] }
	teamNow, ok := tr.Task(team.ID)
	if !ok {
		t.Fatal("the team is gone")
	}
	if got := TaskState(teamNow, tr, look); got != "blocked" {
		t.Fatalf("team state %q, want blocked", got)
	}
	aNow, _ := tr.Task(a.ID)
	if got := TaskState(aNow, tr, look); got != "working" {
		t.Fatalf("task a state %q, want working", got)
	}
	empty := mustTask(t, tr, Task{Label: "empty"})
	if got := TaskState(empty, tr, look); got != "" {
		t.Fatalf("an empty task has state %q, want none", got)
	}
}

func TestTaskIDsCountUpFromOne(t *testing.T) {
	tr := New()
	a := mustTask(t, tr, Task{Label: "a"})
	b := mustTask(t, tr, Task{Label: "b", Parent: a.ID})
	if a.ID != "t1" || b.ID != "t2" {
		t.Fatalf("ids = %q %q, want t1 / t2", a.ID, b.ID)
	}
	aNow, _ := tr.Task(a.ID)
	if len(aNow.Children) != 1 || aNow.Children[0] != b.ID {
		t.Fatalf("parent children = %v, want [%s]", aNow.Children, b.ID)
	}
	all := tr.Tasks()
	if len(all) != 2 || all[0].ID != "t1" || all[1].ID != "t2" {
		t.Fatalf("Tasks() = %+v", all)
	}
	// The copy handed out must not reach the tree.
	all[0].Children[0] = "zz"
	aNow, _ = tr.Task(a.ID)
	if aNow.Children[0] != b.ID {
		t.Fatal("Tasks() aliased the stored children slice")
	}
}

func TestUnknownParentIsRefused(t *testing.T) {
	tr := New()
	if _, err := tr.CreateTask(Task{Label: "a", Parent: "t9"}); !errors.Is(err, ErrNoTask) {
		t.Fatalf("CreateTask with a missing parent = %v, want ErrNoTask", err)
	}
	if len(tr.Tasks()) != 0 {
		t.Fatal("a refused task was stored")
	}
}

func TestMoveRefusesACycle(t *testing.T) {
	tr := New()
	a := mustTask(t, tr, Task{Label: "a"})
	b := mustTask(t, tr, Task{Label: "b", Parent: a.ID})
	c := mustTask(t, tr, Task{Label: "c", Parent: b.ID})
	if err := tr.MoveTask(a.ID, b.ID); !errors.Is(err, ErrCycle) {
		t.Fatalf("move a under b = %v, want ErrCycle", err)
	}
	if err := tr.MoveTask(a.ID, c.ID); !errors.Is(err, ErrCycle) {
		t.Fatalf("move a under its grandchild = %v, want ErrCycle", err)
	}
	if err := tr.MoveTask(a.ID, a.ID); !errors.Is(err, ErrCycle) {
		t.Fatalf("move a under itself = %v, want ErrCycle", err)
	}
	if err := tr.MoveTask(a.ID, "t9"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("move under a missing parent = %v, want ErrNoTask", err)
	}
	if err := tr.MoveTask("t9", a.ID); !errors.Is(err, ErrNoTask) {
		t.Fatalf("move a missing task = %v, want ErrNoTask", err)
	}
}

func TestMoveReparentsAndDetaches(t *testing.T) {
	tr := New()
	a := mustTask(t, tr, Task{Label: "a"})
	b := mustTask(t, tr, Task{Label: "b"})
	c := mustTask(t, tr, Task{Label: "c", Parent: a.ID})
	if err := tr.MoveTask(c.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	aNow, _ := tr.Task(a.ID)
	bNow, _ := tr.Task(b.ID)
	cNow, _ := tr.Task(c.ID)
	if len(aNow.Children) != 0 || len(bNow.Children) != 1 || bNow.Children[0] != c.ID || cNow.Parent != b.ID {
		t.Fatalf("after move: a=%+v b=%+v c=%+v", aNow, bNow, cNow)
	}
	if err := tr.MoveTask(c.ID, ""); err != nil {
		t.Fatal(err)
	}
	bNow, _ = tr.Task(b.ID)
	cNow, _ = tr.Task(c.ID)
	if len(bNow.Children) != 0 || cNow.Parent != "" {
		t.Fatalf("after detach: b=%+v c=%+v", bNow, cNow)
	}
}

func TestCloseTaskRemovesTheTeamAndClearsItsPanes(t *testing.T) {
	tr := New()
	root := mustTask(t, tr, Task{Label: "root"})
	team := mustTask(t, tr, Task{Label: "team", Parent: root.ID})
	a := mustTask(t, tr, Task{Label: "a", Parent: team.ID})
	b := mustTask(t, tr, Task{Label: "b", Parent: team.ID})
	other := mustTask(t, tr, Task{Label: "other", Parent: root.ID})
	pa := mustPane(t, tr, a.ID)
	pb := mustPane(t, tr, b.ID)
	po := mustPane(t, tr, other.ID)
	if err := tr.CloseTask(team.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{team.ID, a.ID, b.ID} {
		if _, ok := tr.Task(id); ok {
			t.Fatalf("task %s survived CloseTask", id)
		}
	}
	rootNow, _ := tr.Task(root.ID)
	if len(rootNow.Children) != 1 || rootNow.Children[0] != other.ID {
		t.Fatalf("root children = %v, want [%s]", rootNow.Children, other.ID)
	}
	for _, id := range []string{pa, pb} {
		p, _ := tr.Pane(id)
		if p.TaskID != "" {
			t.Fatalf("pane %s still names task %q", id, p.TaskID)
		}
	}
	if p, _ := tr.Pane(po); p.TaskID != other.ID {
		t.Fatalf("an unrelated pane lost its task: %+v", p)
	}
	if err := tr.CloseTask(team.ID); !errors.Is(err, ErrNoTask) {
		t.Fatalf("second close = %v, want ErrNoTask", err)
	}
}

func TestTasksRoundTripThroughSaveAndLoad(t *testing.T) {
	tr := New()
	team := mustTask(t, tr, Task{Label: "team", Cwd: "/repo", Model: "opus"})
	a := mustTask(t, tr, Task{Label: "a", Parent: team.ID, Worktree: "/repo-worktrees/a"})
	pa := mustPane(t, tr, a.ID)
	path := filepath.Join(t.TempDir(), "layout.json")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	gotTeam, ok := loaded.Task(team.ID)
	if !ok || gotTeam.Label != "team" || gotTeam.Cwd != "/repo" || gotTeam.Model != "opus" || len(gotTeam.Children) != 1 {
		t.Fatalf("loaded team = %+v", gotTeam)
	}
	gotA, ok := loaded.Task(a.ID)
	if !ok || gotA.Parent != team.ID || gotA.Worktree != "/repo-worktrees/a" {
		t.Fatalf("loaded a = %+v", gotA)
	}
	if p, _ := loaded.Pane(pa); p.TaskID != a.ID {
		t.Fatalf("loaded pane = %+v, want TaskID %s", p, a.ID)
	}
	next := mustTask(t, loaded, Task{Label: "next"})
	if next.ID != "t3" {
		t.Fatalf("id after load = %q, want t3", next.ID)
	}
}

func TestLayoutWithoutTasksLoadsWithNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layout.json")
	raw := `{"v":1,"workspaces":{},"tabs":{},"panes":{},"next_workspace":1}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Tasks(); len(got) != 0 {
		t.Fatalf("Tasks() = %+v, want none", got)
	}
	if first := mustTask(t, loaded, Task{Label: "first"}); first.ID != "t1" {
		t.Fatalf("first id = %q, want t1", first.ID)
	}
}

func TestLoadRepairsTheTaskCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layout.json")
	raw := `{"v":1,"workspaces":{},"tabs":{},"panes":{},"next_workspace":1,` +
		`"tasks":{"t4":{"id":"t4","label":"old"}},"next_task":0}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustTask(t, loaded, Task{Label: "new"}); got.ID != "t5" {
		t.Fatalf("id after repair = %q, want t5", got.ID)
	}
}

func TestPaneWithoutATaskWritesNoTaskKey(t *testing.T) {
	tr := New()
	mustPane(t, tr, "")
	path := filepath.Join(t.TempDir(), "layout.json")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"task_id"`) {
		t.Fatalf("a pane with no task wrote task_id:\n%s", raw)
	}
}

func TestSubtreeListsTheTaskThenItsDescendants(t *testing.T) {
	tr := New()
	root := mustTask(t, tr, Task{Label: "root"})
	a := mustTask(t, tr, Task{Label: "a", Parent: root.ID})
	b := mustTask(t, tr, Task{Label: "b", Parent: a.ID})
	mustTask(t, tr, Task{Label: "other"})
	got, err := tr.Subtree(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != root.ID || got[1].ID != a.ID || got[2].ID != b.ID {
		t.Fatalf("Subtree = %+v", got)
	}
	if _, err := tr.Subtree("t9"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("Subtree of a missing task = %v, want ErrNoTask", err)
	}
}

// A hand-edited layout with a parent cycle must not hang any walk.
func TestACyclicLayoutFileDoesNotHangTheWalks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layout.json")
	raw := `{"v":1,"workspaces":{},"tabs":{},"panes":{},"next_workspace":1,` +
		`"tasks":{"t1":{"id":"t1","label":"a","parent":"t2","children":["t2"]},` +
		`"t2":{"id":"t2","label":"b","parent":"t1","children":["t1"]}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.Tasks(); len(got) != 2 {
		t.Fatalf("Tasks() = %+v", got)
	}
	t1, _ := tr.Task("t1")
	if got := TaskState(t1, tr, func(string) string { return "working" }); got != "" {
		t.Fatalf("TaskState on a cycle with no panes = %q", got)
	}
	if err := tr.MoveTask("t1", "t2"); !errors.Is(err, ErrCycle) {
		t.Fatalf("MoveTask into the cycle = %v, want ErrCycle", err)
	}
	if sub, err := tr.Subtree("t1"); err != nil || len(sub) != 2 {
		t.Fatalf("Subtree = %+v, %v", sub, err)
	}
	if err := tr.CloseTask("t1"); err != nil {
		t.Fatal(err)
	}
	if got := tr.Tasks(); len(got) != 0 {
		t.Fatalf("after CloseTask: %+v", got)
	}
}
