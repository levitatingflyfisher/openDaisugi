package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Herdr bridge writes testdata/attach/herdr-coppice.toml into Herdr's
// config. A Python test keeps that file equal to what the bridge writes.
// This test runs it through this port of Herdr's engine against every
// status line attach draws in testdata/attach/status-lines.json.
func TestTheHerdrBridgeManifestReadsEveryAttachStatusLine(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "attach")
	raw, err := os.ReadFile(filepath.Join(dir, "herdr-coppice.toml"))
	if err != nil {
		t.Fatal(err)
	}
	_, c, err := Parse("herdr-coppice.toml", raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "status-lines.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Line       string `json:"line"`
		HerdrState string `json:"herdr_state"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the fixture has no rows")
	}
	for i, r := range rows {
		// The status line sits on the last row, under the pane's own text.
		screen := "some pane text\nblocked via gate in the pane text\n" + r.Line + "\n"
		if got := c.Evaluate(Input{Screen: screen}).State; string(got) != r.HerdrState {
			t.Errorf("row %d %q: state %q, want %q", i, r.Line, got, r.HerdrState)
		}
	}
}
