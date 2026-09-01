package pane

import "testing"

// A program turns bracketed paste on and off with a mode sequence. The grid
// remembers the last one it saw, so a sender knows whether a paste will be
// read as one block.
func TestGridTracksBracketedPasteMode(t *testing.T) {
	g, err := NewGrid(40, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.PasteMode() {
		t.Fatal("a new grid reports paste mode on")
	}
	_, _ = g.Write([]byte("hello \x1b[?2004h> "))
	if !g.PasteMode() {
		t.Fatal("paste mode is off after the program turned it on")
	}
	_, _ = g.Write([]byte("more text"))
	if !g.PasteMode() {
		t.Fatal("plain text turned paste mode off")
	}
	_, _ = g.Write([]byte("\x1b[?2004hx\x1b[?2004l"))
	if g.PasteMode() {
		t.Fatal("the last sequence turned paste mode off, but it reads on")
	}
}

// FirstWrite keeps the time of the first bytes, however many come after.
func TestGridKeepsTheFirstWrite(t *testing.T) {
	g, err := NewGrid(40, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, ok := g.FirstWrite(); ok {
		t.Fatal("a new grid has a first write")
	}
	_, _ = g.Write([]byte("a"))
	first, ok := g.FirstWrite()
	if !ok {
		t.Fatal("no first write after a write")
	}
	_, _ = g.Write([]byte("b"))
	again, _ := g.FirstWrite()
	last, _ := g.LastWrite()
	if !again.Equal(first) || last.Before(first) {
		t.Fatalf("first %v then %v, last %v", first, again, last)
	}
}
