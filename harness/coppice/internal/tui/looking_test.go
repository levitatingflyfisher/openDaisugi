package tui

import (
	"strings"
	"testing"
)

// A row another person has open says who looks at it.
func TestARowSaysWhoIsLooking(t *testing.T) {
	rows := RowsFrom([]map[string]any{
		{"id": "a", "label": "auth", "state": "working", "looking": []any{"alice", "bob"}},
		{"id": "b", "label": "docs", "state": "working"},
	}, 0)
	if got := strings.Join(rows[0].Looking, ","); got != "alice,bob" {
		t.Fatalf("Looking = %q", got)
	}
	if rows[1].Looking != nil {
		t.Fatalf("a row nobody looks at has %v", rows[1].Looking)
	}
	one := stripSGR(rowLine(Row{ID: "a", Label: "auth", State: "working", Looking: []string{"alice"}}, false, false, false, "", 80))
	if !strings.Contains(one, "· alice looking") {
		t.Fatalf("row line %q does not say alice is looking", one)
	}
	two := stripSGR(rowLine(rows[0], false, false, false, "", 80))
	if !strings.Contains(two, "· alice, bob looking") {
		t.Fatalf("row line %q does not name both", two)
	}
	none := stripSGR(rowLine(rows[1], false, false, false, "", 80))
	if strings.Contains(none, "looking") {
		t.Fatalf("row line %q names somebody", none)
	}
}
