package attach

import (
	"time"

	"github.com/opendaisugi/coppice/internal/config"
)

type Action string

const (
	ActNone   Action = ""
	ActDetach Action = "detach"
	// ActMouse hands back one whole mouse report. Only a reader with
	// ReportMouse set returns it.
	ActMouse Action = "mouse"
)

// leaveHold is how long a pressed leave key waits for a second press
// before it counts as a detach.
const leaveHold = 300 * time.Millisecond

// escHold is how long a held ESC waits for the rest of a mouse report
// before it is forwarded as a key of its own.
const escHold = 50 * time.Millisecond

// mouseReportMax caps the bytes a mouse report may hold. A longer run of
// bytes is not a report and is forwarded whole.
const mouseReportMax = 32

// KeyReader turns a byte stream into actions plus the bytes to forward to
// the pane. It steals one key, the leave key. Every other byte belongs to
// the harness and forwards as it is. The leave key is held: a second press
// within the hold forwards one leave byte to the pane, and a hold that
// expires is a detach, which Flush reports. The one other thing it drops
// is a complete mouse report, ESC [ < digits and semicolons then M or m. A
// terminal sends one only when a program asked for mouse reporting, and
// the pane never sees that request, so the bytes are never text the
// operator meant to type. An ESC is held until the next byte shows
// whether a report follows. Pending reports a held key or held bytes,
// Hold says how long to wait, and Flush resolves the hold. With
// ReportMouse set, a finished report comes back as ActMouse with its bytes
// instead of being dropped, so a caller that owns the screen can act on
// the click.
//
// With DropReports set, it also drops a whole CSI u key report, ESC [
// then digits, semicolons, colons or a question mark, then u, and a
// terminal's answer to a device query, ESC [ ? ... c. The floor sets it
// because its own talk key is the only thing that turns such reports on
// in the terminal, so a late one is never meant for the agent. coppice
// attach leaves it off, since there the agent may ask for them itself.
type KeyReader struct {
	ReportMouse bool
	DropReports bool
	leave       byte
	armed       bool
	held        []byte
}

// NewKeyReader makes a reader that steals leave. A zero leave means
// config.DefaultLeave.
func NewKeyReader(leave byte) *KeyReader {
	if leave == 0 {
		leave = config.DefaultLeave
	}
	return &KeyReader{leave: leave}
}

// Pending reports whether Feed holds the leave key or bytes that may
// start a mouse report.
func (k *KeyReader) Pending() bool { return k.armed || len(k.held) > 0 }

// Hold is how long the caller should wait before Flush: leaveHold for a
// held leave key, escHold for held ESC bytes, and zero with nothing held.
func (k *KeyReader) Hold() time.Duration {
	switch {
	case k.armed:
		return leaveHold
	case len(k.held) > 0:
		return escHold
	}
	return 0
}

// Held returns the bytes held for a possible mouse report and empties
// them. A caller that stops reading through k after a detach passes them
// on, so the rest of a sequence is never read as text.
func (k *KeyReader) Held() []byte {
	out := k.held
	k.held = nil
	return out
}

// Flush resolves the hold. A held leave key is a detach. Held ESC bytes
// come back for forwarding. Nothing is held after.
func (k *KeyReader) Flush() (Action, []byte) {
	if k.armed {
		k.armed = false
		return ActDetach, nil
	}
	out := k.held
	k.held = nil
	return ActNone, out
}

// feedHeld takes b into the held bytes. A finished report is dropped. A
// byte that breaks the report forwards the bytes typed before it and
// feeds b again on its own, so a leave key after an ESC is still held.
func (k *KeyReader) feedHeld(b byte) (Action, []byte) {
	k.held = append(k.held, b)
	n := len(k.held)
	csi := n >= 3 && k.held[2] != '<'
	switch {
	case n == 2 && b == '[', n == 3 && b == '<':
		return ActNone, nil
	case k.DropReports && csi && n <= mouseReportMax &&
		(b == ';' || b == ':' || (b >= '0' && b <= '9') || (n == 3 && b == '?')):
		return ActNone, nil
	case k.DropReports && csi && n > 3 && (b == 'u' || (b == 'c' && k.held[2] == '?')):
		k.held = nil
		return ActNone, nil
	case n > 3 && !csi && (b == 'M' || b == 'm'):
		report := k.held
		k.held = nil
		if k.ReportMouse {
			return ActMouse, report
		}
		return ActNone, nil
	case n > 3 && n <= mouseReportMax && (b == ';' || (b >= '0' && b <= '9')):
		return ActNone, nil
	}
	typed := k.held[:n-1]
	k.held = nil
	act, out := k.Feed(b)
	return act, append(append([]byte{}, typed...), out...)
}

// Feed takes one byte. The leave key is held. The leave key again while
// held forwards one leave byte. Any other byte while the leave key is
// held ends the hold as a detach. That byte is dropped, except an ESC,
// which stays held so Held can hand it to whatever reads the keys next. ESC is held for a mouse
// report. Every other byte forwards.
func (k *KeyReader) Feed(b byte) (Action, []byte) {
	if k.armed {
		k.armed = false
		if b == k.leave {
			return ActNone, []byte{k.leave}
		}
		if b == 0x1b {
			k.held = []byte{b}
		}
		return ActDetach, nil
	}
	if len(k.held) > 0 {
		return k.feedHeld(b)
	}
	switch b {
	case k.leave:
		k.armed = true
		return ActNone, nil
	case 0x1b:
		k.held = []byte{b}
		return ActNone, nil
	}
	return ActNone, []byte{b}
}
