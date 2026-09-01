package tui

import "strings"

// Key is one chord the floor answers to and what it does.
type Key struct {
	Chord string
	Does  string
}

// Keys is the floor's key map, in the order the footer shows it.
func Keys() []Key {
	return []Key{
		{"↑↓", "move"},
		{"Enter", "attach"},
		{"Space", "tile or peek"},
		{"t", "open tile"},
		{"Esc", "close"},
		{"q", "quit"},
	}
}

// Hints is the footer line: every key and what it does, two spaces apart.
func Hints() string {
	parts := make([]string, 0, len(Keys()))
	for _, k := range Keys() {
		parts = append(parts, k.Chord+" "+k.Does)
	}
	return strings.Join(parts, "  ")
}
