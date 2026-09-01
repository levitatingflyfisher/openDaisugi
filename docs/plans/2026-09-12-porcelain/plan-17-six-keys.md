# Plan 17: The Six Keys and One to Leave Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The roster answers to six keys only: ctrl-t, shift-tab, ctrl-w, Space, Enter, Esc. Inside a pane coppice steals exactly one key to leave. The ctrl-a leader vocabulary is removed. The footer prints the keys from one table.

**Architecture:** One key table in `internal/tui/keys.go` maps byte sequences to actions for the roster. The attach key reader keeps only the leave key, read from config, default ctrl-] (0x1D). Shift-tab arrives as `\x1b[Z`. ctrl-t is 0x14, ctrl-w is 0x17. The footer text is generated from the table so it can never drift from the bindings.

**Tech Stack:** Go 1.26.

**Spec:** ROADMAP plan 17 and "Decisions still open" (the leave key); floor board "Six keys"; the conversation rule that in-pane keys belong to the harness.

## Global Constraints

Same as plan 12. Decision recorded here and in the ROADMAP rules: the leave key defaults to ctrl-] because Claude Code binds shift-tab and ctrl-t, readline binds ctrl-w, and ctrl-] is the escape telnet and virsh console already teach, with no claim on it from Claude Code, Codex, or readline. Pressed twice it goes to the pane. The attach loop holds the first press for 300 ms; `Flush()` after that window returns `ActDetach`, the same shape the roster uses for a lone Esc. It is configurable as `[keys] leave = "ctrl-]"` in `coppice.toml`.

---

### Task 1: The roster key table

**Files:**
- Create: `harness/coppice/internal/tui/keys.go`
- Test: `harness/coppice/internal/tui/keys_test.go`

**Interfaces:**
- Produces:

```go
type Action string
const (
	Talk Action = "talk"      // ctrl-t
	NextNeed Action = "next"  // shift-tab: \x1b[Z
	CloseTile Action = "close" // ctrl-w
	Peek Action = "peek"      // space
	GoIn Action = "goin"      // enter
	Up Action = "up"          // esc alone (not an escape sequence prefix)
	None Action = ""
)
type Binding struct { Keys string; Action Action; Help string }
func Table() []Binding
func Keys() string                 // footer text, generated from Table()
type Reader struct{ ... }          // Feed(b byte) (Action, []byte) with a 30 ms esc timer resolved by Flush()
```

- [ ] **Step 1: Write the failing tests**

```go
func TestTheSixKeysMapAndNothingElseDoes(t *testing.T) {
	r := NewReader()
	cases := map[string]Action{"\x14": Talk, "\x1b[Z": NextNeed, "\x17": CloseTile, " ": Peek, "\r": GoIn}
	for seq, want := range cases {
		var got Action
		for _, b := range []byte(seq) { a, _ := r.Feed(b); if a != None { got = a } }
		if got != want { t.Fatalf("%q -> %v, want %v", seq, got, want) }
	}
	for _, b := range []byte("qnpcdx?") {
		if a, _ := r.Feed(b); a != None { t.Fatalf("%q must be text, got %v", b, a) }
	}
}

func TestLoneEscIsUpAfterFlush(t *testing.T) {
	r := NewReader()
	if a, _ := r.Feed(0x1b); a != None { t.Fatal("esc must wait for a sequence") }
	if a := r.Flush(); a != Up { t.Fatalf("flush gave %v", a) }
}

func TestFooterComesFromTheTable(t *testing.T) {
	f := Keys()
	for _, b := range Table() {
		if !strings.Contains(f, b.Help) { t.Fatalf("footer lacks %q", b.Help) }
	}
}
```

- [ ] **Step 2: Run**: `cd harness/coppice && go test -p 1 ./internal/tui -run 'TestTheSixKeys|TestLoneEsc|TestFooter' -v`. Expected: FAIL.
- [ ] **Step 3: Implement** `keys.go`. Any byte that is not a binding is text for the prompt line. `run.go` from plan 14 replaces its placeholder map with `Table()` and calls `Flush` on a 30 ms timer after a bare escape.
- [ ] **Step 4: Run**: `go test -p 1 ./internal/tui`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/keys.go harness/coppice/internal/tui/keys_test.go harness/coppice/internal/tui/run.go harness/coppice/internal/tui/render.go
git commit -m "coppice/tui: six keys, one table, a footer generated from it"
```

---

### Task 2: shift-tab cycles in attention order

**Files:**
- Modify: `harness/coppice/internal/tui/model.go` (`NextNeed()` moves the cursor to the next row in the NEEDS YOU section after the current one, wrapping; with none, to the next WORKING row)
- Test: `harness/coppice/internal/tui/model_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestNextNeedCyclesThroughAsksThenWorking(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "w", State: "working"}, {ID: "b1", State: "blocked", Age: 60}, {ID: "b2", State: "blocked", Age: 10}}}
	m.Regroup()
	m.NextNeed(); if m.Selected().ID != "b2" { t.Fatal("youngest ask first") }
	m.NextNeed(); if m.Selected().ID != "b1" { t.Fatal("then the older ask") }
	m.NextNeed(); if m.Selected().ID != "b2" { t.Fatal("wraps inside asks while any exist") }
}
```

- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/model.go harness/coppice/internal/tui/model_test.go
git commit -m "coppice/tui: shift-tab goes to the next one that needs you"
```

---

### Task 3: One key to leave a pane, and no leader

**Files:**
- Modify: `harness/coppice/internal/attach/keys.go` (delete the leader table; `KeyReader` takes a `leave byte` and returns `ActDetach` on it; every other byte forwards; the leave key pressed twice within 300 ms forwards one literal leave byte to the pane, the same escape tmux gives its prefix, so vim's tag jump and telnet's own escape stay reachable)
- Modify: `harness/coppice/internal/attach/attach.go` (`Options.Leave byte`; the status line reads `<label> · N need you · ctrl-] leave`)
- Modify: `harness/coppice/internal/config/config.go` (`Keys struct{ Leave string }`; `ParseKey("ctrl-]") (byte, error)`)
- Modify: `harness/coppice/internal/cli/cli.go` (pass the configured leave key)
- Test: `harness/coppice/internal/attach/keys_test.go` (rewrite), `harness/coppice/internal/config/keys_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestOnlyTheLeaveKeyIsStolen(t *testing.T) {
	k := NewKeyReader(0x1d)
	if a, fwd := k.Feed(0x01); a != ActNone || !bytes.Equal(fwd, []byte{0x01}) { t.Fatal("ctrl-a must forward now") }
	if a, fwd := k.Feed(0x1d); a != ActNone || fwd != nil { t.Fatal("ctrl-] is held, not forwarded") }
	if a := k.Flush(); a != ActDetach { t.Fatal("a lone ctrl-] leaves once the hold expires") }
	for _, b := range []byte{0x14, 0x17, 'n', 'p', 'd', 'x'} {
		if a, fwd := k.Feed(b); a != ActNone || len(fwd) != 1 { t.Fatalf("%x must forward", b) }
	}
}

func TestTheLeaveKeyTwiceGoesToThePane(t *testing.T) {
	k := NewKeyReader(0x1d)
	if a, fwd := k.Feed(0x1d); a != ActNone || fwd != nil { t.Fatal("first press is held") }
	if a, fwd := k.Feed(0x1d); a != ActNone || !bytes.Equal(fwd, []byte{0x1d}) { t.Fatal("second press forwards one leave byte") }
	if a := k.Flush(); a != ActNone { t.Fatal("nothing is held after the pair") }
}

func TestParseKeyNamesCtrlAndBrackets(t *testing.T) {
	b, err := ParseKey("ctrl-]")
	if err != nil || b != 0x1d { t.Fatalf("%x %v", b, err) }
	if _, err := ParseKey("shift-tab"); err == nil { t.Fatal("a multi-byte key cannot be the leave key") }
}
```

- [ ] **Step 2: Run**: `go test -p 1 ./internal/attach ./internal/config`: FAIL.
- [ ] **Step 3: Implement**. Delete `HelpText` and the `?` help screen; the status line is the only hint. The leave key returns to the roster in the TUI and detaches in the standalone `coppice attach`.
- [ ] **Step 4: Run**: `cd harness/coppice && go test -p 1 ./...`: PASS. Fix every test that scripted `ctrl+a` sequences to use the leave key.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/attach harness/coppice/internal/config harness/coppice/internal/cli/cli.go
git commit -m "coppice: inside a pane, one key leaves and every other key belongs to the harness"
```

---

### Task 4: ctrl-t talks, ctrl-w closes what you look at, Esc goes up

**Files:**
- Modify: `harness/coppice/internal/tui/run.go`
- Test: `harness/coppice/internal/tui/run_test.go`

- [ ] **Step 1: Write the failing tests**: ctrl-t from the roster focuses the prompt and the next typed bytes land in it; ctrl-w on a selected row with a tile closes the tile only (the pane stays in `pane.list`); ctrl-w on a row with no tile asks `close <label>? y/n` on the prompt line and `y` sends `pane.close`; Esc with a peek open closes the peek; Esc with the prompt focused blurs it; Esc on the bare roster does nothing and prints nothing.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/run.go harness/coppice/internal/tui/run_test.go
git commit -m "coppice/tui: ctrl-t talks, ctrl-w closes what you look at, esc goes up one level"
```

---

### Task 5: Update the docs and the master spec

**Files:**
- Modify: `harness/coppice/README.md` (keys section)
- Modify: `docs/plans/2026-09-08-workshop/00-master-spec.md` (replace any ctrl+a mention)
- Modify: `docs/plans/2026-09-12-porcelain/ROADMAP.md` (move the leave key from "open" to a decision line)

- [ ] **Step 1:** `grep -rn "ctrl+a\|ctrl-a" harness/coppice/README.md docs/` and rewrite each hit.
- [ ] **Step 2:** Commit

```bash
git add harness/coppice/README.md docs/plans/2026-09-08-workshop/00-master-spec.md docs/plans/2026-09-12-porcelain/ROADMAP.md
git commit -m "docs: the six keys and the leave key, decided"
```
