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
	if !strings.Contains(lines[0], "1 need you") {
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

func TestTheFooterIsTheKeyHints(t *testing.T) {
	lines := Render(&Model{Rows: []Row{{ID: "a", State: "working"}}}, 80, 10)
	if strings.TrimRight(stripSGR(lines[8]), " ") != Hints() {
		t.Fatalf("footer = %q, want %q", lines[8], Hints())
	}
}

func TestSectionsOnlyRenderWhenTheyHoldRows(t *testing.T) {
	out := strings.Join(Render(&Model{Rows: []Row{{ID: "a", State: "working"}}}, 80, 10), "\n")
	if strings.Contains(out, "NEEDS YOU") || strings.Contains(out, "DONE") {
		t.Fatalf("empty sections rendered: %s", out)
	}
	if !strings.Contains(out, "WORKING") {
		t.Fatalf("WORKING missing: %s", out)
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
	if strings.TrimRight(stripSGR(lines[4]), " ") != Hints() {
		t.Fatalf("footer not second to last: %q", lines[4])
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
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}}, Cursor: 1}
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
	lines := Render(&Model{Rows: []Row{{ID: "a", State: "working"}}, Message: "swap is not wired yet."}, 60, 10)
	if !strings.HasPrefix(stripSGR(lines[7]), "swap is not wired yet.") {
		t.Fatalf("message line = %q", lines[7])
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
	if !strings.Contains(out, "WORKING") {
		t.Fatalf("the roster must keep the upper half: %s", out)
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
