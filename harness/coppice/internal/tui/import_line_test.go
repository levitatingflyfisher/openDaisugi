package tui

import (
	"strings"
	"testing"
)

// FM-R-85: the bar says "importing done/total" while the foreman imports.
func TestTheBarShowsTheImportProgress(t *testing.T) {
	m := chatModel()
	if bar := barLine(m, 120); strings.Contains(bar, "import") {
		t.Fatalf("a bar with no import says %q", bar)
	}
	m.ImportLine = importLine(map[string]any{"state": "running", "done": float64(12), "total": float64(340)})
	if bar := barLine(m, 120); !strings.HasSuffix(bar, " · importing 12/340") && !strings.Contains(bar, "importing 12/340") {
		t.Fatalf("bar = %q", bar)
	}
	joined := stripSGR(strings.Join(Render(m, 120, 30), "\n"))
	if !strings.Contains(joined, "importing 12/340") {
		t.Fatalf("the floor lacks the import progress:\n%s", joined)
	}
}

func TestImportLineWords(t *testing.T) {
	for _, c := range []struct {
		state string
		want  string
	}{{"running", "importing 3/9"}, {"paused", "import paused 3/9"}, {"done", ""}, {"off", ""}, {"declined", ""}} {
		got := importLine(map[string]any{"state": c.state, "done": float64(3), "total": float64(9)})
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.state, got, c.want)
		}
	}
}
