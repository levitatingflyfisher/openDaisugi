# Plan 22: The View Suite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Seven view plugins ship on by default, each small, each proving the socket is enough: tree (graph form), minimap, kanban, colony with foragers, shift log, a Herdr-shaped grid, and a lazybox-shaped inbox.

**Architecture:** Every view is a static page under `harness/coppice/plugins/<id>/` with one ES module of pure functions that turn `/api/panes`, `/api/tasks`, and the websocket event stream into a picture, and one small DOM file that draws it. Pure functions are tested with `node --test`. Each view reads `sel=` from the hash and writes it back on selection change, so selection carries across views. No view keeps state the socket cannot see: a kanban drag calls `task.move` or nothing.

**Tech Stack:** Vanilla ES modules, Canvas for the colony and the minimap, `node --test`.

**Spec:** ROADMAP plan 22; floor board "plugins" gallery; the rule "every mark means a state the socket reports".

## Global Constraints

Same as plan 12. No framework, no bundler, no external script. Each view's pure module is under 200 lines.

---

### Task 1: The shared view library

**Files:**
- Create: `harness/coppice/plugins/_lib/view.js` (`connect(token)` returning `{panes(), tasks(), onEvent(fn)}`; `selection.read()`, `selection.write(ids)`; `stateColor(state)` returning the same five tokens the roster uses; `age(seconds)`)
- Test: `harness/coppice/plugins/_lib/_tests/view.test.mjs`

- [ ] **Step 1: Write the failing tests**: `selection.read` parses `#/view/x?sel=a,b` into `["a","b"]`; `selection.write` rewrites the hash without dropping the view id; `stateColor("blocked")` is the amber token and `stateColor("nonsense")` throws, because a mark with no state is a lie.
- [ ] **Step 2: Run**: `node --test harness/coppice/plugins/_lib/_tests/view.test.mjs`. Expected: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS. Add the plugins test glob to `js_test.go` so `go test ./internal/web` runs these too.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins/_lib harness/coppice/internal/web/js_test.go
git commit -m "coppice/plugins: the small library every view shares"
```

---

### Task 2: Tree as a graph, and the minimap

**Files:**
- Create: `harness/coppice/plugins/tree/graph.js` (pure: `layout(tasks, panes) -> {nodes:[{id,x,y,state,label}], edges:[[a,b]]}` with a simple tidy tree layout) and `graph-view.js` (Canvas, drag to pan, click a node to select, double click to open `#/pane/<id>`)
- Create: `harness/coppice/plugins/minimap/manifest.json`, `minimap.js` (pure: `dots(tasks, panes, w, h)`), `index.html`
- Modify: `harness/coppice/internal/web/static/floor.js` (mount the minimap at the bottom of the rail when the plugin is enabled)
- Test: `harness/coppice/plugins/tree/_tests/graph.test.mjs`, `harness/coppice/plugins/minimap/_tests/minimap.test.mjs`

- [ ] **Step 1: Write the failing tests**: a team with two children lays out with the children below the parent and no overlapping nodes; the minimap yields one dot per node with the node's state color and a click on a dot's position returns its id.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins/tree harness/coppice/plugins/minimap harness/coppice/internal/web/static/floor.js
git commit -m "coppice/plugins: the tree as a graph, and a minimap that is the tree shrunk"
```

---

### Task 3: Kanban

**Files:**
- Create: `harness/coppice/plugins/kanban/manifest.json`, `kanban.js` (pure: `columns(tasks, panes)` returning `inbox, working, needs you, done`, where `inbox` is a task with no panes), `index.html`, `kanban-view.js` (drag a card between `inbox` and nothing else: the other columns are states the server owns, so a drop there is refused with a one-line toast `columns are states. prompt the pane to move it.`)
- Test: `harness/coppice/plugins/kanban/_tests/kanban.test.mjs`

- [ ] **Step 1: Write the failing tests**: a task with a blocked pane lands in `needs you`; a task with no panes lands in `inbox`; `allowedDrop("working")` is false and `allowedDrop("inbox")` is true.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins/kanban
git commit -m "coppice/plugins: a kanban whose columns are the four states and nothing else"
```

---

### Task 4: Colony

**Files:**
- Create: `harness/coppice/plugins/colony/manifest.json`, `colony.js` (pure: `place(panes, w, h, seed)` deterministic positions by pane id hash; `inBox(positions, box)` for drag-select; `bar(quietFor)` mapping seconds to a 0 to 1 length), `index.html`, `colony-view.js` (Canvas; drag a box to select; a prompt line at the bottom `N selected · say what they should do next` that sends `pane.send_text` to each; drop one forager on another calls `task.move`)
- Test: `harness/coppice/plugins/colony/_tests/colony.test.mjs`

- [ ] **Step 1: Write the failing tests**: placement is stable for the same ids; a box around two of three positions selects those two; the bar for `quietFor = 0` is 0 and for an hour is 1; dropping `a` on `b` yields a `task.move` request, never anything else.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins/colony
git commit -m "coppice/plugins: the colony, foragers you can box-select and speak to"
```

---

### Task 5: Shift log

**Files:**
- Create: `harness/coppice/plugins/shift-log/manifest.json`, `shift.js` (pure: `cells(events, now, minutes=80, cell=5)` folding `state` events into rows by pane and cells by five minutes with levels 0 to 3 for output, `ask`, and `deny`), `index.html`, `shift-view.js`
- Modify: `harness/coppice/internal/web/api.go` (`GET /api/events?since=<ts>` serving the server's in-memory ring of the last two hours of state events)
- Test: `harness/coppice/plugins/shift-log/_tests/shift.test.mjs`, `harness/coppice/internal/web/events_api_test.go`

- [ ] **Step 1: Write the failing tests**: two `working` events eleven minutes apart fill two cells with a gap between; a `blocked` event marks its cell `ask`; a `deny` in the detail marks `deny`; the API returns only events after `since`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins/shift-log harness/coppice/internal/web/api.go harness/coppice/internal/web/events_api_test.go
git commit -m "coppice/plugins: the shift log answers where the hour went"
```

---

### Task 6: The Herdr-shaped grid and the lazybox-shaped inbox

**Files:**
- Create: `harness/coppice/plugins/herdr-grid/` (a roster on the left with the state word per row, a grid of live canvases on the right for every pane, the Herdr shape; uses `tiles.js` with layout `all`)
- Create: `harness/coppice/plugins/inbox/` (rows are tasks with a worktree, sorted by needs you, with `ahead` and the PR number when `gh` reports one through `GET /api/tasks`; `!` jumps to the first row that needs you; a live canvas on the right for the selected row)
- Test: `harness/coppice/plugins/herdr-grid/_tests/grid.test.mjs`, `harness/coppice/plugins/inbox/_tests/inbox.test.mjs`

- [ ] **Step 1: Write the failing tests**: the grid gives every pane a slot; the inbox sorts blocked first and `jumpToNeed(rows)` returns the index of the first blocked row.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS. Then the whole suite: `cd harness/coppice && go test -p 1 ./...`.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins/herdr-grid harness/coppice/plugins/inbox
git commit -m "coppice/plugins: a Herdr-shaped grid and a lazybox-shaped inbox over the same socket"
```

---

### Task 7: The gallery on the field board

**Files:**
- Modify: `docs/research/boards/floor/body.html` (the plugins section links each shipped view by id and says each is on by default)

- [ ] **Step 1:** Edit, run `python3 docs/research/boards/_shell/render.py`, commit `docs/research/boards/floor`.
