package tui

import (
	"regexp"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestAnActIsAVersion4UUID(t *testing.T) {
	a, b := newAct(), newAct()
	if !uuidRe.MatchString(a) || a == b {
		t.Fatalf("acts = %q, %q", a, b)
	}
}

func TestASentenceKeepsItsActUntilDone(t *testing.T) {
	f := &floor{}
	a := f.actFor("build it")
	if f.actFor("build it") != a {
		t.Fatal("the id changed for the same sentence")
	}
	if f.actFor("another") == a {
		t.Fatal("a new sentence kept the old id")
	}
	b := f.actFor("another")
	f.actDone()
	if f.actFor("another") == b {
		t.Fatal("the id survived done")
	}
}
