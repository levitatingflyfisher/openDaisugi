package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The web page keeps its own copy of these words. Both sides hold theirs
// to testdata/words.json, so the owner reads the same line on each.
func TestTheSharedWordsMatchTheWebPage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "words.json"))
	if err != nil {
		t.Fatal(err)
	}
	var w map[string]any
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	code := func(n int) *int { return &n }
	got := map[string]any{
		"ended":                endedText(Row{ID: "x", Label: "fine"}),
		"ended_exit_0":         endedText(Row{ID: "x", Label: "fine", ExitCode: code(0)}),
		"ended_exit_3":         endedText(Row{ID: "x", Label: "fine", ExitCode: code(3)}),
		"ended_killed":         endedText(Row{ID: "x", Label: "fine", ExitCode: code(-1)}),
		"kill_confirm":         StopQuestion("fine"),
		"clear_all_confirm_2":  ClearAllQuestion(2),
		"clear_all_confirm_1":  ClearAllQuestion(1),
		"blocked_by_gate":      BlockedByGate,
		"blocked_own_question": BlockedOwnQuestion,
		"ended_clear_ms":       float64(EndedClear.Milliseconds()),
	}
	for k, want := range w {
		if strings.HasPrefix(k, "_") {
			continue
		}
		if got[k] != want {
			t.Errorf("%s = %q, words.json says %q", k, got[k], want)
		}
	}
	if len(got) != len(w)-1 {
		t.Errorf("the test pins %d words, words.json holds %d", len(got), len(w)-1)
	}
}

// A blocked row with no ask says what found the block. Only the gate's
// own block blames the gate.
func TestABlockWithNoAskSaysWhoFoundIt(t *testing.T) {
	rows := RowsFrom([]map[string]any{
		{"id": "a", "state": "blocked", "source": "manifest", "detail": "live_prompt_box"},
		{"id": "b", "state": "blocked", "source": "gate"},
		{"id": "c", "state": "blocked", "source": "gate", "ask": map[string]any{"tool": "Bash", "summary": "rm"}},
	}, 0)
	if rows[0].Line != BlockedOwnQuestion || rows[1].Line != BlockedByGate || rows[2].Line != "Bash: rm" {
		t.Fatalf("lines %q %q %q", rows[0].Line, rows[1].Line, rows[2].Line)
	}
}

// No label, ask line or message reaches the terminal with its control
// characters: an agent could set any of them.
func TestNoEscapeFromALabelReachesTheScreen(t *testing.T) {
	evil := "x\x1b]0;owned\x07\x1b[2J\x9b1m"
	code := 2
	m := &Model{
		Rows:    []Row{{ID: "a", Label: evil, State: "blocked", Line: evil}},
		Ended:   []Row{{ID: "e", Label: evil, State: "ended", ExitCode: &code}},
		Message: evil + " stopped.",
		NeedYou: 1,
	}
	m.noteEnded(m.Ended[0])
	check := func(lines []string) {
		t.Helper()
		for _, l := range lines {
			if plain := stripSGR(l); strings.ContainsAny(plain, "\x1b\x07\x9b") {
				t.Fatalf("a control character reached the screen: %q", plain)
			}
		}
	}
	check(Render(m, 100, 20))
	m.Renaming = "a"
	check(Render(m, 100, 20))
	m.Renaming, m.Confirm = "", "a"
	check(Render(m, 100, 20))
}

// A harness name, a task label and a worktree name reach no screen with
// their control characters, in the rail, the window header or the tree.
func TestNoEscapeFromAHarnessOrTaskReachesTheScreen(t *testing.T) {
	evil := "x\x1b]0;owned\x07\x1b[2J\x9b1m"
	check := func(where string, lines []string) {
		t.Helper()
		for _, l := range lines {
			if plain := stripSGR(l); strings.ContainsAny(plain, "\x1b\x07\x9b") {
				t.Fatalf("%s: a control character reached the screen: %q", where, plain)
			}
		}
	}
	m := tiledModel()
	for i := range m.Rows {
		m.Rows[i].Harness = evil
		m.Rows[i].Task = "t1"
	}
	m.Tasks = []TaskRow{{ID: "t1", Label: evil, Worktree: "/w/" + evil}}
	check("floor", Render(m, 140, 20))
	check("tree", RenderTree(m.Tasks, m.Rows, 80))
}
