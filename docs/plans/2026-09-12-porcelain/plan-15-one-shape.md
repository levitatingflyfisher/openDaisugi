# Plan 15: One Shape Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Terminal and browser show the same shape: the roster plus as many live tiles as the width allows, with tiles that never rearrange themselves, three layouts, swap, reset, lock, and the mouse on every row and tile.

**Architecture:** Tiles are a pure model, `internal/tiles` in Go and `tiles.js` in the web static files, with the same operations and the same tests in both languages. A pane has at most one tile; a tile shows exactly one pane. The TUI splits the screen when it is wide enough and shows one live tile through the frame stream from `pane.attach` in a non-interactive mode. The web roster screen becomes the floor page: rail plus tiles.

**Tech Stack:** Go 1.26, vanilla ES modules, `node --test`.

**Spec:** ROADMAP plan 15 and the rules "one shape" and "a pane has at most one tile"; floor board "laptop floor"; the tmux `swap-pane` reading in the conversation record.

## Global Constraints

Same as plan 12. JS tests run through `go test ./internal/web` which shells to `node --test`; run them directly with `node --test "harness/coppice/internal/web/static/_tests/*.test.mjs"`.

---

### Task 1: The tiles model in Go

**Files:**
- Create: `harness/coppice/internal/tiles/tiles.go`
- Test: `harness/coppice/internal/tiles/tiles_test.go`

**Interfaces:**
- Produces:

```go
type Layout string // "all" | "focus" | "one"
type Tiles struct { Slots []string; Focus int; Layout Layout; Locked bool }
func New(layout Layout, n int) *Tiles
func (t *Tiles) Fill(pane string)            // into the focused slot, or the first empty one
func (t *Tiles) Open(pane string) int        // append a new slot, return its index
func (t *Tiles) CloseSlot(i int)             // never closes a pane
func (t *Tiles) Swap(i, j int) error         // refused when Locked
func (t *Tiles) Reset(order []string)        // roster order into slots
func (t *Tiles) Focused() string
func (t *Tiles) SlotOf(pane string) (int, bool)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestFillReplacesTheFocusedSlotOnly(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1"); ts.Focus = 1; ts.Fill("p2"); ts.Focus = 0
	ts.Fill("p3")
	if ts.Slots[0] != "p3" || ts.Slots[1] != "p2" {
		t.Fatalf("slots %v", ts.Slots)
	}
}

func TestAPaneHasAtMostOneTile(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1"); ts.Focus = 1
	ts.Fill("p1")
	if n := count(ts.Slots, "p1"); n != 1 {
		t.Fatalf("p1 in %d slots", n)
	}
}

func TestSwapKeepsFocusWithThePane(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1"); ts.Focus = 1; ts.Fill("p2"); ts.Focus = 0
	if err := ts.Swap(0, 1); err != nil { t.Fatal(err) }
	if ts.Focused() != "p1" || ts.Slots[1] != "p1" {
		t.Fatalf("focus did not follow: %v focus=%d", ts.Slots, ts.Focus)
	}
}

func TestLockedRefusesSwap(t *testing.T) {
	ts := New("focus", 2); ts.Locked = true
	if err := ts.Swap(0, 1); err == nil { t.Fatal("swap while locked") }
}

func TestEmptySlotWinsOverFocus(t *testing.T) {
	ts := New("focus", 2)
	ts.Fill("p1")
	ts.Fill("p2")
	if ts.Slots[1] != "p2" || ts.Slots[0] != "p1" {
		t.Fatalf("empty slot not used: %v", ts.Slots)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**: `cd harness/coppice && go test -p 1 ./internal/tiles -v`. Expected: FAIL.
- [ ] **Step 3: Implement** `tiles.go` to the interface. `Layout "all"` means `Fill` always appends; `"one"` means one slot.
- [ ] **Step 4: Run the tests**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tiles
git commit -m "coppice/tiles: slots that never rearrange themselves"
```

---

### Task 2: The same model in JavaScript

**Files:**
- Create: `harness/coppice/internal/web/static/tiles.js`
- Test: `harness/coppice/internal/web/static/_tests/tiles.test.mjs`

- [ ] **Step 1: Write the failing tests**: the five cases from Task 1, in `node:test` form, against `export function newTiles(layout, n)`, `fill`, `open`, `closeSlot`, `swap`, `reset`, `focused`, `slotOf`.
- [ ] **Step 2: Run**: `node --test harness/coppice/internal/web/static/_tests/tiles.test.mjs`. Expected: FAIL.
- [ ] **Step 3: Implement** `tiles.js` as a pure module, no DOM.
- [ ] **Step 4: Run**: PASS, and `cd harness/coppice && go test -p 1 ./internal/web` still green.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static/tiles.js harness/coppice/internal/web/static/_tests/tiles.test.mjs
git commit -m "coppice/web: the tiles model, same rules as the Go one"
```

---

### Task 3: The floor page: rail plus tiles

**Files:**
- Modify: `harness/coppice/internal/web/static/roster.js` (render the rail groups; a row click fills the focused tile; middle click or ctrl click opens a tile)
- Create: `harness/coppice/internal/web/static/floor.js` (mounts tiles, each a canvas driven by `grid.js` and `pane.js` frames; header per tile with state dot and an amber ask bar)
- Modify: `harness/coppice/internal/web/static/app.js` (default route mounts the floor; `#/pane/<id>` still opens one pane full screen)
- Modify: `harness/coppice/internal/web/static/index.html`, `app.css`
- Test: `harness/coppice/internal/web/static/_tests/floor.test.mjs` using `browser-stub.mjs`

**Interfaces:**
- Consumes: `tiles.js`, `sortPanes` and `paneRows` from `roster.js`, `applyFrame` and `drawGrid` from `grid.js`.
- Produces: `mountFloor(root, state)`; `tileCount(widthPx)` returning 1 under 900 px, 2 under 1400, else 3.

- [ ] **Step 1: Write the failing tests**

```js
test('a row click fills the focused tile and keeps the other', () => {
  const { doc, state } = stub({ panes: [p('a','blocked'), p('b','working'), p('c','working')] , width: 1500 });
  mountFloor(doc.body, state);
  click(doc, '[data-row="c"]');
  assert.deepEqual(tilesOf(doc), ['c', 'b', '']);   // a was focused, replaced by c
});

test('middle click opens a new tile', () => {
  const { doc, state } = stub({ panes: [p('a','working'), p('b','working')], width: 1500 });
  mountFloor(doc.body, state);
  middleClick(doc, '[data-row="b"]');
  assert.equal(tilesOf(doc).filter(Boolean).length, 2);
});

test('narrow widths get one tile', () => {
  assert.equal(tileCount(800), 1);
  assert.equal(tileCount(1200), 2);
  assert.equal(tileCount(1600), 3);
});

test('an ask shows as an amber bar on the tile, never a dialog', () => {
  const { doc, state } = stub({ panes: [p('a','blocked', { summary: 'git push --force', gate: { verdict: 'deny', rule: 3 } })], width: 1500 });
  mountFloor(doc.body, state);
  assert.ok(doc.querySelector('[data-tile="a"] .askbar').textContent.includes('rule 3'));
  assert.equal(doc.querySelector('dialog'), null);
});
```

- [ ] **Step 2: Run**: `node --test harness/coppice/internal/web/static/_tests/floor.test.mjs`. Expected: FAIL.
- [ ] **Step 3: Implement** `floor.js` and the roster changes. Tile headers show the state dot, label, harness, worktree. The ask bar shows the summary, the verdict, and two keys, `n deny` and `r reply`, that send `agent.deny` or open the reply box. Layout choice and lock persist in `localStorage` under `coppice.floor`.
- [ ] **Step 4: Run** all JS tests and `go test -p 1 ./internal/web`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static/roster.js harness/coppice/internal/web/static/floor.js harness/coppice/internal/web/static/app.js harness/coppice/internal/web/static/index.html harness/coppice/internal/web/static/app.css harness/coppice/internal/web/static/_tests/floor.test.mjs
git commit -m "coppice/web: the floor page, a rail and as many tiles as the width allows"
```

---

### Task 4: The terminal split, one live tile beside the roster

**Files:**
- Modify: `harness/coppice/internal/tui/render.go` (when `cols >= 120`, the roster takes the left 45 percent and the right shows the focused pane's grid)
- Modify: `harness/coppice/internal/tui/run.go` (subscribe to frames for the focused pane with `pane.attach` in view-only mode: keys are not forwarded; use `events.pause` on the previous pane when focus moves)
- Modify: `harness/coppice/internal/server/attach.go` (`pane.attach` accepts `"view_only": true`, which streams frames and refuses input with `not_attached`)
- Test: `harness/coppice/internal/tui/split_test.go`, `harness/coppice/internal/server/viewonly_test.go`

- [ ] **Step 1: Write the failing tests**: at 140 columns the render has a vertical divider and the right side contains the pane's text; at 80 columns there is no divider. Server: a `view_only` attach that then sends `pane.send_text` on that connection gets `not_attached`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. The right-side grid is rendered from the same `pane.Grid` snapshot format the web uses; reuse `attach/render.go`'s row drawing.
- [ ] **Step 4: Run**: `cd harness/coppice && go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/render.go harness/coppice/internal/tui/run.go harness/coppice/internal/tui/split_test.go harness/coppice/internal/server/attach.go harness/coppice/internal/server/viewonly_test.go harness/coppice/PROTOCOL.md
git commit -m "coppice/tui: a wide terminal shows the focused pane live beside the roster"
```

---

### Task 5: Mouse on rows and tiles in the terminal

**Files:**
- Modify: `harness/coppice/internal/tui/run.go` (enable SGR mouse reporting `\x1b[?1000h\x1b[?1006h` on start, disable on exit; parse `\x1b[<b;x;yM`)
- Create: `harness/coppice/internal/tui/mouse.go`
- Test: `harness/coppice/internal/tui/mouse_test.go`

- [ ] **Step 1: Write the failing test**: feeding the bytes of a left click at row 3 selects the row rendered at line 3; a middle click on a row opens a second slot when `cols >= 120`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement** `mouse.go` with a pure parser `ParseSGR(b []byte) (Click, int)` and a hit test `HitRow(m *Model, y int) (rowIndex int, ok bool)` driven by the last render's row map.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/mouse.go harness/coppice/internal/tui/mouse_test.go harness/coppice/internal/tui/run.go
git commit -m "coppice/tui: the mouse works on every row and tile"
```

---

### Task 6: Swap, reset, lock, and the layout word on the prompt

**Files:**
- Modify: `harness/coppice/internal/tui/prompt.go` (`swap 2 4`, `rotate`, `reset`, `lock`, `unlock`, `layout all|focus|one`)
- Modify: `harness/coppice/internal/tui/run.go` (persist layout and lock in `<data dir>/floor.json`)
- Test: `harness/coppice/internal/tui/prompt_test.go`

- [ ] **Step 1: Write the failing tests**: `swap 1 2` exchanges slots and focus follows the pane; `reset` orders slots by the roster; `lock` makes `swap` answer `Layout is locked. Type unlock first.`; `layout all` gives every pane a slot.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement** through `tiles.Tiles`.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/prompt.go harness/coppice/internal/tui/prompt_test.go harness/coppice/internal/tui/run.go
git commit -m "coppice/tui: swap, reset, lock, and layouts as words on the prompt"
```
