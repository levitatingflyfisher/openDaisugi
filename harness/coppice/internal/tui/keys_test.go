package tui

import (
	"strings"
	"testing"
)

// bind decodes seq through readKey and returns the one action it binds
// to, or None.
func bind(t *testing.T, seq string) Action {
	t.Helper()
	ks, closed := readKey(feed(seq), seq[0])
	if closed {
		t.Fatalf("%q closed the keys", seq)
	}
	got := None
	for _, k := range ks {
		if a := Bind(k); a != None {
			got = a
		}
	}
	return got
}

func TestTheRailKeysMapAndNothingElseDoes(t *testing.T) {
	cases := map[string]Action{
		"\x14": Talk, "\x1b[Z": NextNeed, "\x17": Stop, " ": Peek, "\r": GoIn, "\n": GoIn,
		"n": New, "N": NewIn, "r": Rename, "1": Window, "9": Window,
	}
	for seq, want := range cases {
		if got := bind(t, seq); got != want {
			t.Fatalf("%q -> %v, want %v", seq, got, want)
		}
	}
	for _, b := range []byte("qpcdxt?o0\x01\x03\x7f") {
		if a := bind(t, string(b)); a != None {
			t.Fatalf("%q must be text, got %v", b, a)
		}
	}
	for _, seq := range []string{"\x1b[A", "\x1b[B", "\x1b[<0;3;4M"} {
		if a := bind(t, seq); a != None {
			t.Fatalf("%q must not bind, got %v", seq, a)
		}
	}
}

func TestLoneEscIsUpAfterTheWait(t *testing.T) {
	if a := bind(t, "\x1b"); a != Up {
		t.Fatalf("a lone esc gave %v, want Up", a)
	}
}

func TestFooterComesFromTheTable(t *testing.T) {
	f := Keys()
	for _, b := range Table() {
		if !strings.Contains(f, b.Keys+" "+b.Help) {
			t.Fatalf("footer %q lacks %q", f, b.Keys+" "+b.Help)
		}
	}
	if len(Table()) != 10 {
		t.Fatalf("%d bindings, want 10", len(Table()))
	}
	if strings.Contains(f, "q quit") || strings.Contains(f, "t open") {
		t.Fatalf("footer names a key that is text now: %q", f)
	}
}

func TestEveryBindingInTheTableHasAnAction(t *testing.T) {
	seen := map[Action]bool{}
	for _, b := range Table() {
		if b.Action == None || b.Keys == "" || b.Help == "" {
			t.Fatalf("binding %+v is not whole", b)
		}
		if seen[b.Action] {
			t.Fatalf("action %v bound twice", b.Action)
		}
		seen[b.Action] = true
	}
}
