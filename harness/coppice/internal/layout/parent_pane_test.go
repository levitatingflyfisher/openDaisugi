package layout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fork records its parent, and the record survives a save and a load. A
// pane with no parent writes no parent_pane key, so older layouts and
// ordinary panes look the same as before.
func TestParentPaneIsSavedAndLoaded(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("w", "/")
	tab, err := tr.CreateTab(ws.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := tr.CreatePane(ws.ID, tab.ID, Pane{Label: "p", Cwd: "/", Kind: KindHeadless})
	if err != nil {
		t.Fatal(err)
	}
	child, err := tr.CreatePane(ws.ID, tab.ID, Pane{Label: "c", Cwd: "/", Kind: KindHeadless, ParentPane: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "layout.json")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"parent_pane"`) != 1 {
		t.Fatalf("want exactly one parent_pane key in the saved layout, got:\n%s", raw)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.Pane(child.ID)
	if !ok || got.ParentPane != parent.ID {
		t.Fatalf("loaded child = %+v, want ParentPane %s", got, parent.ID)
	}
	if p, _ := loaded.Pane(parent.ID); p.ParentPane != "" {
		t.Fatalf("the parent gained a ParentPane: %+v", p)
	}
}
