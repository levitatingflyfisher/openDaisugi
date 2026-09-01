package layout

import (
	"errors"
	"testing"
)

// RemovePane drops a record the server could not start behind. The tab no
// longer lists it, Pane no longer finds it, and the workspace counter still
// moves on, so the removed id is never handed out again.
func TestRemovePaneDropsTheRecordButNeverReusesItsNumber(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, err := tr.CreateTab(ws.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	p1, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.RemovePane(p1.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.Pane(p1.ID); ok {
		t.Fatal("removed pane is still in the tree")
	}
	if got, _ := tr.Tab(tab.ID); len(got.PaneIDs) != 0 {
		t.Fatalf("tab still lists %v", got.PaneIDs)
	}
	if len(tr.Panes()) != 0 {
		t.Fatalf("Panes still lists %v", tr.Panes())
	}
	p2, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if p2.ID != "w1:p2" {
		t.Fatalf("pane after a removal is %s, want w1:p2", p2.ID)
	}
	if err := tr.RemovePane("w1:p9"); !errors.Is(err, ErrNoPane) {
		t.Fatalf("removing an unknown id gave %v, want ErrNoPane", err)
	}
}
