package tui

import (
	"strings"
	"testing"
)

// FN-R-13: a fenced run that changed a watched workspace file shows on its
// rail row, live or ended.
func TestTheRailShowsAWorkspaceAFencedRunChanged(t *testing.T) {
	r := Row{ID: "w1:p1", Label: "auth", State: "idle", Changed: []string{".git/hooks/pre-commit"}}
	if line := stripSGR(rowLine(r, false, false, false, "", 100)); !strings.Contains(line, "workspace config changed: .git/hooks/pre-commit") {
		t.Errorf("row line %q", line)
	}
	rows := endedRowsFrom([]map[string]any{{"id": "w1:p1", "closed": true,
		"workspace_changed": []any{".git/config"}}}, 0)
	if len(rows) != 1 || len(rows[0].Changed) != 1 || rows[0].Changed[0] != ".git/config" {
		t.Errorf("ended rows %+v", rows)
	}
	if line := stripSGR(rowLine(Row{ID: "x", Label: "x"}, false, false, false, "", 100)); strings.Contains(line, "changed") {
		t.Errorf("a clean row says changed: %q", line)
	}
}
