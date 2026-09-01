// Package attach also holds Run, the loop that drives one attach session:
// dial the socket, send pane.attach, render every frame and state event that
// arrives, and turn keystrokes into either forwarded bytes or a pane.* call.
package attach

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/state"
	"golang.org/x/term"
)

// Options configures one attach session.
type Options struct {
	Socket string
	Pane   string
	// In is read for keystrokes when Keys is nil. Pass the real terminal
	// file here even when Keys is set: raw mode is set on In itself, so Keys
	// only changes where bytes come from, not which file goes into raw
	// mode.
	In  io.Reader
	Out io.Writer
	// Cols and Rows are the terminal's size. Run reserves the last row for
	// the status line: it attaches the pane with Rows-1 rows and draws the
	// status line on row Rows. A frame that resizes the pane moves the
	// status line with it.
	Cols int
	Rows int
	// Keys, when set, replaces In as the source of keystrokes. A caller
	// that runs Run more than once in the same process, such as a CLI
	// looping on Next to follow ctrl+a n across panes, must pass the same
	// channel every time: a fresh reader on In for every call would put two
	// readers on one stdin. Get one long-lived channel with ReadKeys and
	// pass it to every call.
	Keys <-chan byte
}

// Next is what the caller should do when Run returns. An empty Next means the
// user detached or closed the pane and the caller should exit.
type Next struct {
	Pane   string // re-attach to this pane
	Create bool   // create a pane in the same working directory, then attach
}

// ErrServerGone is what Run returns when the connection drops without a
// detach or close Run itself asked for, or the server never acknowledges the
// attach in time. The pane may still be running. Only the connection to it
// is gone.
var ErrServerGone = errors.New("the server went away")

// attachAckTimeout bounds how long Run waits for the server to acknowledge
// pane.attach. A server that accepts a connection and never answers must
// not hang the caller forever. A var, not a const, so a test can shrink it
// rather than wait out the real five seconds.
var attachAckTimeout = 5 * time.Second

// labelFetchTimeout bounds the one-time pane.list call Run makes to learn
// this pane's label. A slow or silent answer must not delay attaching.
const labelFetchTimeout = 500 * time.Millisecond

// escHold is how long a held ESC waits for the rest of a mouse report
// before it is forwarded as a key of its own.
const escHold = 50 * time.Millisecond

// teardownDeadline bounds neighbour's secondary connection and every send
// process makes on the main connection. A peer that accepts a connection
// and then goes silent must not be able to hold up ctrl+a n, ctrl+a p, a
// detach, or a close forever.
const teardownDeadline = 2 * time.Second

// ReadKeys starts the one long-lived reader a CLI should own for a whole
// session and returns the channel it feeds. It reads In one byte at a time,
// so the caller does not need to guess a read size. The channel closes once
// In returns an error, which is the signal that no more keys are coming.
func ReadKeys(in io.Reader) <-chan byte {
	ch := make(chan byte)
	go func() {
		defer close(ch)
		buf := make([]byte, 1)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				ch <- buf[0]
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// stateEvent is the wire shape of one state broadcast: PaneStateEvent's own
// fields, plus the event name, all at the top level.
type stateEvent struct {
	Event string `json:"event"`
	proto.PaneStateEvent
}

// utf8Buf assembles the bytes of one keystroke into a whole rune before it
// is sent. A terminal delivers a non-ASCII character as several bytes, one
// syscall read at a time, and sending each byte alone as its own text field
// mangles every one of them: encoding/json replaces a lone invalid byte with
// the Unicode replacement character. Holding the bytes until utf8.FullRune
// says the rune is complete is what keeps a keystroke whole.
type utf8Buf struct {
	pending []byte
}

// feed returns the bytes to send now, or nil when more bytes are needed. A
// byte below 0x80 always sends at once: ASCII is already one byte per rune,
// and it also clears anything left pending from an incomplete sequence,
// since an ASCII byte can never complete one. A byte at or above 0x80 joins
// the pending sequence and sends once utf8.FullRune says it is complete. A
// completed sequence that still decodes as an error is a byte that was never
// a valid keystroke, so it is dropped rather than sent.
func (u *utf8Buf) feed(b byte) []byte {
	if b < 0x80 {
		u.pending = nil
		return []byte{b}
	}
	if len(u.pending) >= 4 {
		u.pending = nil
	}
	u.pending = append(u.pending, b)
	if !utf8.FullRune(u.pending) {
		return nil
	}
	out := u.pending
	u.pending = nil
	if r, size := utf8.DecodeRune(out); r == utf8.RuneError && size == 1 {
		return nil
	}
	return out
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// fetchLabel asks the server for pane's label from a fresh connection. It
// gives up and returns "" on any error, on its own short deadline, or the
// moment done closes - a missing label is cosmetic and must never hold up
// or break an attach, and this call must never outlive the Run that started
// it either. Without the done watcher below, a Run that returned while this
// call was still blocked on the network left this connection open for up
// to its own full labelFetchTimeout after Run had already gone: the select
// that used to wrap this call in Run gave up waiting, but never actually
// stopped it.
func fetchLabel(socket, pane string, done <-chan struct{}) string {
	c2, err := net.Dial("unix", socket)
	if err != nil {
		return ""
	}
	defer c2.Close()
	_ = c2.SetDeadline(time.Now().Add(labelFetchTimeout))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-done:
			_ = c2.Close() // unblocks whatever this call is doing right now
		case <-stop:
		}
	}()

	e2, d2 := proto.NewEncoder(c2), proto.NewDecoder(c2)
	if err := e2.Send(map[string]any{"id": "lbl", "cmd": "pane.list"}); err != nil {
		return ""
	}
	line, err := d2.Next()
	if err != nil {
		return ""
	}
	var r struct {
		Result struct {
			Panes []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return ""
	}
	for _, p := range r.Result.Panes {
		if p.ID == pane {
			return p.Label
		}
	}
	return ""
}

// Run attaches to one pane and renders it until the user detaches, closes it,
// or asks for another pane. It restores the terminal on every exit path,
// including a panic, because leaving a shell in raw mode is a bug the user
// has to fix by hand.
func Run(o Options) (Next, error) {
	conn, err := net.Dial("unix", o.Socket)
	if err != nil {
		// Folded into ErrServerGone, not a plain error -
		// its own doc comment already covers "the server never acknowledges
		// the attach", which is the same class as never answering the dial
		// at all. A caller checks errors.Is(err, ErrServerGone) for exit 3
		// regardless of which of the two happened.
		return Next{}, fmt.Errorf("%w: cannot reach the server at %s. Run: coppice server start",
			ErrServerGone, o.Socket)
	}
	defer conn.Close()

	if f, ok := o.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		st, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return Next{}, err
		}
		restore := func() { _ = term.Restore(int(f.Fd()), st) }
		defer func() {
			if r := recover(); r != nil {
				restore()
				panic(r)
			}
			restore()
		}()
	}

	// done tells every goroutine Run starts to stop. It closes when Run
	// returns, so none of them can outlive the Run call that started them,
	// which matters most for the Keys channel: a caller shares one channel
	// across many Run calls, and a goroutine left running after Run returns
	// would steal the next call's keystrokes.
	//
	// stopped closes when the input goroutine itself has actually returned.
	// Run waits for it, right after that goroutine is started below, before
	// handing control back to its caller: closing done only asks the
	// goroutine to stop, and Go's select picks uniformly at random between
	// two ready cases, so a goroutine that is mid-select when done closes
	// could still receive one more byte from Keys instead of noticing done.
	// Waiting for stopped removes that window: by the time Run returns, the
	// goroutine is not selecting on Keys at all any more. This only covers
	// the Keys branch. The In branch wraps a real io.Reader.Read in its own
	// inner goroutine, and a generic Read cannot be interrupted, so that
	// inner goroutine is not joined and can outlive Run. The In branch is
	// for a single Run call and for tests, never for a channel shared
	// across many calls, which is why this gap does not reach the Keys case
	// real callers depend on.
	//
	// The wait for stopped is deferred where the goroutine is started, not
	// here where done and stopped are only created: an early return between
	// this line and that one (the initial pane.attach send failing, say)
	// would otherwise defer a wait on a channel nothing is ever going to
	// close, since the one goroutine that closes stopped would never have
	// run. Registering the combined close-then-wait as one deferred call,
	// and only once the goroutine that satisfies it actually exists, is
	// what keeps every return path safe.
	done := make(chan struct{})
	stopped := make(chan struct{})
	// helpReq carries a ctrl+a ? keypress from the input goroutine to the
	// main loop. screen is owned by the main loop, which alone calls Apply
	// and RenderTo on it; reading screen.rows or writing screen.resized
	// from the input goroutine would race both. One slot is enough: a
	// double press paints the same block once.
	helpReq := make(chan struct{}, 1)
	// noPaneReq carries a leader key that found no other open pane, from the
	// input goroutine to the main loop, for the same reason helpReq does.
	noPaneReq := make(chan struct{}, 1)

	paneRows := o.Rows - 1
	if paneRows < 0 {
		paneRows = 0
	}
	_ = conn.SetReadDeadline(time.Now().Add(attachAckTimeout))

	enc := proto.NewEncoder(conn)
	dec := proto.NewDecoder(conn)
	// The same teardownDeadline every later send on this connection carries
	// (sendMain, below): a peer that accepts but never reads must not be
	// able to block this first send any longer than any other.
	_ = conn.SetWriteDeadline(time.Now().Add(teardownDeadline))
	if err := enc.Send(map[string]any{
		"id": "attach", "cmd": "pane.attach", "pane": o.Pane,
		"cols": o.Cols, "rows": paneRows,
	}); err != nil {
		return Next{}, err
	}

	screen := NewScreen(o.Cols, paneRows)
	var lastState *proto.PaneStateEvent
	harness := ""

	var labelMu sync.Mutex
	label := ""
	getLabel := func() string { labelMu.Lock(); defer labelMu.Unlock(); return label }
	setLabel := func(l string) { labelMu.Lock(); label = l; labelMu.Unlock() }

	go func() {
		if l := fetchLabel(o.Socket, o.Pane, done); l != "" {
			setLabel(l)
		}
	}()

	// weClosed marks that this Run itself is why the connection is closing:
	// a detach, a close, or a leader key asking for another pane. Only then
	// does the decoder's resulting read error mean a clean end; any other
	// read error is the server going away.
	var weClosed atomic.Bool

	// nextMu guards next, which the input goroutine writes and the read
	// loop reads once the connection closes.
	var nextMu sync.Mutex
	var next Next
	setNext := func(n Next) {
		nextMu.Lock()
		next = n
		nextMu.Unlock()
	}

	// neighbour asks the server for the pane list and picks the one before
	// or after this pane, skipping closed ones. It uses a separate
	// connection: the attach connection is busy streaming frames.
	neighbour := func(delta int) string {
		c2, err := net.Dial("unix", o.Socket)
		if err != nil {
			return ""
		}
		defer c2.Close()
		_ = c2.SetDeadline(time.Now().Add(teardownDeadline))
		e2, d2 := proto.NewEncoder(c2), proto.NewDecoder(c2)
		if err := e2.Send(map[string]any{"id": "l", "cmd": "pane.list"}); err != nil {
			return ""
		}
		line, err := d2.Next()
		if err != nil {
			return ""
		}
		var r struct {
			Result struct {
				Panes []struct {
					ID     string `json:"id"`
					Closed bool   `json:"closed"`
				} `json:"panes"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return ""
		}
		var open []string
		for _, p := range r.Result.Panes {
			if !p.Closed {
				open = append(open, p.ID)
			}
		}
		if len(open) < 2 {
			return ""
		}
		for i, id := range open {
			if id == o.Pane {
				return open[((i+delta)%len(open)+len(open))%len(open)]
			}
		}
		return open[0]
	}

	// sendMain gives the main connection a fresh write deadline before every
	// send process and endStdinClosed make on it. A stuck peer can then
	// never hold up a detach, a close, or a leader key's own send for more
	// than teardownDeadline; the send's own error is discarded exactly as
	// it always was, since none of these callers wait for a reply anyway.
	sendMain := func(v any) {
		_ = conn.SetWriteDeadline(time.Now().Add(teardownDeadline))
		_ = enc.Send(v)
	}

	// endStdinClosed detaches on the pane's behalf when there is no more
	// keyboard to read from. Staying attached with a dead keyboard would
	// leave a live session rendering frames nobody can ever answer.
	endStdinClosed := func() {
		weClosed.Store(true)
		sendMain(map[string]any{"id": "eof", "cmd": "pane.detach", "pane": o.Pane})
		msg := "stdin closed. Detached."
		_, _ = io.WriteString(o.Out, fmt.Sprintf("\x1b[%d;1H\x1b[7m\x1b[K%s\x1b[0m", o.Rows, msg))
		_ = conn.Close()
	}

	ub := &utf8Buf{}

	// forward sends bytes to the pane as text, one whole rune at a time.
	forward := func(out []byte) {
		for _, ob := range out {
			if chunk := ub.feed(ob); chunk != nil {
				sendMain(map[string]any{
					"id": "t", "cmd": "pane.send_text", "pane": o.Pane,
					"text": string(chunk), "enter": false,
				})
			}
		}
	}

	// process turns one keystroke into a forwarded byte or a pane.* call. It
	// returns false once this attach session is over: detach, close, or a
	// leader key asking for another pane.
	process := func(k *KeyReader, b byte) bool {
		act, out := k.Feed(b)
		switch act {
		case ActDetach:
			weClosed.Store(true)
			sendMain(map[string]any{"id": "d", "cmd": "pane.detach", "pane": o.Pane})
			_ = conn.Close()
			return false
		case ActClose:
			weClosed.Store(true)
			sendMain(map[string]any{"id": "x", "cmd": "pane.close", "pane": o.Pane})
			_ = conn.Close()
			return false
		case ActHelp:
			select {
			case helpReq <- struct{}{}:
			default:
			}
		case ActNext, ActPrev:
			delta := 1
			if act == ActPrev {
				delta = -1
			}
			id := neighbour(delta)
			if id == "" {
				select {
				case noPaneReq <- struct{}{}:
				default:
				}
				return true
			}
			setNext(Next{Pane: id})
			weClosed.Store(true)
			sendMain(map[string]any{"id": "d", "cmd": "pane.detach", "pane": o.Pane})
			_ = conn.Close()
			return false
		case ActCreate:
			setNext(Next{Create: true})
			weClosed.Store(true)
			sendMain(map[string]any{"id": "d", "cmd": "pane.detach", "pane": o.Pane})
			_ = conn.Close()
			return false
		}
		forward(out)
		return true
	}

	// hold returns a timer that fires once the reader has held an ESC for
	// escHold, or nil when nothing is held.
	hold := func(k *KeyReader) <-chan time.Time {
		if k.Pending() {
			return time.After(escHold)
		}
		return nil
	}

	// Input goes straight through, except leader sequences and mouse
	// reports. Keys, when set, is the only source read: starting a second
	// reader on In as well would race it for the same bytes. Both paths
	// select on done, so neither keeps running once Run itself has
	// returned, and both forward a held ESC once escHold passes.
	go func() {
		defer close(stopped)
		k := NewKeyReader()
		if o.Keys != nil {
			for {
				select {
				case b, ok := <-o.Keys:
					if !ok {
						endStdinClosed()
						return
					}
					if !process(k, b) {
						return
					}
				case <-hold(k):
					forward(k.Flush())
				case <-done:
					return
				}
			}
		}
		type readRes struct {
			n   int
			err error
		}
		buf := make([]byte, 1)
		var rc chan readRes
		for {
			if rc == nil {
				rc = make(chan readRes, 1)
				go func(rc chan readRes) {
					n, err := o.In.Read(buf)
					rc <- readRes{n, err}
				}(rc)
			}
			select {
			case <-done:
				return
			case <-hold(k):
				forward(k.Flush())
			case r := <-rc:
				rc = nil
				if r.err != nil || r.n == 0 {
					endStdinClosed()
					return
				}
				if !process(k, buf[0]) {
					return
				}
			}
		}
	}()
	// The join above (done and stopped, explained where they are declared)
	// only holds if this defer is registered right here, immediately after
	// the goroutine it joins is started - never after any line that could
	// itself return early, which would defer a wait on a stopped nothing
	// is ever going to close.
	defer func() {
		close(done)
		<-stopped
	}()

	// lines wraps the blocking decoder read in a goroutine so the main loop
	// can also wait on a ticker. The send also selects on done: the main
	// loop can return without draining a line already read (a fatal reply
	// followed by a second line, say), and without done as an alternative
	// case a send into the one-slot buffer that never empties again blocks
	// this goroutine for the life of the process.
	type lineRes struct {
		line []byte
		err  error
	}
	lines := make(chan lineRes, 1)
	go func() {
		for {
			line, err := dec.Next()
			select {
			case lines <- lineRes{line, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	repaint := func() {
		eff := state.EffectiveState(lastState, nowSeconds())
		status := StatusLine(o.Pane, getLabel(), harness, eff, screen.cols)
		_, _ = io.WriteString(o.Out,
			fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[7m\x1b[K%s\x1b[0m\x1b8", screen.rows+1, status))
	}

	for {
		select {
		case <-helpReq:
			writeHelp(o.Out, screen)
		case <-noPaneReq:
			writeMessage(o.Out, screen, "There is no other open pane. Run: coppice pane create --cwd . -- claude")
		case <-ticker.C:
			// Only a blocked ask can go stale without a fresh event: its
			// deadline passes on the clock, not on a message arriving.
			// Anything else already repaints when the next frame or state
			// event lands.
			if lastState != nil && lastState.State == proto.StateBlocked && lastState.Ask != nil {
				repaint()
			}
		case r := <-lines:
			if r.err != nil {
				if weClosed.Load() {
					nextMu.Lock()
					n := next
					nextMu.Unlock()
					return n, nil
				}
				return Next{}, ErrServerGone
			}
			var probe struct {
				ID    string `json:"id"`
				Event string `json:"event"`
				OK    *bool  `json:"ok"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(r.line, &probe); err != nil {
				continue
			}
			if probe.OK != nil {
				if !*probe.OK && probe.Error != nil {
					if probe.ID == "attach" {
						return Next{}, fmt.Errorf("%s: %s", probe.Error.Code, probe.Error.Message)
					}
					// Only a failed attach ends the session. Any other
					// command's failure, a keystroke sent to a pane that
					// just closed, say, is shown and the session goes on.
					writeMessage(o.Out, screen, probe.Error.Code+": "+probe.Error.Message)
					continue
				}
				if *probe.OK && probe.ID == "attach" {
					_ = conn.SetReadDeadline(time.Time{})
				}
				continue
			}
			switch probe.Event {
			case "frame":
				var f proto.Frame
				if err := json.Unmarshal(r.line, &f); err != nil {
					continue
				}
				screen.Apply(f)
				if err := screen.RenderTo(o.Out); err != nil {
					return Next{}, err
				}
				eff := state.EffectiveState(lastState, nowSeconds())
				status := StatusLine(o.Pane, getLabel(), harness, eff, screen.cols)
				_, _ = io.WriteString(o.Out,
					fmt.Sprintf("\x1b[%d;1H\x1b[7m\x1b[K%s\x1b[0m", screen.rows+1, status))
				_, _ = io.WriteString(o.Out,
					fmt.Sprintf("\x1b[%d;%dH", screen.cursor[1]+1, screen.cursor[0]+1))
			case "state":
				var se stateEvent
				if err := json.Unmarshal(r.line, &se); err != nil {
					continue
				}
				ev := se.PaneStateEvent
				// The server broadcasts a state event for every pane it
				// knows, not only the attached one. Adopting one for
				// another pane would show a stranger's state on this
				// pane's status line.
				if ev.Pane == nil || *ev.Pane != o.Pane {
					continue
				}
				lastState = &ev
				if ev.Harness != "" {
					harness = ev.Harness
				}
				repaint()
			}
		}
	}
}

// writeLinesAboveStatus paints lines on the rows immediately above the
// status row, each line positioned by its own absolute cursor move, with no
// bare newline anywhere in the write. That keeps every line off the status
// row and stops the write from ever scrolling the terminal. Setting
// s.resized marks the screen for a full clear on the next frame, so the
// block disappears on the next repaint or keystroke instead of staying on
// screen until someone scrolls it away by hand.
func writeLinesAboveStatus(w io.Writer, s *Screen, lines []string) {
	if s.rows <= 0 || s.cols <= 0 {
		return
	}
	if len(lines) > s.rows {
		// A terminal shorter than the block shows only the tail of it,
		// never scrolls to fit the rest.
		lines = lines[len(lines)-s.rows:]
	}
	top := s.rows + 1 - len(lines)
	var b strings.Builder
	b.WriteString("\x1b7")
	for i, ln := range lines {
		if r := []rune(ln); len(r) > s.cols {
			// A terminal narrower than the longest line must not wrap it:
			// a wrap is a scroll by another name.
			ln = string(r[:s.cols])
		}
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[K%s", top+i, ln)
	}
	b.WriteString("\x1b8")
	_, _ = io.WriteString(w, b.String())
	s.resized = true
}

// writeHelp paints HelpText on the rows immediately above the status row.
func writeHelp(w io.Writer, s *Screen) {
	lines := strings.Split(strings.TrimSuffix(HelpText, "\r\n"), "\r\n")
	writeLinesAboveStatus(w, s, lines)
}

// writeMessage paints one line on the row immediately above the status row.
func writeMessage(w io.Writer, s *Screen, msg string) {
	writeLinesAboveStatus(w, s, []string{msg})
}
