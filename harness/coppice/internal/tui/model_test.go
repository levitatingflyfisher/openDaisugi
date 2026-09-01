package tui

import "testing"

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

func TestGroupAlwaysReturnsThreeSectionsInOrder(t *testing.T) {
	secs := Group(nil)
	if len(secs) != 3 {
		t.Fatalf("%d sections, want 3", len(secs))
	}
	want := []string{"NEEDS YOU", "WORKING", "DONE"}
	for i, s := range secs {
		if s.Title != want[i] || len(s.Rows) != 0 {
			t.Fatalf("section %d = %+v, want empty %q", i, s, want[i])
		}
	}
}

func TestIdleAndUnknownRowsAreWorking(t *testing.T) {
	secs := Group([]Row{{ID: "a", State: "idle"}, {ID: "b", State: "unknown"}})
	if len(secs[1].Rows) != 2 {
		t.Fatalf("WORKING holds %d rows, want 2: %+v", len(secs[1].Rows), secs)
	}
}

func TestRowsWithEqualAgeSortByID(t *testing.T) {
	secs := Group([]Row{{ID: "b", State: "working"}, {ID: "a", State: "working"}})
	if secs[1].Rows[0].ID != "a" || secs[1].Rows[1].ID != "b" {
		t.Fatalf("equal ages must sort by id: %+v", secs[1].Rows)
	}
}

func TestNeedYouCountFollowsTheList(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "blocked"}, {"id": "b", "state": "working"}}, 0)
	if m.NeedYou != 1 {
		t.Fatalf("NeedYou = %d", m.NeedYou)
	}
}

func TestAClosedRowIsDropped(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{
		{"id": "a", "state": "done", "closed": true},
		{"id": "b", "state": "working", "closed": false},
	}, 0)
	if len(m.Rows) != 1 || m.Rows[0].ID != "b" {
		t.Fatalf("rows = %+v, want only b", m.Rows)
	}
}

func TestApplyCopiesTheListFields(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{
		"id": "a", "label": "gate-refactor", "cwd": "/work/gate", "harness": "claude",
		"state": "working", "source": "gate", "detail": "editing", "quiet_for": 4.5,
	}, {
		"id": "b", "state": "idle", "source": nil,
	}}, 0)
	a := m.Rows[0]
	if a.Label != "gate-refactor" || a.Worktree != "/work/gate" || a.Harness != "claude" ||
		a.Source != "gate" || a.Line != "editing" || a.QuietFor != 4.5 {
		t.Fatalf("row a = %+v", a)
	}
	if m.Rows[1].Source != "" {
		t.Fatalf("a null source must be empty, got %q", m.Rows[1].Source)
	}
}

func TestLinePrefersTheAskSummaryWithTheToolPrefix(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{
		"id": "a", "state": "blocked", "detail": "ignored",
		"ask": map[string]any{"tool": "git", "summary": "push --force"},
	}, {
		"id": "b", "state": "blocked", "detail": "ignored",
		"ask": map[string]any{"summary": "rm -rf build"},
	}}, 0)
	if m.Rows[0].Line != "git: push --force" {
		t.Fatalf("line = %q", m.Rows[0].Line)
	}
	if m.Rows[1].Line != "rm -rf build" {
		t.Fatalf("line with no tool = %q", m.Rows[1].Line)
	}
}

func TestAgeIsNowMinusTS(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working", "ts": 100.0}, {"id": "b", "state": "working"}}, 160)
	if m.Rows[0].Age != 60 {
		t.Fatalf("age = %v, want 60", m.Rows[0].Age)
	}
	if m.Rows[1].Age != 0 {
		t.Fatalf("age with no ts = %v, want 0", m.Rows[1].Age)
	}
}

func TestTheCursorFollowsThePaneAcrossAReorder(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working"}, {"id": "b", "state": "working"}}, 0)
	m.Move(1)
	if r, ok := m.Selected(); !ok || r.ID != "b" {
		t.Fatalf("selected %+v %v, want b", r, ok)
	}
	m.Apply([]map[string]any{{"id": "b", "state": "blocked"}, {"id": "a", "state": "working"}}, 0)
	if r, ok := m.Selected(); !ok || r.ID != "b" {
		t.Fatalf("selected %+v %v after reorder, want b", r, ok)
	}
	if m.Cursor != 0 {
		t.Fatalf("cursor = %d, want 0 since b now leads NEEDS YOU", m.Cursor)
	}
}

func TestTheCursorClampsWhenRowsGoAway(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working"}, {"id": "b", "state": "working"}}, 0)
	m.Move(1)
	m.Apply([]map[string]any{{"id": "a", "state": "working"}}, 0)
	if r, ok := m.Selected(); !ok || r.ID != "a" || m.Cursor != 0 {
		t.Fatalf("selected %+v %v cursor %d, want a at 0", r, ok, m.Cursor)
	}
	m.Apply(nil, 0)
	if _, ok := m.Selected(); ok || m.Cursor != 0 {
		t.Fatalf("an empty floor selects nothing, cursor %d", m.Cursor)
	}
}

func TestMoveStaysInsideTheRows(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working"}, {"id": "b", "state": "working"}}, 0)
	m.Move(-1)
	if m.Cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.Cursor)
	}
	m.Move(5)
	if m.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.Cursor)
	}
}

func TestEventOnAKnownPaneChangesItsStateAndNeedYou(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working", "ts": 10.0}}, 70)
	m.Event(map[string]any{
		"event": "state", "pane": "a", "state": "blocked", "source": "gate", "detail": "",
		"ask": map[string]any{"tool": "Bash", "summary": "ls"},
	})
	r := m.Rows[0]
	if r.State != "blocked" || r.Source != "gate" || r.Line != "Bash: ls" || r.Age != 0 {
		t.Fatalf("row = %+v", r)
	}
	if m.NeedYou != 1 {
		t.Fatalf("NeedYou = %d", m.NeedYou)
	}
}

func TestEventOnAnUnknownPaneChangesNothing(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working"}}, 0)
	m.Event(map[string]any{"event": "state", "pane": "zzz", "state": "blocked", "source": "gate"})
	m.Event(map[string]any{"event": "state", "pane": nil, "state": "blocked", "source": "gate"})
	if len(m.Rows) != 1 || m.Rows[0].State != "working" || m.NeedYou != 0 {
		t.Fatalf("model changed: %+v", m)
	}
}

func TestApplyClosesThePeekWhenItsPaneIsGone(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working"}, {"id": "b", "state": "working"}}, 0)
	m.OpenPeek("text", nil)
	if m.Peek == nil || m.Peek.Pane != "a" {
		t.Fatalf("peek = %+v, want a", m.Peek)
	}
	m.Apply([]map[string]any{{"id": "b", "state": "working"}}, 0)
	if m.Peek != nil {
		t.Fatalf("peek still open on a pane that is gone: %+v", m.Peek)
	}
}

func TestApplyKeepsThePeekWhileItsPaneStays(t *testing.T) {
	var m Model
	m.Apply([]map[string]any{{"id": "a", "state": "working"}}, 0)
	m.OpenPeek("text", nil)
	m.Apply([]map[string]any{{"id": "a", "state": "blocked"}, {"id": "b", "state": "working"}}, 0)
	if m.Peek == nil || m.Peek.Pane != "a" {
		t.Fatalf("peek = %+v, want a", m.Peek)
	}
}
