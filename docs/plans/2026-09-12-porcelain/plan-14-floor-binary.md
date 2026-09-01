# Plan 14: The Floor Binary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `coppice` with no arguments opens the terminal porcelain from the Go binary: the roster, the prompt line, peek, and attach. The Textual floor screen in Python is retired and `daisugi coppice floor` execs the binary.

**Architecture:** A new package `internal/tui` renders with the same raw-terminal approach `internal/attach` already uses: a screen model, a renderer that diffs rows, and a key loop. It talks to the server over the socket as any client would, so it exercises the protocol from plan 13. State comes from `pane.list` plus the event stream. The Python side keeps every plumbing verb and loses only the screen.

**Tech Stack:** Go 1.26, `golang.org/x/term` (already a dependency), no TUI framework.

**Spec:** ROADMAP plan 14; floor board views "cold start", "roster", "peek", "attached"; ROADMAP rules "one shape" and "six keys" (the keys themselves land in plan 17; this plan uses the existing bindings and a placeholder map that plan 17 replaces).

## Global Constraints

Same as plan 12. In addition: no new Go dependency. The TUI never runs in tests against a real harness; tests drive the screen model with fake pane lists and fake key bytes.

---

### Task 1: The screen model, pure

**Files:**
- Create: `harness/coppice/internal/tui/model.go`
- Test: `harness/coppice/internal/tui/model_test.go`

**Interfaces:**
- Produces: `type Row struct{ID, Label, Harness, Worktree, State, Source, Line string; Age, QuietFor float64}`; `type Model struct{Rows []Row; Cursor int; Peek *Peek; Prompt string; NeedYou int}`; `func Group(rows []Row) []Section` returning `NEEDS YOU`, `WORKING`, `DONE` in that order with rows sorted by age within a section; `func (m *Model) Apply(list []map[string]any, now float64)` and `func (m *Model) Event(ev map[string]any)`.

- [ ] **Step 1: Write the failing test**

```go
func TestRowsGroupByWhetherYouAreNeeded(t *testing.T) {
	rows := []Row{
		{ID: "a", State: "working"}, {ID: "b", State: "blocked", Age: 120},
		{ID: "c", State: "done"}, {ID: "d", State: "blocked", Age: 30},
	}
	secs := Group(rows)
	if secs[0].Title != "NEEDS YOU" || secs[0].Rows[0].ID != "d" || secs[0].Rows[1].ID != "b" {
		t.Fatalf("needs you not first and oldest last: %+v", secs)
	}
	if secs[1].Title != "WORKING" || secs[2].Title != "DONE" {
		t.Fatalf("section order wrong: %+v", secs)
	}
}

func TestNeedYouCountFollowsTheList(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"pane": "a", "state": "blocked"}, {"pane": "b", "state": "working"}}, 0)
	if m.NeedYou != 1 {
		t.Fatalf("NeedYou = %d", m.NeedYou)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd harness/coppice && go test -p 1 ./internal/tui -v`
Expected: FAIL, package missing.

- [ ] **Step 3: Implement** `model.go` with the types above. `Apply` maps `pane.list` rows to `Row` (state `blocked` means needs you; the ask summary or the last detail line is `Line`). `Event` updates one row from a `state` event.

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./internal/tui`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/model.go harness/coppice/internal/tui/model_test.go
git commit -m "coppice/tui: a screen model that groups rows by whether you are needed"
```

---

### Task 2: The renderer

**Files:**
- Create: `harness/coppice/internal/tui/render.go`
- Test: `harness/coppice/internal/tui/render_test.go`

**Interfaces:**
- Produces: `func Render(m *Model, cols, rows int) []string` returning exactly `rows` lines, each at most `cols` cells wide: a bar line (`coppice · N need you`), the sections, a footer with the key hints, and the prompt line last. The footer text comes from `Keys()` in `keys.go`, so plan 17 changes it in one place.

- [ ] **Step 1: Write the failing test**

```go
func TestRenderFitsTheScreenAndEndsWithThePrompt(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: "gate-refactor", Harness: "claude", State: "blocked", Line: "wants git push --force"}}, NeedYou: 1}
	lines := Render(m, 80, 12)
	if len(lines) != 12 {
		t.Fatalf("%d lines", len(lines))
	}
	if !strings.Contains(lines[0], "1 need you") {
		t.Fatalf("bar: %q", lines[0])
	}
	if !strings.HasPrefix(lines[11], "› ") {
		t.Fatalf("prompt not last: %q", lines[11])
	}
	for _, l := range lines {
		if width(l) > 80 {
			t.Fatalf("line too wide: %q", l)
		}
	}
}

func TestAnEmptyFloorSaysWhatToDo(t *testing.T) {
	lines := Render(&Model{}, 80, 6)
	if !strings.Contains(strings.Join(lines, "\n"), "Nothing running. Enter opens") {
		t.Fatal("empty state does not teach")
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd harness/coppice && go test -p 1 ./internal/tui -run TestRender -v`
Expected: FAIL.

- [ ] **Step 3: Implement** `render.go`. Reuse `attach/render.go`'s cell width helper by moving it to a small shared package `internal/textwidth` if it is unexported there. Colours: amber for `blocked`, blue for `working`, green for `done`, grey for `idle`, through the same escape helpers attach uses. The empty state line reads `Nothing running. Enter opens <default> here.` where `<default>` comes from the config in plan 16 and is `claude` until then.

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./internal/tui`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/render.go harness/coppice/internal/tui/render_test.go harness/coppice/internal/textwidth
git commit -m "coppice/tui: render the roster, the footer, and the prompt inside the screen"
```

---

### Task 3: Peek and the prompt line

**Files:**
- Modify: `harness/coppice/internal/tui/model.go` (`Peek` type: pane id, last lines, ask, gate verdict)
- Create: `harness/coppice/internal/tui/prompt.go`
- Test: `harness/coppice/internal/tui/prompt_test.go`

**Interfaces:**
- Produces: `func Classify(line string, harnesses []string) Kind` returning `Plumbing` when the first word is a verb (`list`, `read`, `close`, `open`, `swap`, `reset`) or a harness name, and `Talk` otherwise. `func (m *Model) OpenPeek(read string, ask map[string]any)`; `func (m *Model) ClosePeek()`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAHarnessNameIsPlumbingASentenceIsTalk(t *testing.T) {
	h := []string{"claude", "pi"}
	if Classify("claude", h) != Plumbing || Classify("pi --model llama", h) != Plumbing {
		t.Fatal("harness names must be plumbing")
	}
	if Classify("close docs", h) != Plumbing {
		t.Fatal("verbs must be plumbing")
	}
	if Classify("open a pane with pi on the docs", h) != Talk {
		t.Fatal("a sentence must be talk")
	}
}

func TestPeekRendersTheAskAndTheVerdictOnTheirOwnLines(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: "x", State: "blocked"}}}
	m.OpenPeek("line1\nline2", map[string]any{"summary": "git push --force", "gate": map[string]any{"verdict": "deny", "rule": 3}})
	out := strings.Join(Render(m, 80, 20), "\n")
	if !strings.Contains(out, "The gate says no") || !strings.Contains(out, "rule 3") {
		t.Fatalf("verdict missing: %s", out)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `cd harness/coppice && go test -p 1 ./internal/tui -run 'TestAHarnessName|TestPeekRenders' -v`
Expected: FAIL.

- [ ] **Step 3: Implement** `prompt.go` and the peek rendering. A `Talk` line with no foreman renders the one-line answer `No foreman. Enter opens <default> here, or type "foreman <harness>" to let it run the floor.` (the foreman itself is plan 19).

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./internal/tui`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/model.go harness/coppice/internal/tui/prompt.go harness/coppice/internal/tui/prompt_test.go harness/coppice/internal/tui/render.go
git commit -m "coppice/tui: peek shows the ask and the verdict, the prompt tells plumbing from talk"
```

---

### Task 4: The loop, wired to the socket

**Files:**
- Create: `harness/coppice/internal/tui/run.go`
- Modify: `harness/coppice/internal/cli/cli.go` (no args on a terminal runs the TUI; no args on a pipe prints the short help and exits 1)
- Test: `harness/coppice/internal/tui/run_test.go` (drives `Run` with a fake socket server from `internal/server` in-process and a scripted key stream; never a real TTY)

**Interfaces:**
- Consumes: `pane.list`, `pane.read`, `events.subscribe`, `pane.create`, `pane.close`, and `attach.Run` for Enter.
- Produces: `func Run(o Options) error` where `Options{Socket string; In io.Reader; Out io.Writer; Size func() (int, int)}`.

- [ ] **Step 1: Write the failing test**

```go
func TestSpaceOpensPeekAndEscClosesIt(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s, "sh", "-c", "echo hello; sleep 30")
	keys := scripted(" ", "\x1b", "q")
	var out bytes.Buffer
	if err := Run(Options{Socket: s.Socket(), In: keys, Out: &out, Size: func() (int, int) { return 80, 24 }}); err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out.String())
	if !strings.Contains(frames[1], "hello") {
		t.Fatal("peek did not show the pane's last lines")
	}
	if strings.Contains(frames[2], "hello") {
		t.Fatal("esc did not close the peek")
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd harness/coppice && go test -p 1 ./internal/tui -run TestSpaceOpensPeek -v`
Expected: FAIL.

- [ ] **Step 3: Implement** `run.go`: dial, `events.subscribe`, poll `pane.list` every two seconds and on every `state` event, read keys with `attach.ReadKeys`, and on Enter hand the terminal to `attach.Run` and re-render on return. `q` quits until plan 17 lands its map. Wire `cli.go`: `coppice` with no arguments and a terminal runs `tui.Run`; with a pipe it prints the short help and exits 1.

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/run.go harness/coppice/internal/tui/run_test.go harness/coppice/internal/cli/cli.go
git commit -m "coppice: no arguments opens the floor"
```

---

### Task 5: Retire the Python floor screen

**Files:**
- Delete: `src/opendaisugi/tui_floor.py`, `src/opendaisugi/tui_grid.py`
- Modify: `src/opendaisugi/tui.py` (remove the floor screen registration), `src/opendaisugi/cli.py` (`daisugi coppice floor` execs `coppice`)
- Delete tests: `tests/test_tui_floor.py`, `tests/floor/test_tui_grid.py`
- Modify: `tests/floor/test_cli_coppice.py` (exec test, same shape as plan 12 Task 4)
- Modify: `docs/reference/` page that documents the floor screen, if one exists (grep `tui_floor`)

- [ ] **Step 1: Write the failing test** for `daisugi coppice floor` exec, mirroring plan 12 Task 4.
- [ ] **Step 2: Run it**: `uv run --no-sync pytest -q tests/floor/test_cli_coppice.py -k floor_execs`. Expected: FAIL.
- [ ] **Step 3: Implement** the exec, delete the two modules and their tests, remove the import from `tui.py`.
- [ ] **Step 4: Run everything**: `uv run --no-sync pytest -q && uv run --no-sync ruff check .`. Expected: PASS. `tests/test_layer_boundary.py` must still pass.
- [ ] **Step 5: Commit**

```bash
git add -u src/opendaisugi/tui_floor.py src/opendaisugi/tui_grid.py tests/test_tui_floor.py tests/floor/test_tui_grid.py
git add src/opendaisugi/tui.py src/opendaisugi/cli.py tests/floor/test_cli_coppice.py
git commit -m "floor: the terminal porcelain lives in the coppice binary now"
```
