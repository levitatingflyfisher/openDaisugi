package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/tiles"
)

// stackFixture is a team with one child task, and a task beside it. One
// pane in the team's child and one in the other task are blocked.
func stackFixture() *Model {
	return &Model{
		Tasks: []TaskRow{
			{ID: "t1", Label: "review-team"},
			{ID: "t2", Label: "docs", Parent: "t1", Panes: []string{"w1:p1", "w1:p2"}},
			{ID: "t3", Label: "other", Panes: []string{"w1:p3"}},
		},
		Rows: []Row{
			{ID: "w1:p1", Label: "writer", Task: "t2", State: "blocked"},
			{ID: "w1:p2", Label: "checker", Task: "t2", State: "working"},
			{ID: "w1:p3", Label: "loner", Task: "t3", State: "blocked"},
			{ID: "w1:p4", Label: "loose", State: "working"},
		},
	}
}

func TestPushScopesTheRosterAndPopRestoresIt(t *testing.T) {
	m := stackFixture()
	m.recount()
	if got := bar(m); !strings.Contains(got, "coppice · 2 need you") {
		t.Fatalf("top bar = %q", got)
	}
	m.Push("t1")
	if !reflect.DeepEqual(m.Path, []string{"t1"}) {
		t.Fatalf("path = %v", m.Path)
	}
	ids := m.rosterOrder()
	if len(ids) != 2 || !contains(ids, "w1:p1") || !contains(ids, "w1:p2") {
		t.Fatalf("pushed roster = %v, want the team's two panes", ids)
	}
	if got := bar(m); got != "coppice › review-team · 1 needs you" {
		t.Fatalf("pushed bar = %q", got)
	}
	m.Push("t2")
	if !reflect.DeepEqual(m.Path, []string{"t1", "t2"}) {
		t.Fatalf("path after a second push = %v", m.Path)
	}
	if got := bar(m); !strings.HasPrefix(got, "coppice › review-team › docs") {
		t.Fatalf("deep bar = %q", got)
	}
	m.Pop()
	m.Pop()
	if len(m.Path) != 0 || len(m.rosterOrder()) != 4 {
		t.Fatalf("after two pops: path=%v roster=%v", m.Path, m.rosterOrder())
	}
	if got := bar(m); got != "coppice · 2 need you" {
		t.Fatalf("bar after pops = %q", got)
	}
	m.Pop()
	if len(m.Path) != 0 {
		t.Fatal("a pop on the top floor did something")
	}
}

// Push from anywhere names the whole chain, so the bar always reads from
// the floor down. A task the model does not know pushes nothing.
func TestPushNamesTheWholeChain(t *testing.T) {
	m := stackFixture()
	m.Push("t2")
	if !reflect.DeepEqual(m.Path, []string{"t1", "t2"}) {
		t.Fatalf("path = %v", m.Path)
	}
	m.Push("t9")
	if !reflect.DeepEqual(m.Path, []string{"t1", "t2"}) {
		t.Fatalf("an unknown task changed the path: %v", m.Path)
	}
}

func TestPushClosesAPeekItHides(t *testing.T) {
	m := stackFixture()
	m.Peek = &PeekView{Pane: "w1:p3"}
	m.Push("t1")
	if m.Peek != nil {
		t.Fatal("a peek on a hidden pane stayed open")
	}
	m.Peek = &PeekView{Pane: "w1:p1"}
	m.Push("t2")
	if m.Peek == nil {
		t.Fatal("a peek on a shown pane was closed")
	}
}

// Enter on a team line in the tree pushes. Esc then goes up one level at
// a time: the tree, then the path.
func TestTreeEnterPushesAndEscPops(t *testing.T) {
	m := stackFixture()
	f := &floor{m: m, o: Options{Size: func() (int, int) { return 80, 24 }}}
	m.Tree = NewTreeView(m.Tasks, m.Rows, 80)
	if m.Tree.Selected() != "t1" {
		t.Fatalf("tree opens on %q", m.Tree.Selected())
	}
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	if m.Tree != nil || !reflect.DeepEqual(m.Path, []string{"t1"}) {
		t.Fatalf("after Enter: tree=%v path=%v", m.Tree != nil, m.Path)
	}
	lines := Render(m, 80, 24)
	if plain := stripSGR(lines[0]); !strings.Contains(plain, "coppice › review-team · 1 needs you") {
		t.Fatalf("bar = %q", plain)
	}
	body := stripSGR(strings.Join(lines, "\n"))
	if !strings.Contains(body, "writer") || strings.Contains(body, "loner") {
		t.Fatalf("pushed roster:\n%s", body)
	}
	// A tree opened while pushed shows only the scope's subtree.
	m.Tree = m.NewTree(80)
	text := strings.Join(m.Tree.Lines, "\n")
	if !strings.Contains(text, "docs") || strings.Contains(text, "other") {
		t.Fatalf("scoped tree:\n%s", text)
	}
	handleAll(t, f, key{kind: keyEsc})
	if m.Tree != nil || len(m.Path) != 1 {
		t.Fatalf("after one Esc: tree=%v path=%v", m.Tree != nil, m.Path)
	}
	handleAll(t, f, key{kind: keyEsc})
	if len(m.Path) != 0 {
		t.Fatalf("after two Esc: path=%v", m.Path)
	}
	body = stripSGR(strings.Join(Render(m, 80, 24), "\n"))
	if !strings.Contains(body, "loner") {
		t.Fatalf("the full roster did not come back:\n%s", body)
	}
	// Enter on an empty tree closes it and pushes nothing.
	m.Tree = NewTreeView(nil, nil, 80)
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	if m.Tree != nil || len(m.Path) != 0 {
		t.Fatalf("Enter on an empty tree: tree=%v path=%v", m.Tree != nil, m.Path)
	}
}

// A task that leaves the list takes itself and every level under it off
// the path, and the message line says so.
func TestApplyDropsAPathTaskThatIsGone(t *testing.T) {
	m := stackFixture()
	m.Push("t2")
	m.Tasks = m.Tasks[:1]
	m.Apply(nil, 0)
	if !reflect.DeepEqual(m.Path, []string{"t1"}) {
		t.Fatalf("path = %v", m.Path)
	}
	if !strings.Contains(m.Message, "gone") {
		t.Fatalf("message = %q", m.Message)
	}
}

// foreman with a harness while pushed opens that task's foreman: a pane in
// the task, named for it, that the task names as its foreman. The floor's
// own foreman in the config file stays as it was.
func TestForemanWhilePushedNamesTheTaskForeman(t *testing.T) {
	s := newTestServer(t)
	foremanConfig(t)
	teamDir := t.TempDir()
	team := createTask(t, s.Socket(), map[string]any{"label": "review-team", "cwd": teamDir})
	if _, err := runFloor(t, s, scripted("\x14tree\r", "\r", "\x14foreman pi\r", "\x03")); err != nil {
		t.Fatal(err)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	rows, _ := res["panes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("panes = %v, want one", rows)
	}
	row, _ := rows[0].(map[string]any)
	id, _ := row["id"].(string)
	if row["label"] != "review-team-foreman" || row["task"] != team || row["cwd"] != teamDir {
		t.Fatalf("row = %v", row)
	}
	tasks := rawCall(t, s.Socket(), "task.list", nil)
	list, _ := tasks["tasks"].([]any)
	tk, _ := list[0].(map[string]any)
	if tk["foreman"] != id {
		t.Fatalf("task row = %v, want foreman %s", tk, id)
	}
	facts := rawCall(t, s.Socket(), "floor.facts", nil)
	if facts["foreman"] != nil {
		t.Fatalf("floor foreman = %v: a task foreman changed the floor's", facts["foreman"])
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		read := rawCall(t, s.Socket(), "pane.read", map[string]any{"pane": id})
		text, _ := read["text"].(string)
		text = strings.ReplaceAll(text, "\n", "")
		if strings.Contains(text, "Say ready.") {
			// The pane wraps, and a wrap drops the space at the break, so
			// the test reads the line up to its first sentence end.
			if !strings.Contains(text, "You are the foreman of task review-team.") {
				t.Fatalf("the pane got %q", text)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the promotion line never reached the pane: %v", read)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Enter on a pane row while pushed gives the keyboard to the row the
// operator sees, not to the row at the same place in the full roster.
func TestEnterOnAPaneRowWhilePushedTypesInThatPane(t *testing.T) {
	m := stackFixture()
	m.Tiles = tiles.New(tiles.Focus, 1)
	m.Screens = map[string]*attach.Screen{}
	f := testFloor(m, 140)
	m.Push("t2")
	m.Move(1)
	want, _ := m.Selected()
	if want.ID != "w1:p2" {
		t.Fatalf("the second pushed row is %q, want w1:p2", want.ID)
	}
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	if m.Typing != "w1:p2" {
		t.Fatalf("typing = %q, want w1:p2", m.Typing)
	}
}

// The message after a promotion says where the operator's sentences go,
// and a task foreman does not claim them.
func TestThePromotedMessageFitsTheForeman(t *testing.T) {
	if got := promotedMessage(ForemanLabel, ""); got != "foreman has its page. Type a sentence and it goes there." {
		t.Fatalf("floor message = %q", got)
	}
	got := promotedMessage("review-team-foreman", "review-team")
	if strings.Contains(got, "Type a sentence") || !strings.Contains(got, "review-team") ||
		!strings.Contains(got, "first") {
		t.Fatalf("task message = %q", got)
	}
}
