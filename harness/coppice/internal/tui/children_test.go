package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/tiles"
)

func listWithChild() []map[string]any {
	return []map[string]any{
		{"id": "w1:p1", "label": "floor", "harness": "claude", "state": "working", "ts": 10.0,
			"children": []any{map[string]any{"id": "a1", "label": "Explore", "state": "working", "ts": 11.0}}},
		{"id": "w1:p2", "label": "docs", "harness": "claude", "state": "working", "ts": 9.0},
	}
}

func TestAChildRowSitsUnderItsParent(t *testing.T) {
	m := &Model{}
	m.Apply(listWithChild(), 12)
	all := m.treeRows()
	if len(all) != 3 {
		t.Fatalf("rows = %+v, want 3", all)
	}
	if all[0].ID != "w1:p1" || all[1].Parent != "w1:p1" || all[1].Label != "Explore" || all[2].ID != "w1:p2" {
		t.Fatalf("order = %+v", all)
	}
	lines := Render(m, 80, 12)
	parentAt, childAt := -1, -1
	for i, l := range lines {
		plain := stripSGR(l)
		if strings.Contains(plain, "floor  claude") {
			parentAt = i
		}
		if strings.Contains(plain, "Explore") {
			childAt = i
		}
	}
	if parentAt < 0 || childAt != parentAt+1 {
		t.Fatalf("child line %d, parent line %d: %q", childAt, parentAt, lines)
	}
	parent := stripSGR(lines[parentAt])
	kid := stripSGR(lines[childAt])
	col := func(line, word string) int { return utf8.RuneCountInString(line[:strings.Index(line, word)]) }
	if col(kid, "Explore") != col(parent, "floor")+2 {
		t.Fatalf("the child is not indented two spaces under the parent:\n%q\n%q", parent, kid)
	}
}

func TestEnterOnAChildIsANoOpThatSaysWhy(t *testing.T) {
	m := &Model{Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	m.Apply(listWithChild(), 12)
	m.Cursor = m.indexOf(childID("w1:p1", "a1"), 0)
	f := testFloor(m, 140)
	for _, b := range []byte{'\r', ' ', 0x17} {
		if _, err := f.handle(key{kind: keyByte, b: b}); err != nil {
			t.Fatal(err)
		}
		if m.Message != ChildMessage {
			t.Fatalf("key %q: message %q, want %q", b, m.Message, ChildMessage)
		}
		if m.Typing != "" || m.Confirm != "" || m.Peek != nil {
			t.Fatalf("key %q acted on a child: typing %q confirm %q peek %v", b, m.Typing, m.Confirm, m.Peek)
		}
		for _, id := range m.Tiles.Slots {
			if id != "" {
				t.Fatalf("key %q put a child in a tile: %v", b, m.Tiles.Slots)
			}
		}
		m.Message = ""
	}
}

func TestAClickOnAChildFillsNoTile(t *testing.T) {
	m := &Model{Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	m.Apply(listWithChild(), 12)
	f := testFloor(m, 140)
	Render(m, 140, 20)
	y := 0
	for i, r := range m.RowAt {
		if r == m.indexOf(childID("w1:p1", "a1"), 0) {
			y = i + 1
		}
	}
	if y == 0 {
		t.Fatal("no child line on screen")
	}
	if err := f.click(Click{Button: 0, X: 3, Y: y}); err != nil {
		t.Fatal(err)
	}
	for _, id := range m.Tiles.Slots {
		if id != "" {
			t.Fatalf("a click put a child in a tile: %v", m.Tiles.Slots)
		}
	}
	if m.Message != ChildMessage {
		t.Fatalf("message %q", m.Message)
	}
}

func TestAChildEventUpdatesTheChildRow(t *testing.T) {
	m := &Model{}
	m.Apply(listWithChild(), 12)
	m.Child(map[string]any{"event": "child", "pane": "w1:p1", "child": "a1", "state": "done", "label": "Explore"})
	m.Child(map[string]any{"event": "child", "pane": "w1:p1", "child": "a2", "state": "working", "label": "Plan"})
	all := m.treeRows()
	if len(all) != 4 || all[1].State != "done" || all[2].Label != "Plan" || all[2].Parent != "w1:p1" {
		t.Fatalf("rows = %+v", all)
	}
}

func TestChildrenCountForNothingAndNextNeedSkipsThem(t *testing.T) {
	m := &Model{}
	list := listWithChild()
	list[0]["children"] = []any{map[string]any{"id": "a1", "label": "Explore", "state": "blocked"}}
	m.Apply(list, 12)
	if m.NeedYou != 0 {
		t.Fatalf("a blocked child counted as need you: %d", m.NeedYou)
	}
	m.Cursor = 0
	m.NextNeed()
	if r, _ := m.Selected(); r.Parent != "" {
		t.Fatalf("NextNeed landed on a child: %+v", r)
	}
	for _, id := range m.rosterOrder() {
		if strings.Contains(id, "/") {
			t.Fatalf("rosterOrder holds a child: %v", m.rosterOrder())
		}
	}
}

func TestZoomOnAChildSaysWhy(t *testing.T) {
	m := &Model{Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	m.Apply(listWithChild(), 12)
	m.Cursor = m.indexOf(childID("w1:p1", "a1"), 0)
	f := testFloor(m, 80)
	if err := f.runPrompt("zoom"); err != nil {
		t.Fatal(err)
	}
	if m.Message != ChildMessage {
		t.Fatalf("message %q, want %q", m.Message, ChildMessage)
	}
}
