package fleet

import (
	"fmt"
	"strings"
)

// The minimap is grove's whole point: the fleet's health on one screen, read at a
// glance. Following the tool-interface doctrine — color IS the data (htop), the
// keybindings are printed in the chrome (lazygit), and the header answers the one
// question the view exists for (Raskin: keep the locus of attention on the
// decision — who needs me now — not on the plumbing).

func colorFor(state string) string {
	switch {
	case strings.HasPrefix(state, "tool:"):
		return "32" // green — actively working
	case state == "running":
		return "32"
	case state == "blocked":
		return "33" // amber — the gate refused a call; needs your ruling
	case state == "done":
		return "36" // cyan
	case state == "failed":
		return "31" // red
	default:
		return "90" // dim — idle
	}
}

func glyphFor(state string) string {
	switch {
	case strings.HasPrefix(state, "tool:"), state == "running":
		return "▶"
	case state == "blocked":
		return "!"
	case state == "done":
		return "✓"
	case state == "failed":
		return "✗"
	default:
		return "·"
	}
}

// RenderMinimap draws the fleet as one glanceable frame. When color is true each
// row is wrapped in the ANSI code for its state.
func RenderMinimap(jobs []Job, color bool) string {
	needs := 0
	for _, j := range jobs {
		if j.NeedsYou() {
			needs++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "grove — %d agents · %d need you\n", len(jobs), needs)
	for i, j := range jobs {
		row := fmt.Sprintf("  [%d] %-10s %s %-11s %s", i, trunc(j.ID, 10), glyphFor(j.State), j.State, trunc(j.Task, 40))
		if color {
			row = "\x1b[" + colorFor(j.State) + "m" + row + "\x1b[0m"
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\nkeys: ↑↓/jk move · enter warp · a approve · d deny · q quit\n")
	return b.String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
