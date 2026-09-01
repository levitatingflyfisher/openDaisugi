// Package tmuxmirror keeps one tmux window per open coppice pane. Each
// window runs coppice attach on its pane, so keys reach the pane. The
// mirror creates, renames and kills its own windows and sets the session's
// status-right to the roster count. It never touches a window it did not
// create.
//
// Plan and StatusLine are pure. control.go drives tmux in control mode.
package tmuxmirror

import (
	"fmt"
	"strings"
	"unicode"
)

// MaxName is the longest window name the mirror sets, in runes.
const MaxName = 30

// MarkOption is the tmux window option that holds the pane id of a window
// the mirror created. A window without it belongs to the user.
const MarkOption = "@coppice_pane"

// Pane is one row of pane.list, as far as the mirror reads it.
type Pane struct {
	ID     string
	Label  string
	State  string
	Closed bool
	// Held is true while a foreman hears the pane's ask first.
	Held bool
}

// Window is one tmux window. Pane is the mark the mirror set on it, and is
// empty for a window the mirror did not create.
type Window struct {
	ID   string
	Name string
	Pane string
}

// OpKind names what an Op does to a window.
type OpKind string

const (
	OpCreate OpKind = "create"
	OpRename OpKind = "rename"
	OpKill   OpKind = "kill"
)

// Op is one change to the tmux windows. A create has no Window yet.
type Op struct {
	Kind   OpKind
	Window string
	Pane   string
	Name   string
}

// WindowName is the name of a pane's window: its label with control runes
// dropped, or its id when the label is empty, cut to MaxName runes.
func WindowName(p Pane) string {
	name := clean(p.Label)
	if name == "" {
		name = clean(p.ID)
	}
	r := []rune(name)
	if len(r) > MaxName {
		name = string(r[:MaxName-1]) + "…"
	}
	return name
}

// clean drops every control rune and every rune that is not printable.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || (!unicode.IsPrint(r) && r != ' ') || r == unicode.ReplacementChar {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// Plan returns the ops that make the mirror's windows match the open
// panes: one window per open pane, named for it. It kills a window whose
// pane is closed or gone, and a second window for one pane. Renames and
// kills come first, in window order, then creates, in pane order. A window
// with no mark is never in an op.
func Plan(panes []Pane, windows []Window) []Op {
	open := map[string]Pane{}
	for _, p := range panes {
		if !p.Closed && p.ID != "" {
			open[p.ID] = p
		}
	}
	var ops []Op
	has := map[string]bool{}
	for _, w := range windows {
		if w.Pane == "" {
			continue
		}
		p, ok := open[w.Pane]
		if !ok || has[w.Pane] {
			ops = append(ops, Op{Kind: OpKill, Window: w.ID, Pane: w.Pane})
			continue
		}
		has[w.Pane] = true
		if name := WindowName(p); name != w.Name {
			ops = append(ops, Op{Kind: OpRename, Window: w.ID, Pane: w.Pane, Name: name})
		}
	}
	for _, p := range panes {
		if p.Closed || p.ID == "" || has[p.ID] {
			continue
		}
		has[p.ID] = true
		ops = append(ops, Op{Kind: OpCreate, Pane: p.ID, Name: WindowName(p)})
	}
	return ops
}

// StatusLine is the roster count for status-right: how many open panes need
// you and how many work, or quiet when none does either. A blocked pane
// whose ask a foreman holds counts as working.
func StatusLine(panes []Pane) string {
	need, working := 0, 0
	for _, p := range panes {
		if p.Closed {
			continue
		}
		switch {
		case p.State == "blocked" && !p.Held:
			need++
		case p.State == "working", p.State == "blocked":
			working++
		}
	}
	var parts []string
	if need > 0 {
		parts = append(parts, fmt.Sprintf("%d need you", need))
	}
	if working > 0 {
		parts = append(parts, fmt.Sprintf("%d working", working))
	}
	if len(parts) == 0 {
		return "quiet"
	}
	return strings.Join(parts, " · ")
}

// PanesFromList reads the rows of a pane.list result. A row with no id is
// skipped.
func PanesFromList(res map[string]any) []Pane {
	rows, _ := res["panes"].([]any)
	var out []Pane
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		label, _ := m["label"].(string)
		state, _ := m["state"].(string)
		closed, _ := m["closed"].(bool)
		_, held := m["held"].(map[string]any)
		out = append(out, Pane{ID: id, Label: label, State: state, Closed: closed, Held: held})
	}
	return out
}
