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
	None     Action = ""
)

// Binding is one rail key: its name as the footer shows it, what it
// does, and the word the footer prints beside it.
type Binding struct {
	Keys   string
	Action Action
	Help   string
}

// Table is the rail keys, in the order the footer shows them. Every key
// the rail answers to is here, and the footer is printed from it, so the
// two can never differ.
func Table() []Binding {
	return []Binding{
		{"Enter", GoIn, "go in"},
		{"1-9", Window, "to window"},
		{"n", New, "new"},
		{"N", NewIn, "new in project"},
		{"r", Rename, "rename"},
		{"ctrl-w", Stop, "stop"},
		{"shift-tab", NextNeed, "next need"},
		{"Space", Peek, "peek or fill"},
		{"ctrl-t", Talk, "talk"},
		{"Esc", Up, "up"},
	}
}

// Keys is the footer text: every binding's keys and help, two spaces
// apart.
func Keys() string {
	return joinKeys(Table())
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
		}
		if k.b >= '1' && k.b <= '9' {
			return Window
		}
	}
	return None
}
