package fleet

import (
	"fmt"
	"strings"
)

// Cockpit is grove's interactive controller: a selection cursor over the fleet
// and the key handling to move it and rule on the agent under it. Kept separate
// from the raw-terminal glue (cmd/grove) so the whole interaction is testable
// with synthetic keys — no live terminal, no live model.
//
// Keys (vim-style, one press each — Raskin: monotony, minimal keystrokes):
//
//	j / k   move the cursor down / up
//	a       approve the selected agent's flagged call (override the envelope)
//	d       deny it (uphold the envelope)
//	q       quit
type Cockpit struct {
	F   *Fleet
	Sel int
}

// Key handles one keypress. Returns quit=true on q (or Ctrl-C, byte 3).
func (c *Cockpit) Key(b byte) (quit bool) {
	jobs := c.F.Snapshot()
	n := len(jobs)
	switch b {
	case 'q', 3:
		return true
	case 'j', 'B': // 'B' = the final byte of the down-arrow escape sequence
		if c.Sel < n-1 {
			c.Sel++
		}
	case 'k', 'A':
		if c.Sel > 0 {
			c.Sel--
		}
	case 'a':
		if c.Sel < n && jobs[c.Sel].NeedsYou() {
			c.F.Approve(jobs[c.Sel].ID)
		}
	case 'd':
		if c.Sel < n && jobs[c.Sel].NeedsYou() {
			c.F.Deny(jobs[c.Sel].ID)
		}
	}
	return false
}

// RenderCockpit is the interactive frame: the minimap with the selected row
// marked by a '>' cursor (reverse-video when colour is on). A blocked selected
// agent shows the call it is waiting on, so you can rule with full context.
func RenderCockpit(jobs []Job, sel int, color bool) string {
	needs := 0
	for _, j := range jobs {
		if j.NeedsYou() {
			needs++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "grove — %d agents · %d need you\n", len(jobs), needs)
	for i, j := range jobs {
		cursor := "  "
		if i == sel {
			cursor = "> "
		}
		row := fmt.Sprintf("%s[%d] %-10s %s %-11s %s", cursor, i, trunc(j.ID, 10), glyphFor(j.State), j.State, trunc(j.Task, 36))
		if color {
			code := colorFor(j.State)
			if i == sel {
				code = "7;" + code // reverse video for the selected row
			}
			row = "\x1b[" + code + "m" + row + "\x1b[0m"
		}
		b.WriteString(row + "\n")
	}
	return b.String() + "\nkeys: j/k move · a approve · d deny · q quit\n"
}
