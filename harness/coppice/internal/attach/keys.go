package attach

// Leader is ctrl+a, the same leader tmux and screen users already have in their
// fingers.
const Leader = 0x01

type Action string

const (
	ActNone    Action = ""
	ActNext    Action = "next"
	ActPrev    Action = "prev"
	ActCreate  Action = "create"
	ActDetach  Action = "detach"
	ActClose   Action = "close"
	ActHelp    Action = "help"
	ActLiteral Action = "literal"
)

// HelpText is what leader ? prints. Every line here does something: the three
// navigation keys return through attach.Next and the CLI re-attaches. STE100,
// one line per key.
const HelpText = "ctrl+a n  next pane\r\n" +
	"ctrl+a p  previous pane\r\n" +
	"ctrl+a c  create a pane here\r\n" +
	"ctrl+a d  detach and leave the pane running\r\n" +
	"ctrl+a x  close this pane\r\n" +
	"ctrl+a ?  this list\r\n" +
	"ctrl+a ctrl+a  send one ctrl+a to the pane\r\n"

// mouseReportMax caps the bytes a mouse report may hold. A longer run of
// bytes is not a report and is forwarded whole.
const mouseReportMax = 32

// KeyReader turns a byte stream into actions plus the bytes to forward to the
// pane. It never swallows typed input: an unknown key after the leader
// forwards both bytes, because losing a keystroke is the worst thing a
// terminal wrapper can do. The one thing it drops is a complete mouse
// report, ESC [ < digits and semicolons then M or m. A terminal sends one
// only when a program asked for mouse reporting, and the pane never sees
// that request, so the bytes are never text the operator meant to type.
// An ESC is held until the next byte shows whether a report follows.
// Pending reports held bytes and Flush hands them back, so a caller can
// forward a lone ESC after a short wait.
type KeyReader struct {
	armed bool
	held  []byte
}

func NewKeyReader() *KeyReader { return &KeyReader{} }

// Pending reports whether Feed holds bytes that may start a mouse report.
func (k *KeyReader) Pending() bool { return len(k.held) > 0 }

// Flush returns the held bytes for forwarding and holds nothing after.
func (k *KeyReader) Flush() []byte {
	out := k.held
	k.held = nil
	return out
}

// feedHeld takes b into the held bytes. A finished report is dropped. A
// byte that breaks the report forwards the bytes typed before it and
// feeds b again on its own, so a leader after an ESC still arms.
func (k *KeyReader) feedHeld(b byte) (Action, []byte) {
	k.held = append(k.held, b)
	n := len(k.held)
	switch {
	case n == 2 && b == '[', n == 3 && b == '<':
		return ActNone, nil
	case n > 3 && (b == 'M' || b == 'm'):
		k.held = nil
		return ActNone, nil
	case n > 3 && n <= mouseReportMax && (b == ';' || (b >= '0' && b <= '9')):
		return ActNone, nil
	}
	typed := k.held[:n-1]
	k.held = nil
	act, out := k.Feed(b)
	return act, append(append([]byte{}, typed...), out...)
}

func (k *KeyReader) Feed(b byte) (Action, []byte) {
	if k.Pending() {
		return k.feedHeld(b)
	}
	if !k.armed {
		if b == Leader {
			k.armed = true
			return ActNone, nil
		}
		if b == 0x1b {
			k.held = []byte{b}
			return ActNone, nil
		}
		return ActNone, []byte{b}
	}
	k.armed = false
	switch b {
	case 'n':
		return ActNext, nil
	case 'p':
		return ActPrev, nil
	case 'c':
		return ActCreate, nil
	case 'd':
		return ActDetach, nil
	case 'x':
		return ActClose, nil
	case '?':
		return ActHelp, nil
	case Leader:
		return ActLiteral, []byte{Leader}
	default:
		return ActNone, []byte{Leader, b}
	}
}
