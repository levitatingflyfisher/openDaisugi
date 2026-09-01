# Plan 18: Tasks, Teams, and Trees Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A task is the unit of work above the pane: a name, a worktree on real disk, a parent, children, and a model or none. State bubbles up. The tree is visible in text and as a graph.

**Architecture:** The layout tree gains a `Task` record; panes carry a `TaskID`. A `Task` with `Parent` set is a child; a task with no pane and children is a team. Worktrees are created with `git worktree add` beside the repo under `<repo>-worktrees/<task>`. Task state is the worst of its panes and children, in the order needs you, working, idle, done. Verbs: `task.create|list|close|move`. The TUI renders `tree` as a text tree; the web renders it as a draggable graph in plan 22.

**Tech Stack:** Go 1.26, git.

**Spec:** ROADMAP plan 18; floor board "tasks and trees"; the conversation rules on teams and foremen.

## Global Constraints

Same as plan 12. Tests create worktrees inside `t.TempDir()` repos initialised with `git init`, never in the project repo.

---

### Task 1: The task record and state bubbling

**Files:**
- Modify: `harness/coppice/internal/layout/layout.go` (`Task struct{ID, Label, Worktree, Parent, Model string; Children []string}`, `Pane.TaskID`, `CreateTask`, `Tasks`, `Task(id)`, `MoveTask`, `CloseTask`)
- Create: `harness/coppice/internal/layout/bubble.go` (`func Worst(states []string) string`, `func TaskState(t Task, tree *Tree, paneState func(string) string) string`)
- Test: `harness/coppice/internal/layout/task_test.go`

**Interfaces:**
- Produces: `Worst` orders `blocked > working > idle > done > ""`; `TaskState` folds panes and children recursively; a task cycle is refused by `MoveTask` with `ErrCycle`.

- [ ] **Step 1: Write the failing tests**

```go
func TestNeedsYouBeatsWorkingWhenBubbling(t *testing.T) {
	if Worst([]string{"working", "blocked", "done"}) != "blocked" { t.Fatal("blocked must win") }
	if Worst([]string{"done", "idle"}) != "idle" { t.Fatal("idle beats done") }
}

func TestATeamsStateIsItsChildrensWorst(t *testing.T) {
	tr := New()
	team := tr.CreateTask(Task{Label: "review-team"})
	a := tr.CreateTask(Task{Label: "a", Parent: team.ID})
	b := tr.CreateTask(Task{Label: "b", Parent: team.ID})
	pa := mustPane(t, tr, a.ID); pb := mustPane(t, tr, b.ID)
	st := map[string]string{pa: "working", pb: "blocked"}
	if got := TaskState(tr.MustTask(team.ID), tr, func(id string) string { return st[id] }); got != "blocked" {
		t.Fatalf("team state %q", got)
	}
}

func TestMoveRefusesACycle(t *testing.T) {
	tr := New()
	a := tr.CreateTask(Task{Label: "a"}); b := tr.CreateTask(Task{Label: "b", Parent: a.ID})
	if err := tr.MoveTask(a.ID, b.ID); !errors.Is(err, ErrCycle) { t.Fatalf("%v", err) }
}
```

- [ ] **Step 2: Run**: `cd harness/coppice && go test -p 1 ./internal/layout -v`. Expected: FAIL.
- [ ] **Step 3: Implement**. `Save` and `Load` carry tasks in `layout.json` under `"tasks"`; old files without the key load with none.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/layout
git commit -m "coppice/layout: tasks above panes, teams above tasks, state bubbles up"
```

---

### Task 2: Worktrees beside the repo

**Files:**
- Create: `harness/coppice/internal/worktree/worktree.go`
- Test: `harness/coppice/internal/worktree/worktree_test.go`

**Interfaces:**
- Produces: `func Add(repo, name string) (path string, err error)` running `git -C repo worktree add -b <name> <repo>-worktrees/<name>`; `func Remove(repo, name string, keep bool) error`; `func Status(path string) (ahead, behind int, branch string, err error)`. Names are restricted to `[a-z0-9-]` and refused otherwise with `task names use lowercase letters, digits, and dashes`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAddCreatesAWorktreeBesideTheRepo(t *testing.T) {
	repo := initRepo(t) // git init + one commit, in t.TempDir()
	p, err := Add(repo, "gate-refactor")
	if err != nil { t.Fatal(err) }
	if filepath.Dir(p) != repo+"-worktrees" { t.Fatalf("not beside the repo: %s", p) }
	if _, err := os.Stat(filepath.Join(p, ".git")); err != nil { t.Fatal("no worktree") }
}

func TestBadNamesAreRefusedWithATeachingError(t *testing.T) {
	if _, err := Add(initRepo(t), "Gate Refactor"); err == nil || !strings.Contains(err.Error(), "lowercase") { t.Fatalf("%v", err) }
}

func TestStatusCountsAhead(t *testing.T) {
	repo := initRepo(t); p, _ := Add(repo, "x")
	commitFile(t, p, "a.txt")
	ahead, _, branch, err := Status(p)
	if err != nil || ahead != 1 || branch != "x" { t.Fatalf("%d %s %v", ahead, branch, err) }
}
```

- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement** with `os/exec` and `git`. Never `/tmp`: the path is derived from the repo path.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/worktree
git commit -m "coppice/worktree: one worktree per task, beside the repo, on real disk"
```

---

### Task 3: The task verbs

**Files:**
- Create: `harness/coppice/internal/server/tasks.go` (`task.create {label, parent?, cwd?, worktree: bool}`, `task.list` with bubbled state, `task.close {task, keep_worktree: bool}`, `task.move {task, parent}`)
- Modify: `harness/coppice/internal/server/panes.go` (`pane.create` accepts `task`; a pane created with `task` runs in that task's worktree)
- Modify: `harness/coppice/PROTOCOL.md`, `harness/coppice/testdata/protocol/tasks.jsonl`
- Test: `harness/coppice/internal/server/tasks_test.go`

- [ ] **Step 1: Write the failing tests**: creating a task with `worktree: true` in a temp repo yields a worktree path in the result; a pane created with `task` has that path as `cwd`; `task.list` shows `state: "blocked"` for a task whose pane reports blocked; `task.close` with `keep_worktree: true` closes panes and leaves the directory.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./...`: PASS, corpus included.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/tasks.go harness/coppice/internal/server/tasks_test.go harness/coppice/internal/server/panes.go harness/coppice/PROTOCOL.md harness/coppice/testdata/protocol/tasks.jsonl
git commit -m "coppice: task verbs, and a pane born into a task runs in its worktree"
```

---

### Task 4: The tree in text

**Files:**
- Create: `harness/coppice/internal/tui/tree.go` (`RenderTree(tasks, panes, cols) []string` with box-drawing, state dot, ahead count)
- Modify: `harness/coppice/internal/tui/prompt.go` (`tree` opens it; Enter on a task opens that task's roster; Esc returns)
- Modify: `harness/coppice/internal/cli/verbs.go` (`coppice task list --tree` prints the same lines)
- Test: `harness/coppice/internal/tui/tree_test.go`

- [ ] **Step 1: Write the failing test**: three tasks, one with two panes, render to lines that contain `├─` and `└─`, the task label, the worktree name, and `● needs you` on the right task; width never exceeds `cols`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/tree.go harness/coppice/internal/tui/tree_test.go harness/coppice/internal/tui/prompt.go harness/coppice/internal/cli/verbs.go
git commit -m "coppice: the tree in text, one word from the roster"
```

---

### Task 5: The Python client speaks tasks

**Files:**
- Modify: `src/opendaisugi/floor/backend.py` (`PaneBackend.spawn(..., task: str | None = None)`; `list_tasks()`), `src/opendaisugi/floor/coppice_backend.py`, `src/opendaisugi/floor/herdr_backend.py` and `tmux_backend.py` (tasks unsupported: `list_tasks` returns `[]`, `task=` raises `NotImplementedError` with the backend's name)
- Modify: `src/opendaisugi/cli.py` (`daisugi coppice task create|list|close`)
- Modify: `docs/plans/2026-09-08-workshop/00-master-spec.md` §3.2 (the added parameter)
- Test: `tests/floor/test_backend_contract.py`, `tests/floor/test_cli_coppice.py`

- [ ] **Step 1: Write the failing tests**: the contract test asserts `list_tasks()` returns a list on every backend; the coppice sandbox test creates a task and a pane in it and reads the pane's cwd.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `uv run --no-sync pytest -q tests/floor && uv run --no-sync ruff check .`: PASS.
- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/floor src/opendaisugi/cli.py tests/floor docs/plans/2026-09-08-workshop/00-master-spec.md
git commit -m "floor: tasks in the backend protocol and the daisugi coppice verbs"
```
