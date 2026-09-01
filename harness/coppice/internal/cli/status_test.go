package cli

import (
	"strings"
	"testing"
)

// JSON hands every number back as float64. A pid, a count or an uptime must
// still print as the integer it is, at every size a real value reaches: a
// seven-digit pid is ordinary on Linux, and %v would print it in exponent
// form.
func TestStatusPrintsAnIntegerPid(t *testing.T) {
	out := statusText(map[string]any{
		"pid": float64(1234567), "socket": "/x", "panes": float64(2), "panes_live": float64(1),
		"uptime_s": float64(1000000),
	})
	for _, want := range []string{"pid 1234567\n", "panes 2, 1 live\n", "up 1000000s\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status text %q is missing %q", out, want)
		}
	}
}

// server.status's resumable count prints when the reply carries one; a
// caller talking to an older server that never sent it gets no stray line.
func TestStatusPrintsResumableWhenPresent(t *testing.T) {
	out := statusText(map[string]any{
		"pid": float64(1), "socket": "/x", "panes": float64(2), "panes_live": float64(1),
		"uptime_s": float64(5), "resumable": float64(3),
	})
	if !strings.Contains(out, "3 resumable\n") {
		t.Fatalf("status text %q is missing the resumable count", out)
	}
}

func TestStatusOmitsResumableWhenAbsent(t *testing.T) {
	out := statusText(map[string]any{
		"pid": float64(1), "socket": "/x", "panes": float64(2), "panes_live": float64(1),
		"uptime_s": float64(5),
	})
	if strings.Contains(out, "resumable") {
		t.Fatalf("status text %q names resumable with no such key in the reply", out)
	}
}

// A value that is not a whole number keeps its fraction: the formatter
// prints integers as integers and nothing else changes.
func TestStatusKeepsAFractionWhenThereIsOne(t *testing.T) {
	if got := formatNumber(2.5); got != "2.5" {
		t.Fatalf("formatNumber(2.5) = %q, want 2.5", got)
	}
	if got := formatNumber("text"); got != "text" {
		t.Fatalf("formatNumber(\"text\") = %q, want text", got)
	}
}
