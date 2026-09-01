package tui

import (
	"strconv"
	"strings"
	"time"
)

// The talk key records a voice clip. A terminal that speaks the kitty
// keyboard protocol reports when a key is let go, so there the owner
// holds the key while speaking. Anywhere else one press starts the clip
// and a second press stops it.
//
// The protocol changes how every key is sent, and the windows pass typed
// bytes to agents as they are, so the floor never turns it on for the
// whole run. On the first talk press it asks the terminal, with CSI ? u,
// whether it knows the protocol. Only when the terminal answers does the
// floor push flags 1|2|8 (disambiguate, report press, repeat and
// release, and report every key as an escape code) with CSI > 11 u, and
// only while a clip records. Flag 8 is there because a key that makes
// text, such as \ once ctrl is let go first, may report no release
// without it. Every key goes to the talk key while a clip records, so
// flag 8 never reaches an agent. The floor pops the flags with CSI < u
// when the clip stops and when the floor closes.
const (
	kittyQuery = "\x1b[?u"
	kittyPush  = "\x1b[>11u"
	kittyPop   = "\x1b[<u"
)

// Kitty key event types.
const (
	kittyPress   = 1
	kittyRepeat  = 2
	kittyRelease = 3
)

// kittyKey is one CSI u report: a key with its modifiers and event type,
// or, when query is true, the terminal's answer to CSI ? u.
type kittyKey struct {
	code  int
	mods  int
	event int
	query bool
}

// parseKittyCSI reads a whole sequence that starts with ESC [ and ends
// with u. It reports false for anything else.
func parseKittyCSI(seq []byte) (kittyKey, bool) {
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' || seq[len(seq)-1] != 'u' {
		return kittyKey{}, false
	}
	body := string(seq[2 : len(seq)-1])
	if rest, ok := strings.CutPrefix(body, "?"); ok {
		if _, err := strconv.Atoi(rest); err != nil {
			return kittyKey{}, false
		}
		return kittyKey{query: true}, true
	}
	fields := strings.Split(body, ";")
	code, err := strconv.Atoi(strings.SplitN(fields[0], ":", 2)[0])
	if err != nil {
		return kittyKey{}, false
	}
	k := kittyKey{code: code, mods: 1, event: kittyPress}
	if len(fields) > 1 && fields[1] != "" {
		parts := strings.SplitN(fields[1], ":", 2)
		if parts[0] != "" {
			if k.mods, err = strconv.Atoi(parts[0]); err != nil {
				return kittyKey{}, false
			}
		}
		if len(parts) == 2 {
			if k.event, err = strconv.Atoi(parts[1]); err != nil {
				return kittyKey{}, false
			}
		}
	}
	return k, true
}

// kittyCodes are the key codes the kitty protocol reports for the ctrl
// byte b: the letter or symbol pressed with ctrl. ctrl-^ and ctrl-_ are
// shifted keys on many keyboards, so the unshifted key counts too.
func kittyCodes(b byte) []int {
	switch {
	case b == 0x00:
		return []int{' '}
	case b >= 0x01 && b <= 0x1a:
		return []int{int('a' + b - 1)}
	case b == 0x1c:
		return []int{'\\'}
	case b == 0x1d:
		return []int{']'}
	case b == 0x1e:
		return []int{'^', '6'}
	case b == 0x1f:
		return []int{'_', '-'}
	}
	return nil
}

// isTalkCode reports whether a kitty key code is the talk key b.
func isTalkCode(b byte, code int) bool {
	for _, c := range kittyCodes(b) {
		if c == code {
			return true
		}
	}
	return false
}

// kittySupport is what the floor knows about the terminal.
type kittySupport int

const (
	kittyUnknown kittySupport = iota
	kittyYes
	kittyNo
)

// speakInput is one thing the owner or the terminal did while the talk
// key matters.
type speakInput int

const (
	// inTalkByte is the talk key as a plain byte.
	inTalkByte speakInput = iota
	// inKittyPress, inKittyRepeat and inKittyRelease are the talk key as
	// a kitty report.
	inKittyPress
	inKittyRepeat
	inKittyRelease
	// inKittyAnswer is the terminal's answer to CSI ? u.
	inKittyAnswer
	// inCancel is Esc: the clip is thrown away.
	inCancel
	// inDeviceAnswer is the terminal's answer to the device query CSI c.
	// A terminal answers queries in order, so when it comes with no kitty
	// answer before it, the terminal does not know the protocol.
	inDeviceAnswer
)

// deviceQuery is CSI c, primary device attributes. Every terminal answers
// it. The floor sends it after the kitty query and after each pop, and
// keys go to the talk key until the answer comes: whatever the terminal
// sent before it saw the pop has arrived by then.
const deviceQuery = "\x1b[c"

// speakAct is what the floor does after one input.
type speakAct struct {
	start, stop, cancel bool
	// write is what the floor writes to the terminal: the query, a push
	// or a pop.
	write string
}

// speaker is the talk key's state. It is pure: the floor feeds it inputs
// and does what it returns.
type speaker struct {
	recording bool
	// started is when the clip began, and last when the last plain talk
	// byte came. Without the protocol a held key repeats, and the repeats
	// must not stop the clip.
	started, last time.Time
	// hold is true while the kitty flags are pushed, so a release stops
	// the clip.
	hold    bool
	kitty   kittySupport
	queried bool
}

// A held key starts to repeat after about half a second, then repeats
// every few tens of milliseconds. Without the protocol, a talk byte stops
// the clip only when it comes at least repeatGap after the one before it
// and at least minToggle after the clip began, so holding the key never
// stops the clip it started.
const (
	repeatGap = 150 * time.Millisecond
	minToggle = 700 * time.Millisecond
)

// feed takes one input at now and says what to do.
func (s *speaker) feed(in speakInput, now time.Time) speakAct {
	switch in {
	case inKittyAnswer:
		s.kitty = kittyYes
		if s.recording && !s.hold {
			s.hold = true
			return speakAct{write: kittyPush}
		}
		return speakAct{}
	case inTalkByte:
		last := s.last
		s.last = now
		if !s.recording {
			s.started = now
			return s.begin()
		}
		if s.hold {
			// A plain byte while the flags are pushed was sent before the
			// push took hold: a key repeat, never a second press.
			return speakAct{}
		}
		if now.Sub(last) < repeatGap || now.Sub(s.started) < minToggle {
			return speakAct{}
		}
		return s.end(false)
	case inKittyPress:
		if s.recording && s.hold {
			// The release got lost, so this press is the owner stopping.
			return s.end(false)
		}
		return speakAct{}
	case inKittyRelease:
		if s.recording && s.hold {
			return s.end(false)
		}
		return speakAct{}
	case inCancel:
		if s.recording {
			return s.end(true)
		}
	case inDeviceAnswer:
		if s.kitty == kittyUnknown && s.queried {
			s.kitty = kittyNo
		}
	}
	return speakAct{}
}

// begin starts a clip. The first clip asks the terminal about the
// protocol. Later clips push the flags when it answered.
func (s *speaker) begin() speakAct {
	s.recording = true
	act := speakAct{start: true}
	switch {
	case s.kitty == kittyYes:
		s.hold = true
		act.write = kittyPush
	case s.kitty == kittyUnknown && !s.queried:
		s.queried = true
		act.write = kittyQuery
	}
	return act
}

// end stops a clip, or throws it away when cancel is true. A query still
// unanswered by the end of a clip is taken as no.
func (s *speaker) end(cancel bool) speakAct {
	act := speakAct{stop: !cancel, cancel: cancel}
	if s.hold {
		act.write = kittyPop
	}
	s.recording, s.hold = false, false
	if s.kitty == kittyUnknown && s.queried {
		s.kitty = kittyNo
	}
	return act
}

// close is what the floor writes when it closes: the pop, when the flags
// are still pushed.
func (s *speaker) close() string {
	if s.hold {
		s.hold = false
		return kittyPop
	}
	return ""
}

// speakLine is the line the floor shows while a clip records.
func (s *speaker) speakLine(talk string) string {
	if s.hold {
		return "recording… let go of " + talk + ", or press it again, to stop  Esc throws it away"
	}
	return "recording… press " + talk + " to stop  Esc throws it away"
}
