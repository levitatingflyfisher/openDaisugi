# Plan 25: The Stack Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A foreman's children are one level down. Enter on a foreman or a team pushes into its roster, Esc pops back, and the breadcrumb in the bar says where you are. Attention bubbles up: a child's ask reaches its foreman first and you only when the foreman cannot answer or the tier is permanent. Proven at depth two before anything deeper.

**Architecture:** The tree from plan 18 already holds parents and children. This plan adds a cursor path to the TUI model and the floor page, a `scope` parameter to `pane.list` and `task.list` so a roster can show one subtree, and an escalation rule in the server: a `blocked` event on a pane whose task has a foreman is first delivered as a `note` to the foreman with a deadline; if the foreman does not answer within the deadline, or the tier is permanent, the ask surfaces to the top-level roster.

**Tech Stack:** Go 1.26, vanilla JS.

**Spec:** ROADMAP plan 25; the conversation record on the tree of foremen; research caution from Cognition on escalation judgment.

## Global Constraints

Same as plan 12. Depth is unbounded in the data model and tested at two. A permanent-tier ask always reaches a human, whatever the depth.

---

### Task 1: Scoped listing

**Files:**
- Modify: `harness/coppice/internal/server/tasks.go` and `panes.go` (`task.list {scope?}` and `pane.list {scope?}` return only the subtree under `scope`; the result carries `path`, the labels from the root to `scope`)
- Modify: `harness/coppice/PROTOCOL.md`, `harness/coppice/testdata/protocol/scope.jsonl`
- Test: `harness/coppice/internal/server/scope_test.go`

- [ ] **Step 1: Write the failing test**: a team with two child tasks each holding a pane; `pane.list {scope: <team>}` returns the two panes and `path: ["floor", "review-team"]`; `pane.list` with no scope returns all panes.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server harness/coppice/PROTOCOL.md harness/coppice/testdata/protocol/scope.jsonl
git commit -m "coppice: list one subtree, with the path that leads to it"
```

---

### Task 2: Push and pop in the terminal

**Files:**
- Modify: `harness/coppice/internal/tui/model.go` (`Path []string`; `Push(taskID)`, `Pop()`)
- Modify: `harness/coppice/internal/tui/run.go` (Enter on a task row or a foreman pane row pushes; Esc on a bare roster with a non-empty path pops; the bar reads `coppice › review-team · 1 needs you`)
- Test: `harness/coppice/internal/tui/stack_test.go`

- [ ] **Step 1: Write the failing test**: Enter on a team row makes the roster show only its children and the bar shows the breadcrumb; Esc returns to the full roster; the count in the bar is the scoped count while pushed.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui
git commit -m "coppice/tui: enter pushes into a team, esc pops, the bar shows the path"
```

---

### Task 3: Push and pop on the floor page

**Files:**
- Modify: `harness/coppice/internal/web/static/floor.js` (the rail shows the breadcrumb; clicking a team row scopes the rail and tiles; a `‹ back` row at the top pops; tiles keep their panes when the scope changes, because tiles are yours)
- Test: `harness/coppice/internal/web/static/_tests/stack.test.mjs`

- [ ] **Step 1: Write the failing test**: clicking a team scopes the rail to its children and leaves the tiles untouched; back restores the full rail.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static/floor.js harness/coppice/internal/web/static/_tests/stack.test.mjs
git commit -m "coppice/web: push into a team on the floor page without moving a tile"
```

---

### Task 4: Attention bubbles up through the foreman

**Files:**
- Create: `harness/coppice/internal/server/escalate.go` (on a `blocked` state for a pane whose task, or any ancestor task, has a `Model` foreman pane: send a `note` to the nearest foreman with the ask and a deadline of 120 seconds; mark the ask `held_by: <foreman>`; on the foreman answering through `agent.deny` or a reply, clear; on deadline, clear the hold and surface; a `permanent` tier is never held)
- Modify: `harness/coppice/internal/tui/model.go` and `floor.js` (a held ask renders under WORKING as `waiting on review-team's foreman · 1m` and not under NEEDS YOU)
- Test: `harness/coppice/internal/server/escalate_test.go`

- [ ] **Step 1: Write the failing tests**: an undoable ask on a child pane with a foreman is not in the top-level NEEDS YOU for 120 seconds and the foreman receives a note; after the deadline it surfaces; a permanent ask surfaces at once and the foreman still gets the note.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. The foreman answers a held ask with the commands it already has: `coppice agent prompt` to steer, or `coppice floor note deny <ask>`, which the server maps to `agent.deny` when the note comes from a pane connection. It never gets allow.
- [ ] **Step 4: Run**: `go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/escalate.go harness/coppice/internal/server/escalate_test.go harness/coppice/internal/tui/model.go harness/coppice/internal/web/static/floor.js
git commit -m "coppice: a child's ask goes to its foreman first, and a permanent one goes to you at once"
```

---

### Task 5: Depth two, proven by hand

- [ ] **Step 1:** With the config's foreman set, ask it in words to make a team of two shell panes under a task and to run a command in each. Confirm the tree, the push and pop, and a held ask from a child surfacing after the deadline.
- [ ] **Step 2:** Record in `docs/plans/2026-09-12-porcelain/RESUME.md` with the turn count the foreman used, so the cost the research called unmeasured gets its first number. Commit that file alone.
