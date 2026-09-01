package attach

import (
	"bytes"
	"testing"
)

func TestPlainBytesAreForwarded(t *testing.T) {
	k := NewKeyReader()
	act, out := k.Feed('x')
	if act != ActNone || !bytes.Equal(out, []byte{'x'}) {
		t.Fatalf("Feed('x') = %q %q, want no action and the byte forwarded", act, out)
	}
}

func TestLeaderThenNextIsAnAction(t *testing.T) {
	k := NewKeyReader()
	if act, out := k.Feed(Leader); act != ActNone || len(out) != 0 {
		t.Fatalf("the leader alone produced %q %q, want nothing yet", act, out)
	}
	act, out := k.Feed('n')
	if act != ActNext || len(out) != 0 {
		t.Fatalf("leader n = %q %q, want ActNext and no forwarded bytes", act, out)
	}
}

func TestEveryDocumentedLeaderKeyMaps(t *testing.T) {
	cases := map[byte]Action{
		'n': ActNext, 'p': ActPrev, 'c': ActCreate,
		'd': ActDetach, 'x': ActClose, '?': ActHelp,
	}
	for b, want := range cases {
		k := NewKeyReader()
		k.Feed(Leader)
		if act, _ := k.Feed(b); act != want {
			t.Fatalf("leader %q = %q, want %q", string(b), act, want)
		}
	}
}

// Leader twice sends one literal leader byte to the pane, which is how a user
// types ctrl+a inside the harness.
func TestLeaderTwiceForwardsOneLiteralLeader(t *testing.T) {
	k := NewKeyReader()
	k.Feed(Leader)
	act, out := k.Feed(Leader)
	if act != ActLiteral || !bytes.Equal(out, []byte{Leader}) {
		t.Fatalf("leader leader = %q %q, want one literal leader byte", act, out)
	}
}

// An unknown key after the leader is not swallowed. Swallowing input is the
// worst thing a terminal wrapper can do.
func TestUnknownLeaderKeyForwardsBothBytes(t *testing.T) {
	k := NewKeyReader()
	k.Feed(Leader)
	act, out := k.Feed('q')
	if act != ActNone || !bytes.Equal(out, []byte{Leader, 'q'}) {
		t.Fatalf("leader q = %q %q, want both bytes forwarded", act, out)
	}
}

// Every key HelpText advertises must map to an action the caller can act on.
// A help line for a key that does nothing is the control section 3.5 forbids.
func TestEveryKeyInHelpTextHasAnAction(t *testing.T) {
	for _, line := range []struct {
		key byte
		txt string
	}{
		{'n', "next pane"}, {'p', "previous pane"}, {'c', "create a pane"},
		{'d', "detach"}, {'x', "close"}, {'?', "this list"},
	} {
		if !bytes.Contains([]byte(HelpText), []byte("ctrl+a "+string(line.key))) {
			t.Fatalf("HelpText does not mention ctrl+a %s", string(line.key))
		}
		k := NewKeyReader()
		k.Feed(Leader)
		if act, _ := k.Feed(line.key); act == ActNone {
			t.Fatalf("ctrl+a %s is advertised but maps to no action", string(line.key))
		}
	}
}

func TestTheLeaderStateResetsAfterEveryAction(t *testing.T) {
	k := NewKeyReader()
	k.Feed(Leader)
	k.Feed('n')
	act, out := k.Feed('n')
	if act != ActNone || string(out) != "n" {
		t.Fatalf("the second n = %q %q, want it forwarded as plain input", act, out)
	}
}

// feedAll feeds every byte of s and returns every action and every byte
// the reader forwarded.
func feedAll(k *KeyReader, s string) ([]Action, []byte) {
	var acts []Action
	var out []byte
	for _, b := range []byte(s) {
		act, o := k.Feed(b)
		if act != ActNone {
			acts = append(acts, act)
		}
		out = append(out, o...)
	}
	return acts, out
}

func TestAMouseReportIsDroppedWhole(t *testing.T) {
	for _, s := range []string{"\x1b[<0;3;4M", "\x1b[<0;3;4m", "\x1b[<65;120;40M"} {
		k := NewKeyReader()
		acts, out := feedAll(k, s)
		if len(acts) != 0 || len(out) != 0 {
			t.Fatalf("%q gave actions %v and bytes %q, want nothing", s, acts, out)
		}
		if k.Pending() {
			t.Fatalf("%q left bytes held", s)
		}
	}
}

func TestBytesThatOnlyStartLikeAReportAreForwarded(t *testing.T) {
	k := NewKeyReader()
	if _, out := feedAll(k, "\x1bq"); !bytes.Equal(out, []byte("\x1bq")) {
		t.Fatalf("ESC q forwarded %q", out)
	}
	k = NewKeyReader()
	if _, out := feedAll(k, "\x1b[A"); !bytes.Equal(out, []byte("\x1b[A")) {
		t.Fatalf("ESC [ A forwarded %q", out)
	}
	k = NewKeyReader()
	if _, out := feedAll(k, "\x1b[<0;3x"); !bytes.Equal(out, []byte("\x1b[<0;3x")) {
		t.Fatalf("a broken report forwarded %q", out)
	}
	k = NewKeyReader()
	acts, out := feedAll(k, "\x1b\x01d")
	if !bytes.Equal(out, []byte("\x1b")) || len(acts) != 1 || acts[0] != ActDetach {
		t.Fatalf("ESC then leader d gave %v %q, want ESC forwarded and a detach", acts, out)
	}
}

func TestAHeldEscapeIsPendingUntilFlushed(t *testing.T) {
	k := NewKeyReader()
	if act, out := k.Feed(0x1b); act != ActNone || out != nil {
		t.Fatalf("ESC gave %q %q", act, out)
	}
	if !k.Pending() {
		t.Fatal("ESC is not pending")
	}
	if out := k.Flush(); !bytes.Equal(out, []byte{0x1b}) {
		t.Fatalf("Flush = %q", out)
	}
	if k.Pending() || k.Flush() != nil {
		t.Fatal("Flush did not clear the held bytes")
	}
}

func TestAnOverlongReportIsForwardedNotHeld(t *testing.T) {
	k := NewKeyReader()
	s := "\x1b[<" + string(bytes.Repeat([]byte("1"), 40))
	_, out := feedAll(k, s)
	if k.Pending() {
		t.Fatal("an overlong report is still held")
	}
	if !bytes.Equal(out, []byte(s)) {
		t.Fatalf("forwarded %q, want every byte", out)
	}
}
