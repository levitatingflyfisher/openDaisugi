package tui

import (
	"testing"
	"time"
)

func TestParseKittyCSIReadsKeysAndTheQueryAnswer(t *testing.T) {
	cases := map[string]kittyKey{
		"\x1b[92;5u":      {code: 92, mods: 5, event: kittyPress},
		"\x1b[92;5:1u":    {code: 92, mods: 5, event: kittyPress},
		"\x1b[92;5:2u":    {code: 92, mods: 5, event: kittyRepeat},
		"\x1b[92;1:3u":    {code: 92, mods: 1, event: kittyRelease},
		"\x1b[97:65;6u":   {code: 97, mods: 6, event: kittyPress},
		"\x1b[27u":        {code: 27, mods: 1, event: kittyPress},
		"\x1b[?0u":        {query: true},
		"\x1b[?15u":       {query: true},
		"\x1b[99;5:3;99u": {code: 99, mods: 5, event: kittyRelease},
	}
	for seq, want := range cases {
		got, ok := parseKittyCSI([]byte(seq))
		if !ok || got != want {
			t.Fatalf("%q = %+v %v, want %+v", seq, got, ok, want)
		}
	}
	for _, seq := range []string{"\x1b[A", "\x1b[?u", "\x1b[xu", "\x1b[92;xu", "x"} {
		if _, ok := parseKittyCSI([]byte(seq)); ok {
			t.Fatalf("%q parsed", seq)
		}
	}
}

func TestIsTalkCodeKnowsTheKeyBehindEachCtrlByte(t *testing.T) {
	if !isTalkCode(0x1c, '\\') || !isTalkCode(0x07, 'g') || !isTalkCode(0x00, ' ') || !isTalkCode(0x1e, '6') {
		t.Fatal("a talk key was not recognised")
	}
	if isTalkCode(0x1c, 'g') {
		t.Fatal("another key counted as the talk key")
	}
}

// at gives a moment ms milliseconds after a fixed start.
func at(ms int) time.Time { return time.Unix(1000, 0).Add(time.Duration(ms) * time.Millisecond) }

func TestTheFirstTalkPressStartsAndAsksTheTerminal(t *testing.T) {
	var s speaker
	act := s.feed(inTalkByte, at(0))
	if !act.start || act.write != kittyQuery || !s.recording || s.hold {
		t.Fatalf("act %+v state %+v", act, s)
	}
}

// The failure this names: holding the key on a terminal that reports
// release must record while held and stop on the release, and the flags
// the floor pushed must come off again.
func TestHoldToTalkWhereTheTerminalAnswers(t *testing.T) {
	var s speaker
	s.feed(inTalkByte, at(0))
	if act := s.feed(inKittyAnswer, at(5)); act.write != kittyPush || !s.hold {
		t.Fatalf("the answer did not push the flags: %+v", act)
	}
	// A plain repeat that was already on its way, then kitty repeats.
	for _, in := range []speakInput{inTalkByte, inKittyRepeat, inKittyRepeat} {
		if act := s.feed(in, at(600)); act.stop || act.start || act.write != "" {
			t.Fatalf("a repeat acted: %+v", act)
		}
	}
	act := s.feed(inKittyRelease, at(2000))
	if !act.stop || act.write != kittyPop || s.recording || s.hold {
		t.Fatalf("the release did not stop: %+v %+v", act, s)
	}
	// The next clip pushes at once and asks nothing.
	if act := s.feed(inTalkByte, at(5000)); !act.start || act.write != kittyPush {
		t.Fatalf("second clip: %+v", act)
	}
}

// The failure this names: a terminal answers a device query after its
// answer to CSI ? u, so a device answer with no kitty answer before it
// means the terminal does not know the protocol.
func TestADeviceAnswerWithNoKittyAnswerMeansNo(t *testing.T) {
	var s speaker
	s.feed(inTalkByte, at(0))
	s.feed(inDeviceAnswer, at(5))
	if s.kitty != kittyNo || s.hold {
		t.Fatalf("state %+v", s)
	}
	var k speaker
	k.feed(inTalkByte, at(0))
	k.feed(inKittyAnswer, at(4))
	k.feed(inDeviceAnswer, at(5))
	if k.kitty != kittyYes || !k.hold {
		t.Fatalf("a device answer after the kitty answer undid it: %+v", k)
	}
}

func TestAPressAfterALostReleaseStops(t *testing.T) {
	s := speaker{kitty: kittyYes}
	s.feed(inTalkByte, at(0))
	if act := s.feed(inKittyPress, at(3000)); !act.stop || act.write != kittyPop {
		t.Fatalf("%+v", act)
	}
}

// The failure this names: where the terminal never answers, a press starts
// and a second press stops, and a held key's repeats never stop the clip.
func TestPressToStartAndPressToStopWhereTheTerminalIsSilent(t *testing.T) {
	var s speaker
	s.feed(inTalkByte, at(0))
	// The key is held: the first repeat after half a second, then fast
	// repeats, then it is let go.
	for ms := 500; ms < 2000; ms += 30 {
		if act := s.feed(inTalkByte, at(ms)); act.stop {
			t.Fatalf("a repeat at %d ms stopped the clip", ms)
		}
	}
	act := s.feed(inTalkByte, at(4000))
	if !act.stop || act.write != "" || s.recording {
		t.Fatalf("the second press did not stop: %+v", act)
	}
	if s.kitty != kittyNo {
		t.Fatalf("a query with no answer by the end of a clip must count as no: %v", s.kitty)
	}
	if act := s.feed(inTalkByte, at(9000)); !act.start || act.write != "" {
		t.Fatalf("the next clip asked again or pushed: %+v", act)
	}
	if s.speakLine("ctrl-\\") != "recording… press ctrl-\\ to stop  Esc throws it away" {
		t.Fatalf("line %q", s.speakLine("ctrl-\\"))
	}
}

func TestEscThrowsTheClipAwayAndPops(t *testing.T) {
	s := speaker{kitty: kittyYes}
	s.feed(inTalkByte, at(0))
	if s.speakLine("ctrl-\\") != "recording… let go of ctrl-\\, or press it again, to stop  Esc throws it away" {
		t.Fatalf("line %q", s.speakLine("ctrl-\\"))
	}
	act := s.feed(inCancel, at(100))
	if !act.cancel || act.stop || act.write != kittyPop || s.recording {
		t.Fatalf("%+v", act)
	}
}

func TestCloseWhileHoldingPopsOnce(t *testing.T) {
	s := speaker{kitty: kittyYes}
	s.feed(inTalkByte, at(0))
	if s.close() != kittyPop || s.close() != "" {
		t.Fatal("close did not pop exactly once")
	}
	var idle speaker
	if idle.close() != "" {
		t.Fatal("an idle speaker popped")
	}
}

func TestKittyEventsWhileIdleDoNothing(t *testing.T) {
	var s speaker
	for _, in := range []speakInput{inKittyPress, inKittyRelease, inKittyRepeat, inCancel} {
		if act := s.feed(in, at(0)); act != (speakAct{}) {
			t.Fatalf("%v acted while idle: %+v", in, act)
		}
	}
}
