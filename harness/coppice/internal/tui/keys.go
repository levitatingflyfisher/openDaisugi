package tui

import "strings"

// Action is what one rail key does.
type Action string

const (
	GoIn     Action = "goin"
	Window   Action = "window"
	New      Action = "new"
	NewIn    Action = "project"
	Rename   Action = "rename"
	Stop     Action = "stop"
	NextNeed Action = "next"
	Peek     Action = "peek"
	Talk     Action = "talk"
	Up       Action = "up"
	Journal  Action = "journal"
	Mode     Action = "mode"
	Swap     Action = "swap"
	None     Action = ""
)

// Binding is one rail key: its name as the footer shows it, what it
// does, and the word the footer prints beside it.
type Binding struct {
	Keys   string
	Action Action
	Help   string
}

// Table is the rail keys, in the order the footer shows them. The footer
// is printed from it, so the two can never differ. The talk key and j,
// which opens the journal, join the footer's end only when that costs no
// line; see footerLines.
func Table() []Binding {
	return []Binding{
		{"Enter", GoIn, "go in"},
		{"1-9", Window, "to window"},
		{"n", New, "new"},
		{"N", NewIn, "new in project"},
		{"r", Rename, "rename"},
		{"ctrl-w", Stop, "stop"},
		{"m", Mode, "mode"},
		{"shift-tab", NextNeed, "next need"},
		{"Space", Peek, "peek or fill"},
		{"ctrl-t", Talk, "talk"},
		{"Esc", Up, "up"},
	}
}

// Keys is the footer text on a row with no daisugi mode to set: every
// binding's keys and help, two spaces apart, but m. On an agent a gate
// hook reports for, the footer adds m mode (see footerText).
func Keys() string {
	return joinKeys(withoutMode(Table()))
}

// withoutMode is table without the m binding.
func withoutMode(table []Binding) []Binding {
	out := make([]Binding, 0, len(table))
	for _, b := range table {
		if b.Action != Mode {
			out = append(out, b)
		}
	}
	return out
}

// modeSettable is true for a live agent row a gate hook reports a mode
// for: the row m acts on.
func modeSettable(r Row) bool {
	return r.Parent == "" && (r.Mode == "enforcing" || r.Marker == "watching")
}

// joinKeys prints bindings the way the footer shows them.
func joinKeys(table []Binding) string {
	parts := make([]string, 0, len(table))
	for _, b := range table {
		parts = append(parts, b.Keys+" "+b.Help)
	}
	return strings.Join(parts, "  ")
}

// Bind returns the action one decoded keystroke binds to on the rail. Any
// other keystroke is None: an arrow, a click, or a key the rail does not
// use.
func Bind(k key) Action {
	switch k.kind {
	case keyShiftTab:
		return NextNeed
	case keyEsc:
		return Up
	case keyByte:
		switch k.b {
		case 0x14:
			return Talk
		case 0x17:
			return Stop
		case ' ':
			return Peek
		case '\r', '\n':
			return GoIn
		case 'n':
			return New
		case 'N':
			return NewIn
		case 'r':
			return Rename
		case 'j':
			return Journal
		case 'm':
			return Mode
		case 's':
			return Swap
		}
		if k.b >= '1' && k.b <= '9' {
			return Window
		}
	}
	return None
}
