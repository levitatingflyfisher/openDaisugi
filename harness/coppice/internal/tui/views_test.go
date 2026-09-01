package tui

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/opendaisugi/coppice/internal/tiles"
)

// A terminal cannot draw a page, so a view id typed alone names the place
// that can.
func TestAViewIdAlonePrintsWhereTheViewLives(t *testing.T) {
	m := &Model{Tiles: tiles.New(tiles.Focus, 1)}
	f := testFloor(m, 120)
	f.o.Views = []string{"tree", "minimap"}
	f.o.WebURL = "https://127.0.0.1:8443"
	if err := f.runPrompt("minimap"); err != nil {
		t.Fatal(err)
	}
	want := "views live on the floor page: https://127.0.0.1:8443/#/view/minimap"
	if m.Message != want {
		t.Fatalf("message %q, want %q", m.Message, want)
	}
}

func TestAViewIdWithTheWebServerOffSaysSo(t *testing.T) {
	m := &Model{Tiles: tiles.New(tiles.Focus, 1)}
	f := testFloor(m, 120)
	f.o.Views = []string{"minimap"}
	if err := f.runPrompt("minimap"); err != nil {
		t.Fatal(err)
	}
	want := "views live on the floor page, and the web server is off. Run: coppice web serve --persist"
	if m.Message != want {
		t.Fatalf("message %q, want %q", m.Message, want)
	}
}

func TestAViewIdIsPlumbingOnlyAlone(t *testing.T) {
	if Classify("minimap", Harnesses) != TalkLine {
		t.Fatal("a bare word with no views known must stay talk")
	}
	if ClassifyWith("minimap", Harnesses, []string{"minimap"}) != Plumbing {
		t.Fatal("a view id alone is plumbing")
	}
	if ClassifyWith("minimap shows the map", Harnesses, []string{"minimap"}) != TalkLine {
		t.Fatal("a sentence that starts with a view id is talk")
	}
}

// tree is a word the terminal draws itself. The shipped tree view does
// not take it over.
func TestTheTreeWordStillOpensTheTerminalTree(t *testing.T) {
	m := &Model{Tiles: tiles.New(tiles.Focus, 1)}
	f := testFloor(m, 120)
	f.o.Views = []string{"tree"}
	f.o.WebURL = "https://127.0.0.1:8443"
	if !f.viewWord([]string{"tree"}) {
		return
	}
	t.Fatal("tree was read as a view id")
}

// The web tree view draws the same lines. Both read one fixture.
func TestRenderTreeMatchesTheSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../web/static/_tests/fixtures/tree.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cols  int              `json:"cols"`
		Tasks []map[string]any `json:"tasks"`
		Panes []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			State string `json:"state"`
		} `json:"panes"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var rows []Row
	for _, p := range fx.Panes {
		rows = append(rows, Row{ID: p.ID, Label: p.Label, State: p.State})
	}
	got := RenderTree(DecodeTasks(fx.Tasks), rows, fx.Cols)
	if !reflect.DeepEqual(got, fx.Lines) {
		t.Fatalf("got\n%q\nwant\n%q", got, fx.Lines)
	}
}
