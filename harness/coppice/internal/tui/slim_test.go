package tui

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

func TestSlimRailFrom88To118ColumnsShowsOneWindow(t *testing.T) {
	for cols, want := range map[int]int{80: 0, 87: 0, 88: 1, 100: 1, 118: 1, 119: 1, 120: 1, 200: 2} {
		if got := windowCount(cols); got != want {
			t.Fatalf("windowCount(%d) = %d, want %d", cols, got, want)
		}
	}
	for cols := slimFrom; cols < railCols+minWindow+1; cols++ {
		rail, widths := splitWidths(cols, windowCount(cols))
		if rail != slimCols || len(widths) != 1 || widths[0] < minWindow {
			t.Fatalf("at %d columns: rail %d, windows %v", cols, rail, widths)
		}
	}
	if rail, _ := splitWidths(119, 1); rail != railCols {
		t.Fatalf("at 119 columns the rail is %d, want the full %d", rail, railCols)
	}
}

func TestSlimRailRowsAreSixColumnsAndEveryLineIsColsWide(t *testing.T) {
	m := tiledModel()
	for _, cols := range []int{88, 100, 118} {
		lines := Render(m, cols, 20)
		for _, l := range lines {
			if w := textwidth.Width(stripSGR(l)); w != cols {
				t.Fatalf("at %d columns a line is %d cells: %q", cols, w, l)
			}
		}
		if len(m.TileX) != 1 || m.TileX[0] != slimCols+1 || m.TileW[0] < minWindow {
			t.Fatalf("at %d columns the window starts at %v with width %v", cols, m.TileX, m.TileW)
		}
		joined := stripSGR(strings.Join(lines, "\n"))
		if !strings.Contains(joined, "›1●g  │") {
			t.Fatalf("no slim row for pane a at %d columns:\n%s", cols, joined)
		}
		if !strings.Contains(joined, "  ●d  │") {
			t.Fatalf("no slim row for pane b at %d columns:\n%s", cols, joined)
		}
		if !strings.Contains(joined, "gate-refactor  claude  blocked") {
			t.Fatalf("the window header lost the full name:\n%s", joined)
		}
	}
}

func TestSlimRailBarNamesTheRowUnderTheCursor(t *testing.T) {
	m := tiledModel()
	m.Cursor = m.indexOf("b", 0)
	lines := Render(m, 100, 20)
	if !strings.Contains(lines[0], "coppice · docs") {
		t.Fatalf("the bar does not name the cursor row: %q", lines[0])
	}
	wide := Render(m, 140, 20)
	if strings.Contains(wide[0], "docs") {
		t.Fatalf("the full rail repeats the name in the bar: %q", wide[0])
	}
}

func TestSlimRailGivesWayToThePeek(t *testing.T) {
	m := tiledModel()
	m.Peek = &PeekView{Pane: "a", Label: "gate-refactor", Text: "hello\n"}
	lines := Render(m, 100, 24)
	for _, l := range lines {
		if strings.Contains(l, "│") {
			t.Fatalf("a divider shows with the peek open: %q", l)
		}
	}
	if !strings.Contains(stripSGR(strings.Join(lines, "\n")), "peek a gate-refactor") {
		t.Fatal("the peek does not show")
	}
}

func TestAClickOnTheLastSlimColumnsDoesNotAskToStop(t *testing.T) {
	m := tiledModel()
	Render(m, 100, 20)
	if !m.slim {
		t.Fatal("the slim rail is not drawn at 100 columns")
	}
	f := &floor{m: m, o: Options{Size: func() (int, int) { return 100, 20 }}}
	row, _ := HitRow(m, 4)
	_ = row
	if err := f.click(Click{X: slimCols, Y: 4, Button: 0}); err != nil {
		t.Fatal(err)
	}
	if m.Confirm != "" {
		t.Fatalf("a click on the slim rail asked to stop %q", m.Confirm)
	}
}

func TestSubtasksNestUnderTheirTask(t *testing.T) {
	m := &Model{
		Tasks: []TaskRow{
			{ID: "t1", Label: "ship"},
			{ID: "t2", Label: "docs", Parent: "t1"},
			{ID: "t3", Label: "deep", Parent: "t2"},
			{ID: "t4", Label: "other"},
		},
		Rows: []Row{
			{ID: "w1:p1", Label: "a", Task: "t1", State: "idle"},
			{ID: "w1:p2", Label: "b", Task: "t4", State: "idle"},
			{ID: "w1:p3", Label: "c", Task: "t2", State: "blocked"},
			{ID: "w1:p4", Label: "d", Task: "t3", State: "idle"},
		},
		NeedYou: 1,
	}
	gs := m.groups()
	var keys []string
	for _, g := range gs {
		keys = append(keys, g.Key)
	}
	if got, want := strings.Join(keys, " "), "task:t1 task:t2 task:t3 task:t4"; got != want {
		t.Fatalf("groups = %s, want %s", got, want)
	}
	if gs[1].Depth != 1 || gs[1].Parent != "task:t1" || gs[2].Depth != 2 || gs[2].Parent != "task:t2" || gs[3].Depth != 0 {
		t.Fatalf("depth or parent wrong: %+v", gs)
	}
	if gs[0].needs() != 1 || gs[0].counts() != "1 needs · 2 idle" {
		t.Fatalf("the parent header should count the subtasks: %d %q", gs[0].needs(), gs[0].counts())
	}
	lines := Render(m, 80, 20)
	text := stripSGR(strings.Join(lines, "\n"))
	for _, want := range []string{"▾ ship", "  ▾ docs", "    ▾ deep", "▾ other"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in:\n%s", want, text)
		}
	}
	// Folding the parent hides the subtasks and their rows.
	m.Folded = map[string]bool{"task:t1": true}
	text = stripSGR(strings.Join(Render(m, 80, 20), "\n"))
	if strings.Contains(text, "docs") || strings.Contains(text, "deep") || !strings.Contains(text, "▸ ship") {
		t.Fatalf("a folded parent still shows its subtasks:\n%s", text)
	}
	if got := len(m.treeRows()); got != 4 {
		t.Fatalf("window order skips folded rows: %d", got)
	}
}

func TestAParentWithNoAgentStillDrawsWhenASubtaskHasOne(t *testing.T) {
	m := &Model{
		Tasks: []TaskRow{{ID: "t1", Label: "ship"}, {ID: "t2", Label: "docs", Parent: "t1"}},
		Rows:  []Row{{ID: "w1:p1", Label: "a", Task: "t2", State: "idle"}},
	}
	gs := m.groups()
	if len(gs) != 2 || gs[0].Key != "task:t1" || len(gs[0].Rows) != 0 || gs[1].Parent != "task:t1" {
		t.Fatalf("groups = %+v", gs)
	}
}

func TestAParentCycleDoesNotLoseTasks(t *testing.T) {
	m := &Model{
		Tasks: []TaskRow{{ID: "t1", Parent: "t2"}, {ID: "t2", Parent: "t1"}},
		Rows: []Row{
			{ID: "w1:p1", Task: "t1", State: "idle"},
			{ID: "w1:p2", Task: "t2", State: "idle"},
		},
	}
	if gs := m.groups(); len(gs) != 2 {
		t.Fatalf("groups = %+v", gs)
	}
}

func TestSlimBarKeepsTheCountOnScreen(t *testing.T) {
	m := tiledModel()
	m.Rows[0].Label = strings.Repeat("long-name-", 10)
	m.NeedYou = 2
	for _, cols := range []int{88, 100, 118} {
		lines := Render(m, cols, 20)
		if !strings.HasSuffix(strings.TrimRight(stripSGR(lines[0]), " "), "· 2 need you") {
			t.Fatalf("at %d columns the count is cut: %q", cols, lines[0])
		}
		if !strings.Contains(lines[0], "coppice · long-name-") || !strings.Contains(lines[0], "…") {
			t.Fatalf("at %d columns the name is not cut with an ellipsis: %q", cols, lines[0])
		}
	}
}

func TestSlimLabelsAreTheShortestUniquePrefix(t *testing.T) {
	row := func(id, label string) item { return item{kind: itemRow, row: Row{ID: id, Label: label}} }
	items := []item{
		row("1", "gate"), row("2", "docs"), row("3", "docker"), row("4", "e2e shell"),
		row("5", "e2e ask"), row("6", "e2e trust"), {kind: itemGroup}, row("7", "build"),
	}
	want := map[string]string{"1": "g ", "2": "do", "3": "do", "4": "e1", "5": "e2", "6": "e3", "7": "b "}
	got := slimLabels(items)
	// docs and docker share "do", so both take the digit form.
	want["2"], want["3"] = "d1", "d2"
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("label of %s = %q, want %q (all: %v)", id, got[id], w, got)
		}
	}
}

func TestSlimNestedGroupHeaderLeadsWithADot(t *testing.T) {
	m := &Model{
		Tasks: []TaskRow{{ID: "t1", Label: "ship"}, {ID: "t2", Label: "docs", Parent: "t1"}},
		Rows: []Row{
			{ID: "w1:p1", Label: "a", Task: "t1", State: "idle"},
			{ID: "w1:p2", Label: "b", Task: "t2", State: "idle"},
		},
	}
	m.Tiles = tiledModel().Tiles
	text := stripSGR(strings.Join(Render(m, 100, 12), "\n"))
	if !strings.Contains(text, "▾shi │") || !strings.Contains(text, "▾·do │") {
		t.Fatalf("nested header not marked:\n%s", text)
	}
}

func TestTheFullRailIndentsAtMostFourLevels(t *testing.T) {
	var tasks []TaskRow
	var rows []Row
	parent := ""
	for i := 0; i < 7; i++ {
		id := "t" + string(rune('0'+i))
		tasks = append(tasks, TaskRow{ID: id, Label: "L" + id, Parent: parent})
		rows = append(rows, Row{ID: "w1:p" + string(rune('1'+i)), Label: "a" + id, Task: id, State: "idle"})
		parent = id
	}
	m := &Model{Tasks: tasks, Rows: rows}
	text := stripSGR(strings.Join(Render(m, 80, 30), "\n"))
	if !strings.Contains(text, "\n"+strings.Repeat(" ", 10)+"▾ Lt6") || strings.Contains(text, strings.Repeat(" ", 12)+"▾ ") {
		t.Fatalf("indent not clamped:\n%s", text)
	}
}
