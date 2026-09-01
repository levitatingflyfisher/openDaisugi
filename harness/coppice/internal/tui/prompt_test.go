package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/tiles"
)

func TestAHarnessNameIsPlumbingASentenceIsTalk(t *testing.T) {
	h := []string{"claude", "pi"}
	if Classify("claude", h) != Plumbing || Classify("pi --model llama", h) != Plumbing {
		t.Fatal("harness names must be plumbing")
	}
	if Classify("close docs", h) != Plumbing {
		t.Fatal("verbs must be plumbing")
	}
	if Classify("open a pane with pi on the docs", h) != TalkLine {
		t.Fatal("a sentence must be talk")
	}
}

func TestTheFourWordBoundaryOnVerbs(t *testing.T) {
	h := Harnesses
	if Classify("open claude", h) != Plumbing {
		t.Fatal("open claude is plumbing")
	}
	if Classify("open claude --model opus", h) != Plumbing {
		t.Fatal("four words led by a verb is plumbing")
	}
	if Classify("open claude --model opus --verbose", h) != TalkLine {
		t.Fatal("five words led by a verb is talk")
	}
	if Classify("pi please fix the docs and then the tests", h) != Plumbing {
		t.Fatal("a harness name leads plumbing at any length")
	}
	if Classify("please open claude", h) != TalkLine {
		t.Fatal("a line led by neither is talk")
	}
	if Classify("  list  ", h) != Plumbing {
		t.Fatal("surrounding spaces do not change the verdict")
	}
	if Classify("", h) != TalkLine {
		t.Fatal("an empty line is talk")
	}
}

func TestHarnessesAndVerbsAreTheAgreedLists(t *testing.T) {
	if strings.Join(Harnesses, " ") != "claude codex pi sprig opencode" {
		t.Fatalf("Harnesses = %v", Harnesses)
	}
	if strings.Join(Verbs, " ") != "list read close open swap rotate reset lock unlock layout tree zoom foreman" {
		t.Fatalf("Verbs = %v", Verbs)
	}
}

func TestOpenPeekReadsTheAskAndTheGate(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: "x", State: "blocked"}}}
	m.OpenPeek("line1\nline2", map[string]any{
		"summary": "push --force", "tool": "git",
		"gate": map[string]any{"verdict": "deny", "rule": float64(3)},
	})
	p := m.Peek
	if p == nil {
		t.Fatal("no peek")
	}
	if p.Pane != "a" || p.Label != "x" || p.Text != "line1\nline2" {
		t.Fatalf("peek = %+v", p)
	}
	if p.Ask != "git: push --force" || p.Tool != "git" {
		t.Fatalf("ask = %+v", p)
	}
	if !p.HasGate || p.Verdict != "deny" || p.Rule != 3 {
		t.Fatalf("gate = %+v", p)
	}
}

func TestOpenPeekWithNoAskOrGate(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	m.OpenPeek("text", nil)
	if m.Peek == nil || m.Peek.HasGate || m.Peek.Ask != "" {
		t.Fatalf("peek = %+v", m.Peek)
	}
	m.OpenPeek("text", map[string]any{"summary": "ls"})
	if m.Peek.HasGate || m.Peek.Ask != "ls" {
		t.Fatalf("peek = %+v", m.Peek)
	}
}

func TestOpenPeekOnAnEmptyFloorOpensNothing(t *testing.T) {
	var m Model
	m.OpenPeek("text", nil)
	if m.Peek != nil {
		t.Fatalf("peek = %+v", m.Peek)
	}
}

func TestClosePeekRemovesIt(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	m.OpenPeek("text", nil)
	m.ClosePeek()
	if m.Peek != nil {
		t.Fatal("peek still open")
	}
	m.ClosePeek()
}

// promptFloor is a floor with three panes on the roster, two slots, and a
// data dir for floor.json. It never reaches a server.
func promptFloor(t *testing.T) *floor {
	t.Helper()
	m := &Model{Rows: []Row{
		{ID: "a", State: "working", Age: 1},
		{ID: "b", State: "working", Age: 2},
		{ID: "c", State: "working", Age: 3},
	}, Tiles: tiles.New(tiles.Focus, 2), Screens: map[string]*attach.Screen{}}
	f := testFloor(m, 200)
	f.o.DataDir = t.TempDir()
	return f
}

func TestSwapExchangesSlotsAndFocusFollowsThePane(t *testing.T) {
	f := promptFloor(t)
	f.m.Tiles.Fill("a")
	f.m.Tiles.Fill("b")
	f.m.Tiles.Focus = 0
	if err := f.runPrompt("swap 1 2"); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Tiles.Slots; got[0] != "b" || got[1] != "a" {
		t.Fatalf("slots = %v", got)
	}
	if f.m.Tiles.Focused() != "a" {
		t.Fatalf("focus left a: %+v", f.m.Tiles)
	}
	if f.m.Message != "" {
		t.Fatalf("message = %q", f.m.Message)
	}
}

func TestSwapNeedsTwoSlotNumbers(t *testing.T) {
	for _, line := range []string{"swap", "swap 1", "swap a b", "swap 1 2 3"} {
		f := promptFloor(t)
		if err := f.runPrompt(line); err != nil {
			t.Fatal(err)
		}
		if f.m.Message != "swap needs two slot numbers, as in swap 1 2" {
			t.Fatalf("%q gave message %q", line, f.m.Message)
		}
	}
	for _, line := range []string{"swap 1 3", "swap 0 1", "swap 2 -1"} {
		f := promptFloor(t)
		_ = f.runPrompt(line)
		if f.m.Message != "swap needs slot numbers from 1 to 2" {
			t.Fatalf("%q gave message %q", line, f.m.Message)
		}
	}
}

func TestResetOrdersSlotsByTheRoster(t *testing.T) {
	f := promptFloor(t)
	f.m.Tiles.Fill("c")
	f.m.Tiles.Fill("b")
	if err := f.runPrompt("reset"); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Tiles.Slots; got[0] != "a" || got[1] != "b" {
		t.Fatalf("slots = %v, want the roster order", got)
	}
}

func TestRotateMovesEverySlotRight(t *testing.T) {
	f := promptFloor(t)
	f.m.Tiles.Fill("a")
	f.m.Tiles.Fill("b")
	if err := f.runPrompt("rotate"); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Tiles.Slots; got[0] != "b" || got[1] != "a" {
		t.Fatalf("slots = %v", got)
	}
}

func TestLockRefusesSwapUntilUnlock(t *testing.T) {
	f := promptFloor(t)
	f.m.Tiles.Fill("a")
	f.m.Tiles.Fill("b")
	if err := f.runPrompt("lock"); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"swap 1 2", "rotate", "reset", "layout all"} {
		if err := f.runPrompt(line); err != nil {
			t.Fatal(err)
		}
		if f.m.Message != "Layout is locked. Type unlock first." {
			t.Fatalf("%q gave message %q", line, f.m.Message)
		}
	}
	if got := f.m.Tiles.Slots; got[0] != "a" || got[1] != "b" {
		t.Fatalf("a locked layout moved: %v", got)
	}
	if err := f.runPrompt("unlock"); err != nil {
		t.Fatal(err)
	}
	if err := f.runPrompt("swap 1 2"); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Tiles.Slots; got[0] != "b" {
		t.Fatalf("swap after unlock did nothing: %v", got)
	}
}

func TestLayoutAllGivesEveryPaneASlot(t *testing.T) {
	f := promptFloor(t)
	f.m.Tiles.Fill("c")
	if err := f.runPrompt("layout all"); err != nil {
		t.Fatal(err)
	}
	if f.m.Tiles.Layout != tiles.All {
		t.Fatalf("layout = %q", f.m.Tiles.Layout)
	}
	if got := strings.Join(f.m.Tiles.Slots, " "); got != "c a b" {
		t.Fatalf("slots = %q, want the current slots then the rest of the roster", got)
	}
	if err := f.runPrompt("layout one"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.m.Tiles.Slots, " "); got != "c" {
		t.Fatalf("slots under one = %q", got)
	}
	if err := f.runPrompt("layout focus"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.m.Tiles.Slots, " "); got != "c a" {
		t.Fatalf("slots under focus = %q", got)
	}
}

func TestLayoutWithAnotherWordTeaches(t *testing.T) {
	for _, line := range []string{"layout", "layout grid"} {
		f := promptFloor(t)
		if err := f.runPrompt(line); err != nil {
			t.Fatal(err)
		}
		if f.m.Message != "layout is all, focus, or one" {
			t.Fatalf("%q gave message %q", line, f.m.Message)
		}
	}
}

func TestLayoutAndLockAreWrittenToFloorJSON(t *testing.T) {
	f := promptFloor(t)
	if err := f.runPrompt("layout one"); err != nil {
		t.Fatal(err)
	}
	if err := f.runPrompt("lock"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.o.DataDir, "floor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != `{"layout":"one","locked":true}` {
		t.Fatalf("floor.json = %s", got)
	}
	layout, locked := readFloorState(f.o.DataDir)
	if layout != tiles.One || !locked {
		t.Fatalf("readFloorState = %q %v", layout, locked)
	}
}

func TestAMissingOrUnreadableFloorJSONIsFocusUnlocked(t *testing.T) {
	dir := t.TempDir()
	if layout, locked := readFloorState(dir); layout != tiles.Focus || locked {
		t.Fatalf("missing file gave %q %v", layout, locked)
	}
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if layout, locked := readFloorState(dir); layout != tiles.Focus || locked {
		t.Fatalf("garbage gave %q %v", layout, locked)
	}
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte(`{"layout":"grid","locked":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if layout, locked := readFloorState(dir); layout != tiles.Focus || !locked {
		t.Fatalf("an unknown layout gave %q %v, want focus and the lock kept", layout, locked)
	}
	if layout, locked := readFloorState(""); layout != tiles.Focus || locked {
		t.Fatalf("no data dir gave %q %v", layout, locked)
	}
}

func TestFloorJSONIsWrittenWithNoStrayFiles(t *testing.T) {
	f := promptFloor(t)
	for _, line := range []string{"layout all", "lock", "unlock"} {
		if err := f.runPrompt(line); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(f.o.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != floorFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("data dir holds %v, want only %s", names, floorFile)
	}
	if layout, locked := readFloorState(f.o.DataDir); layout != tiles.All || locked {
		t.Fatalf("readFloorState = %q %v", layout, locked)
	}
}
