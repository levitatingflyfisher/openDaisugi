package attach

import (
	"bytes"
	"testing"
	"time"
)

func TestOnlyTheLeaveKeyIsStolen(t *testing.T) {
	k := NewKeyReader(0x1d)
	if a, fwd := k.Feed(0x01); a != ActNone || !bytes.Equal(fwd, []byte{0x01}) {
		t.Fatal("ctrl-a must forward now")
	}
	if a, fwd := k.Feed(0x1d); a != ActNone || fwd != nil {
		t.Fatal("ctrl-] is held, not forwarded")
	}
	if !k.Pending() || k.Hold() != leaveHold {
		t.Fatalf("a held leave key must be pending for %v, got pending %v hold %v", leaveHold, k.Pending(), k.Hold())
	}
	if a, _ := k.Flush(); a != ActDetach {
		t.Fatal("a lone ctrl-] leaves once the hold expires")
	}
	if k.Pending() || k.Hold() != 0 {
		t.Fatal("nothing is held after the flush")
	}
	for _, b := range []byte{0x14, 0x17, 'n', 'p', 'd', 'x', '?', 'q'} {
		if a, fwd := k.Feed(b); a != ActNone || !bytes.Equal(fwd, []byte{b}) {
			t.Fatalf("%x must forward, got %q %q", b, a, fwd)
		}
	}
}

func TestTheLeaveKeyTwiceGoesToThePane(t *testing.T) {
	k := NewKeyReader(0x1d)
	if a, fwd := k.Feed(0x1d); a != ActNone || fwd != nil {
		t.Fatal("first press is held")
	}
	if a, fwd := k.Feed(0x1d); a != ActNone || !bytes.Equal(fwd, []byte{0x1d}) {
		t.Fatal("second press forwards one leave byte")
	}
	if a, fwd := k.Flush(); a != ActNone || fwd != nil {
		t.Fatal("nothing is held after the pair")
	}
}

// A byte that is not the leave key, arriving while the leave key is held,
// ends the hold as a detach. That byte is dropped.
func TestAnotherKeyDuringTheHoldDetachesAndIsDropped(t *testing.T) {
	k := NewKeyReader(0x1d)
	k.Feed(0x1d)
	if a, fwd := k.Feed('d'); a != ActDetach || fwd != nil {
		t.Fatalf("leave then d gave %q %q, want a detach and nothing forwarded", a, fwd)
	}
	if k.Pending() {
		t.Fatal("the hold did not end")
	}
}

func TestAZeroLeaveKeyMeansTheDefault(t *testing.T) {
	k := NewKeyReader(0)
	if a, fwd := k.Feed(0x1d); a != ActNone || !bytes.Equal(fwd, []byte{0x1d}) {
		t.Fatalf("ctrl-] must forward under the default, got %q %q", a, fwd)
	}
	if a, fwd := k.Feed(0x00); a != ActNone || fwd != nil {
		t.Fatalf("the default leave key, ctrl-space, was forwarded: %q %q", a, fwd)
	}
	if a, _ := k.Flush(); a != ActDetach {
		t.Fatal("the default leave key does not leave")
	}
}

func TestAConfiguredLeaveKeyReplacesTheDefault(t *testing.T) {
	k := NewKeyReader(0x01)
	if a, fwd := k.Feed(0x1d); a != ActNone || !bytes.Equal(fwd, []byte{0x1d}) {
		t.Fatalf("ctrl-] must forward when ctrl-a is the leave key, got %q %q", a, fwd)
	}
	if a, fwd := k.Feed(0x01); a != ActNone || fwd != nil {
		t.Fatalf("ctrl-a was not held: %q %q", a, fwd)
	}
	if a, _ := k.Flush(); a != ActDetach {
		t.Fatal("ctrl-a does not leave")
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
		k := NewKeyReader(0x1d)
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
	k := NewKeyReader(0x1d)
	if _, out := feedAll(k, "\x1bq"); !bytes.Equal(out, []byte("\x1bq")) {
		t.Fatalf("ESC q forwarded %q", out)
	}
	k = NewKeyReader(0x1d)
	if _, out := feedAll(k, "\x1b[A"); !bytes.Equal(out, []byte("\x1b[A")) {
		t.Fatalf("ESC [ A forwarded %q", out)
	}
	k = NewKeyReader(0x1d)
	if _, out := feedAll(k, "\x1b[<0;3x"); !bytes.Equal(out, []byte("\x1b[<0;3x")) {
		t.Fatalf("a broken report forwarded %q", out)
	}
	k = NewKeyReader(0x1d)
	acts, out := feedAll(k, "\x1b\x1d")
	if !bytes.Equal(out, []byte("\x1b")) || len(acts) != 0 || !k.Pending() || k.Hold() != leaveHold {
		t.Fatalf("ESC then the leave key gave %v %q, want ESC forwarded and the leave key held", acts, out)
	}
}

func TestAHeldEscapeIsPendingUntilFlushed(t *testing.T) {
	k := NewKeyReader(0x1d)
	if act, out := k.Feed(0x1b); act != ActNone || out != nil {
		t.Fatalf("ESC gave %q %q", act, out)
	}
	if !k.Pending() || k.Hold() != escHold {
		t.Fatalf("ESC is not pending for %v: pending %v hold %v", escHold, k.Pending(), k.Hold())
	}
	if act, out := k.Flush(); act != ActNone || !bytes.Equal(out, []byte{0x1b}) {
		t.Fatalf("Flush = %q %q", act, out)
	}
	act, out := k.Flush()
	if k.Pending() || act != ActNone || out != nil {
		t.Fatal("Flush did not clear the held bytes")
	}
}

func TestAnOverlongReportIsForwardedNotHeld(t *testing.T) {
	k := NewKeyReader(0x1d)
	s := "\x1b[<" + string(bytes.Repeat([]byte("1"), 40))
	_, out := feedAll(k, s)
	if k.Pending() {
		t.Fatal("an overlong report is still held")
	}
	if !bytes.Equal(out, []byte(s)) {
		t.Fatalf("forwarded %q, want every byte", out)
	}
}

func TestTheHoldsAreShortAndTheLeaveHoldIsLonger(t *testing.T) {
	if escHold != 50*time.Millisecond || leaveHold != 300*time.Millisecond {
		t.Fatalf("escHold %v leaveHold %v", escHold, leaveHold)
	}
}

// feedEach feeds every byte of s and returns the last action, every byte
// forwarded, and every mouse report handed back.
func feedEach(k *KeyReader, s string) (Action, []byte, []byte) {
	var fwd, report []byte
	last := ActNone
	for i := 0; i < len(s); i++ {
		a, out := k.Feed(s[i])
		if a == ActMouse {
			report = append(report, out...)
			last = a
			continue
		}
		if a != ActNone {
			last = a
		}
		fwd = append(fwd, out...)
	}
	return last, fwd, report
}

func TestAReaderThatReportsMouseHandsBackTheWholeReport(t *testing.T) {
	k := NewKeyReader(0)
	k.ReportMouse = true
	a, fwd, report := feedEach(k, "a\x1b[<0;12;5Mb")
	if a != ActMouse || string(report) != "\x1b[<0;12;5M" {
		t.Fatalf("got %q report %q, want the whole report", a, report)
	}
	if string(fwd) != "ab" {
		t.Fatalf("forwarded %q, want the text around the report", fwd)
	}
}

func TestAReaderByDefaultStillDropsTheReport(t *testing.T) {
	k := NewKeyReader(0)
	a, fwd, report := feedEach(k, "\x1b[<0;12;5M")
	if a != ActNone || fwd != nil || report != nil {
		t.Fatalf("got %q %q %q, want the report dropped", a, fwd, report)
	}
}

// An ESC during the leave hold ends the hold as a leave, and the ESC is
// kept as the start of whatever follows, so a click right after the leave
// key is not cut in half.
func TestAnEscDuringTheHoldLeavesAndKeepsTheEsc(t *testing.T) {
	k := NewKeyReader(0)
	k.Feed(0x00)
	if a, fwd := k.Feed(0x1b); a != ActDetach || fwd != nil {
		t.Fatalf("got %q %q, want a detach", a, fwd)
	}
	if got := k.Held(); string(got) != "\x1b" {
		t.Fatalf("held %q, want the ESC", got)
	}
	if k.Pending() || k.Held() != nil {
		t.Fatal("Held did not empty the reader")
	}
}

func TestHeldHandsBackAPartReport(t *testing.T) {
	k := NewKeyReader(0)
	k.ReportMouse = true
	for _, b := range []byte("\x1b[<0;1") {
		k.Feed(b)
	}
	if got := k.Held(); string(got) != "\x1b[<0;1" {
		t.Fatalf("held %q", got)
	}
}

// forwarded feeds s one byte at a time, then flushes, and returns what was
// forwarded.
func forwarded(k *KeyReader, s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		_, fwd := k.Feed(s[i])
		out = append(out, fwd...)
	}
	_, fwd := k.Flush()
	return string(append(out, fwd...))
}

// The failure this names: a key report the terminal sends late, after the
// floor popped the kitty flags, reached an agent as a stray ESC and text.
// A reader with DropReports drops a whole CSI u report and a terminal's
// answer to a device query, and still forwards every other sequence.
func TestDropReportsDropsKeyReportsAndDeviceAnswersOnly(t *testing.T) {
	k := NewKeyReader(0)
	k.DropReports = true
	cases := map[string]string{
		"a\x1b[57442;5:3ub":    "ab",
		"\x1b[92;1:3u":         "",
		"\x1b[?62;22cx":        "x",
		"\x1b[?0u":             "",
		"\x1b[1;5A":            "\x1b[1;5A",
		"\x1b[A":               "\x1b[A",
		"\x1b[200~hi\x1b[201~": "\x1b[200~hi\x1b[201~",
		"\x1b":                 "\x1b",
		"\x1b[3~":              "\x1b[3~",
	}
	for in, want := range cases {
		if got := forwarded(k, in); got != want {
			t.Fatalf("%q forwarded %q, want %q", in, got, want)
		}
	}
	plain := NewKeyReader(0)
	if got := forwarded(plain, "\x1b[92;1:3u"); got != "\x1b[92;1:3u" {
		t.Fatalf("a reader without DropReports changed a report: %q", got)
	}
}
