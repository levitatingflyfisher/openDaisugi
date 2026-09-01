package tui

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

func TestRenderFitsTheScreenAndEndsWithThePrompt(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: "gate-refactor", Harness: "claude", State: "blocked", Line: "wants git push --force"}}, NeedYou: 1}
	lines := Render(m, 80, 12)
	if len(lines) != 12 {
		t.Fatalf("%d lines", len(lines))
	}
	if !strings.Contains(lines[0], "1 needs you") {
		t.Fatalf("bar: %q", lines[0])
	}
	if !strings.HasPrefix(stripSGR(lines[11]), "› ") {
		t.Fatalf("prompt not last: %q", lines[11])
	}
	for _, l := range lines {
		if textwidth.Width(stripSGR(l)) > 80 {
			t.Fatalf("line too wide: %q", l)
		}
	}
}

func TestEveryLineIsExactlyColsWide(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "a", Label: "gate-refactor", Harness: "claude", State: "blocked", Line: "wants git push --force"},
		{ID: "b", Label: "docs", Harness: "pi", State: "working", Line: "editing README"},
		{ID: "c", Label: "tests", Harness: "codex", State: "done"},
		{ID: "d", State: "idle"},
	}, NeedYou: 1, Message: "hello", Prompt: "close b"}
	for _, l := range Render(m, 60, 14) {
		if w := textwidth.Width(stripSGR(l)); w != 60 {
			t.Fatalf("line is %d cells, want 60: %q", w, l)
		}
	}
}

func TestTheBarSaysQuietWhenNobodyNeedsYou(t *testing.T) {
	lines := Render(&Model{Rows: []Row{{ID: "a", State: "working"}}}, 40, 6)
	if !strings.HasPrefix(lines[0], "coppice · quiet") {
		t.Fatalf("bar: %q", lines[0])
	}
}

func TestAnEmptyFloorSaysWhatToDo(t *testing.T) {
	lines := Render(&Model{Default: "claude"}, 80, 6)
	if !strings.Contains(strings.Join(lines, "\n"), "Nothing running. Enter opens claude here.") {
		t.Fatal("empty state does not teach")
	}
}

func TestAnEmptyFloorNamesTheDefaultHarness(t *testing.T) {
	lines := Render(&Model{Default: "pi"}, 80, 6)
	if !strings.Contains(strings.Join(lines, "\n"), "Enter opens pi here.") {
		t.Fatalf("empty state does not name pi: %q", lines)
	}
}

// footerAt joins the n footer lines that end just above the prompt.
func footerAt(lines []string, n int) string {
	var parts []string
	for _, l := range lines[len(lines)-1-n : len(lines)-1] {
		parts = append(parts, strings.TrimRight(stripSGR(l), " "))
	}
	return strings.Join(parts, "  ")
}

func TestTheFooterIsTheKeyHints(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	lines := Render(m, 80, 10)
	n := len(footerLines(m, 80))
	if n != 2 {
		t.Fatalf("the footer takes %d lines at 80 columns, want 2", n)
	}
	// The talk key fits on the second line, so it is named there too.
	if got, want := footerAt(lines, n), Keys()+"  ctrl-\\ speak"; got != want {
		t.Fatalf("footer = %q, want %q", got, want)
	}
}

// At every width the footer keeps every rail key on screen, wrapped at
// its gaps.
func TestTheFooterWrapsAndKeepsEveryKey(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	for _, cols := range []int{60, 80, 120, 200} {
		lines := footerLines(m, cols)
		for _, l := range lines {
			if w := textwidth.Width(l); w != cols {
				t.Fatalf("at %d columns a footer line is %d cells", cols, w)
			}
		}
		joined := strings.Join(lines, " ")
		for _, b := range Table() {
			if !strings.Contains(joined, b.Keys+" "+b.Help) {
				t.Fatalf("at %d columns the footer lacks %q: %q", cols, b.Keys+" "+b.Help, joined)
			}
		}
	}
}

// With no tasks the rail groups agents by project, one header each with
// its state counts, and a count that is zero is left out.
func TestTheRailGroupsByProjectWithCounts(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "w1:p1", State: "working", Worktree: "/src/trellis"},
		{ID: "w1:p2", State: "blocked", Worktree: "/src/glean"},
		{ID: "w1:p3", State: "idle", Worktree: "/src/trellis"},
	}}
	out := stripSGR(strings.Join(Render(m, 80, 12), "\n"))
	glean := strings.Index(out, "▾ glean  1 needs")
	trellis := strings.Index(out, "▾ trellis  1 working · 1 idle")
	if glean < 0 || trellis < 0 || trellis > glean {
		t.Fatalf("want trellis, made first, then glean: %s", out)
	}
	if strings.Contains(out, "0 ") {
		t.Fatalf("a zero count shows: %s", out)
	}
}

// A task with a live agent gets a header, and agents with no task group
// by project after the task groups. A task with no live agent gets none.
func TestTheRailGroupsByTaskThenProject(t *testing.T) {
	m := &Model{Tasks: []TaskRow{{ID: "t1", Label: "trellis-fix"}, {ID: "t2", Label: "empty"}}, Rows: []Row{
		{ID: "w1:p1", State: "idle", Worktree: "/src/glean"},
		{ID: "w1:p2", State: "working", Task: "t1"},
		{ID: "w1:p3", State: "blocked", Task: "t1"},
	}}
	out := stripSGR(strings.Join(Render(m, 80, 12), "\n"))
	task := strings.Index(out, "▾ trellis-fix  1 needs · 1 working")
	project := strings.Index(out, "▾ glean  1 idle")
	if task < 0 || project < 0 || project < task {
		t.Fatalf("want the task group, then the project group: %s", out)
	}
	if strings.Contains(out, "empty") || strings.Contains(out, "no task") {
		t.Fatalf("a header for no live agent: %s", out)
	}
}

// A group's header never moves when an agent in another group comes to
// need you.
func TestHeadersKeepTheirPlaceWhenAStateChanges(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "w1:p1", State: "working", Worktree: "/src/a"},
		{ID: "w1:p2", State: "working", Worktree: "/src/b"},
	}}
	before := m.groups()
	m.Event(map[string]any{"pane": "w1:p2", "state": "blocked", "source": "gate"})
	after := m.groups()
	if before[0].Key != after[0].Key || after[0].Title != "a" {
		t.Fatalf("groups moved: %v then %v", before, after)
	}
}

// Enter on a header folds its group to the header line, and again opens
// it. Rows keep the order they were made in through state changes.
func TestAHeaderFoldsAndRowsKeepTheirOrder(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "w1:p2", State: "working", Worktree: "/a", Age: 1},
		{ID: "w1:p10", State: "working", Worktree: "/a", Age: 0},
	}}
	if got := m.treeRows(); got[0].ID != "w1:p2" || got[1].ID != "w1:p10" {
		t.Fatalf("order %v, want the order made", got)
	}
	f := testFloor(m, 80)
	m.Move(-1)
	if it, _ := m.SelectedItem(); it.kind != itemGroup {
		t.Fatalf("up from the first row landed on %q, want the header", it.key())
	}
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	out := stripSGR(strings.Join(Render(m, 80, 10), "\n"))
	if !strings.Contains(out, "▸ a  2 working") || strings.Contains(out, "w1:p2") {
		t.Fatalf("the group did not fold: %s", out)
	}
	handleAll(t, f, key{kind: keyByte, b: ' '})
	if out := stripSGR(strings.Join(Render(m, 80, 10), "\n")); !strings.Contains(out, "w1:p10") {
		t.Fatalf("Space did not open the group: %s", out)
	}
	m.Event(map[string]any{"pane": "w1:p10", "state": "blocked", "source": "gate"})
	if got := m.treeRows(); got[0].ID != "w1:p2" {
		t.Fatalf("a state event moved the rows: %v", got)
	}
}

func TestAWideGlyphLabelDoesNotOverflow(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: strings.Repeat("日本語", 20), Harness: "claude", State: "working", Line: strings.Repeat("字", 40)}}}
	for _, l := range Render(m, 40, 8) {
		if w := textwidth.Width(stripSGR(l)); w != 40 {
			t.Fatalf("line is %d cells, want 40: %q", w, l)
		}
	}
}

func TestATinyScreenNeverReturnsMoreThanRows(t *testing.T) {
	var rows []Row
	for i := 0; i < 30; i++ {
		rows = append(rows, Row{ID: string(rune('a' + i)), State: "working"})
	}
	lines := Render(&Model{Rows: rows}, 80, 6)
	if len(lines) != 6 {
		t.Fatalf("%d lines, want 6", len(lines))
	}
	if !strings.HasPrefix(stripSGR(lines[5]), "› ") {
		t.Fatalf("prompt not last: %q", lines[5])
	}
	if got := footerAt(lines, 2); got != Keys()+"  ctrl-\\ speak" {
		t.Fatalf("footer not above the prompt: %q", got)
	}
	lines = Render(&Model{Rows: rows}, 80, 1)
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1", len(lines))
	}
	if len(Render(&Model{Rows: rows}, 80, 0)) != 0 {
		t.Fatal("zero rows must render zero lines")
	}
}

func TestTheCursorMarkerSitsOnTheCursorRowOnly(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}}, Cursor: 2}
	lines := Render(m, 60, 10)
	marked := 0
	for _, l := range lines {
		plain := stripSGR(l)
		if strings.HasPrefix(plain, "  › ") {
			marked++
			if !strings.Contains(plain, " b ") {
				t.Fatalf("marker on the wrong row: %q", plain)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("%d marked rows, want 1", marked)
	}
}

func TestALabelFallsBackToTheID(t *testing.T) {
	out := strings.Join(Render(&Model{Rows: []Row{{ID: "w1:p3", State: "working"}}}, 60, 8), "\n")
	if !strings.Contains(out, "w1:p3") {
		t.Fatalf("id missing: %s", out)
	}
}

func TestStatesAreColoredAndReset(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "a", State: "blocked"}, {ID: "b", State: "working"},
		{ID: "c", State: "done"}, {ID: "d", State: "idle"},
	}}
	out := strings.Join(Render(m, 60, 14), "\n")
	for _, code := range []string{"\x1b[33m", "\x1b[34m", "\x1b[32m", "\x1b[2m"} {
		if !strings.Contains(out, code) {
			t.Fatalf("color %q missing", code)
		}
	}
	for _, l := range Render(m, 60, 14) {
		if strings.Contains(l, "\x1b[") && !strings.HasSuffix(l, "\x1b[0m") {
			t.Fatalf("a colored line must end with a reset: %q", l)
		}
	}
}

func TestTheMessageLineShowsAboveTheFooter(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}, Message: "swap is not wired yet."}
	lines := Render(m, 60, 12)
	at := len(lines) - 2 - len(footerLines(m, 60))
	if !strings.HasPrefix(stripSGR(lines[at]), "swap is not wired yet.") {
		t.Fatalf("message line = %q", lines[at])
	}
}

func TestThePromptShowsItsText(t *testing.T) {
	lines := Render(&Model{Prompt: "close w1:p1"}, 60, 5)
	if !strings.HasPrefix(stripSGR(lines[4]), "› close w1:p1") {
		t.Fatalf("prompt = %q", lines[4])
	}
}

func TestStripSGRRemovesOnlyTheCodes(t *testing.T) {
	if got := stripSGR("\x1b[33mab\x1b[0m"); got != "ab" {
		t.Fatalf("got %q", got)
	}
	if got := stripSGR("plain"); got != "plain" {
		t.Fatalf("got %q", got)
	}
}

func TestPeekRendersTheAskAndTheVerdictOnTheirOwnLines(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: "x", State: "blocked"}}}
	m.OpenPeek("line1\nline2", map[string]any{"tool": "git", "summary": "push --force", "gate": map[string]any{"verdict": "deny", "rule": 3}})
	out := strings.Join(Render(m, 80, 20), "\n")
	if !strings.Contains(out, "The gate says no") || !strings.Contains(out, "rule 3") {
		t.Fatalf("verdict missing: %s", out)
	}
	lines := Render(m, 80, 20)
	var header, ask, verdict int
	for i, l := range lines {
		plain := stripSGR(l)
		switch {
		case strings.HasPrefix(plain, "peek a x"):
			header = i
		case strings.HasPrefix(plain, "asks: git: push --force"):
			ask = i
		case strings.HasPrefix(plain, "The gate says no, rule 3"):
			verdict = i
		}
	}
	if header == 0 || ask != header+1 || verdict != header+2 {
		t.Fatalf("header %d ask %d verdict %d: %s", header, ask, verdict, out)
	}
	if !strings.Contains(out, "line1") || !strings.Contains(out, "line2") {
		t.Fatalf("text missing: %s", out)
	}
}

func TestPeekWithAnAllowVerdict(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "blocked"}}}
	m.OpenPeek("", map[string]any{"summary": "ls", "gate": map[string]any{"verdict": "allow", "rule": 1.0}})
	out := strings.Join(Render(m, 80, 20), "\n")
	if !strings.Contains(out, "The gate says yes") {
		t.Fatalf("allow verdict missing: %s", out)
	}
}

func TestPeekWithNoGateShowsNoVerdict(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "blocked"}}}
	m.OpenPeek("text", map[string]any{"summary": "ls"})
	out := strings.Join(Render(m, 80, 20), "\n")
	if strings.Contains(out, "The gate says") {
		t.Fatalf("a verdict with no gate data: %s", out)
	}
	if !strings.Contains(out, "asks: ls") {
		t.Fatalf("ask missing: %s", out)
	}
}

func TestPeekTextIsTheLastLinesThatFit(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	var text []string
	for i := 1; i <= 40; i++ {
		text = append(text, "row"+strings.Repeat("x", i))
	}
	m.OpenPeek(strings.Join(text, "\n")+"\n", nil)
	lines := Render(m, 80, 12)
	if len(lines) != 12 {
		t.Fatalf("%d lines", len(lines))
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "row"+strings.Repeat("x", 40)) {
		t.Fatalf("last text line missing: %s", out)
	}
	if strings.Contains(out, "row"+strings.Repeat("x", 30)+"\n") {
		t.Fatalf("an early line was kept: %s", out)
	}
	if !strings.Contains(out, "▾ ") {
		t.Fatalf("the rail must keep the upper half: %s", out)
	}
}

func TestAClosedPeekLeavesTheRoster(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	m.OpenPeek("secret", nil)
	m.ClosePeek()
	if strings.Contains(strings.Join(Render(m, 80, 12), "\n"), "secret") {
		t.Fatal("peek text still rendered")
	}
}

// A pane label with control runes draws without them.
func TestAPaneLabelDrawsWithNoControlRunes(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "evil\x1b]52;c;aGk=\x07", State: "working"}}}
	for _, l := range Render(m, 80, 8) {
		if strings.Contains(stripSGR(l), "\x1b") || strings.Contains(l, "\x07") {
			t.Fatalf("a control rune reached the screen: %q", l)
		}
	}
}
