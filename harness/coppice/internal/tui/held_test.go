package tui

import (
	"strings"
	"testing"
)

// heldList is a pane.list with one ask a foreman holds and one it does
// not.
func heldList() []map[string]any {
	return []map[string]any{
		{"id": "w1:p1", "label": "writer", "state": "blocked", "ts": 100.0, "task": "t2",
			"ask":  map[string]any{"id": "ask-1", "summary": "rm -rf build/", "tier": "undoable"},
			"held": map[string]any{"by": "w1:p9", "task": "t1", "task_label": "review-team", "since": 40.0, "until": 160.0}},
		{"id": "w1:p2", "label": "loner", "state": "blocked", "ts": 100.0,
			"ask": map[string]any{"id": "ask-2", "summary": "git push", "tier": "permanent"}},
	}
}

func TestAHeldAskRendersUnderWorkingAndNotUnderNeedsYou(t *testing.T) {
	m := &Model{}
	m.Apply(heldList(), 100)
	if m.NeedYou != 1 {
		t.Fatalf("NeedYou = %d, want 1: a held ask is not the operator's yet", m.NeedYou)
	}
	secs := Group(m.Rows)
	if len(secs[0].Rows) != 1 || secs[0].Rows[0].ID != "w1:p2" {
		t.Fatalf("NEEDS YOU = %+v", secs[0].Rows)
	}
	if len(secs[1].Rows) != 1 || secs[1].Rows[0].ID != "w1:p1" {
		t.Fatalf("WORKING = %+v", secs[1].Rows)
	}
	text := stripSGR(strings.Join(Render(m, 100, 20), "\n"))
	if !strings.Contains(text, "waiting on review-team's foreman · 1m") {
		t.Fatalf("no waiting line:\n%s", text)
	}
	if !strings.Contains(text, "▾ t2  1 working") || !strings.Contains(text, "▾ no directory  1 needs") {
		t.Fatalf("the held row does not count as working in its group:\n%s", text)
	}
}

// A state event with no held field ends the hold on the row: that is how
// the ask reaches NEEDS YOU at the deadline.
func TestAnEventWithNoHoldMovesTheAskToNeedsYou(t *testing.T) {
	m := &Model{}
	m.Apply(heldList(), 100)
	m.Event(map[string]any{"pane": "w1:p1", "state": "blocked", "source": "gate",
		"ask": map[string]any{"id": "ask-1", "summary": "rm -rf build/"}})
	if m.NeedYou != 2 {
		t.Fatalf("NeedYou = %d, want 2", m.NeedYou)
	}
	m.Event(map[string]any{"pane": "w1:p1", "state": "blocked", "source": "gate",
		"ask":  map[string]any{"id": "ask-1", "summary": "rm -rf build/"},
		"held": map[string]any{"by": "w1:p9", "task": "t1", "task_label": "review-team", "since": 40.0}})
	if m.NeedYou != 1 {
		t.Fatalf("a held event counted: NeedYou = %d", m.NeedYou)
	}
}
