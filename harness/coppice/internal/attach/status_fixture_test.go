package attach

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
)

// statusCase is one row of testdata/attach/status-lines.json. The Herdr
// bridge tests read the same file, so a change to StatusLine that the
// bridge's rules do not follow fails there.
type statusCase struct {
	Pane       string `json:"pane"`
	Label      string `json:"label"`
	Harness    string `json:"harness"`
	State      string `json:"state"`
	Source     string `json:"source"`
	AskTool    string `json:"ask_tool"`
	AskSummary string `json:"ask_summary"`
	Cols       int    `json:"cols"`
	Leave      string `json:"leave"`
	Line       string `json:"line"`
	HerdrState string `json:"herdr_state"`
}

func TestStatusLineMatchesTheSharedFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "attach", "status-lines.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []statusCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the fixture has no rows")
	}
	for i, c := range cases {
		var ev *proto.PaneStateEvent
		if c.State != "" {
			ev = &proto.PaneStateEvent{State: c.State, Source: c.Source}
			if c.AskTool != "" {
				ev.Ask = &proto.Ask{Tool: c.AskTool, Summary: c.AskSummary}
			}
		}
		got := StatusLine(c.Pane, c.Label, c.Harness, ev, c.Cols, c.Leave)
		if got != c.Line {
			t.Errorf("row %d\n got %q\nwant %q", i, got, c.Line)
		}
	}
}
