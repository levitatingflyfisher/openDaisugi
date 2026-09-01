package pane

import (
	"testing"
	"time"
)

// A fresh grid has seen no output. After a write, LastWrite reports when
// the bytes landed.
func TestGridRecordsWhenItLastSawOutput(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, ok := g.LastWrite(); ok {
		t.Fatal("a fresh grid claims it has seen output")
	}
	before := time.Now()
	if _, err := g.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	last, ok := g.LastWrite()
	if !ok {
		t.Fatal("LastWrite reports no output after a write")
	}
	if last.Before(before) || last.After(time.Now()) {
		t.Fatalf("LastWrite %v is not between %v and now", last, before)
	}
}
