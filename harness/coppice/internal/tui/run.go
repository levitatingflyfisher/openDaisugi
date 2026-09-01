package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/tiles"
	"golang.org/x/term"
)

// Options configures one run of the floor.
type Options struct {
	Socket string
	// In is read for keystrokes. When it is a terminal file, Run puts it
	// in raw mode for its lifetime.
	In  io.Reader
	Out io.Writer
	// Size returns the terminal's columns and rows. Run calls it before
	// every render, so a resize lands on the next frame.
	Size func() (int, int)
	// Cwd is where new panes open.
	Cwd string
	// Default is the harness Enter opens on an empty floor. Empty means
	// claude.
	Default string
	// DataDir is where floor.json keeps the layout and the lock between
	// runs. Empty means nothing is kept.
	DataDir string
	// Leave is the one key attach takes for itself inside a pane. Zero
	// means config.DefaultLeave.
	Leave byte
	// Resize, when set, gets a value each time the terminal size changes,
	// and the floor draws again at once and sends the tiles their new
	// sizes. When it is nil and In is a terminal, Run listens for SIGWINCH
	// itself.
	Resize <-chan os.Signal
	// Views are the view plugin ids. A view id typed alone at the prompt
	// names the floor page, since a terminal cannot draw a page.
	Views []string
	// WebURL is the floor page's address, or "" when the web server is
	// off.
	WebURL string
	// Talk is the key that records a voice clip for the selected agent.
	// Zero means config.DefaultTalk.
	Talk byte
}

const (
	// mouseOn asks the terminal for button presses in SGR 1006 form.
	// mouseOff turns that off again. Run writes them only on a terminal.
	mouseOn  = "\x1b[?1000h\x1b[?1006h"
	mouseOff = "\x1b[?1000l\x1b[?1006l"
	// mouseSeqMax caps the bytes a mouse report may take. Past it the
	// bytes are dropped.
	mouseSeqMax = 32
	// escWait is how long a bare ESC waits for the rest of a sequence.
	escWait = 50 * time.Millisecond
	// pasteWait is how long a \r or \n in a headless window's own input
	// line waits to see whether another byte follows it in the same
	// read: the reader goroutine sends one byte at a time, so a paste
	// already sitting in the pipe can still be a beat behind an instant
	// check. The same duration as escWait, for the same reason: long
	// enough to catch up, short enough that a deliberate Enter never
	// feels held up.
	pasteWait = escWait
	// pollEvery is how often the floor refetches pane.list.
	pollEvery = 2 * time.Second
	// callDeadline bounds every short-lived request the floor makes.
	callDeadline = 10 * time.Second
	// doubleClick is how close two left presses on one tile must come to
	// count as a double click.
	doubleClick = 400 * time.Millisecond
)

// reply is the wire shape of a reply or an event, as far as the floor
// reads it.
type reply struct {
	ID     string         `json:"id"`
	Event  string         `json:"event"`
	OK     *bool          `json:"ok"`
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call sends one request over a fresh connection and returns its result.
// A reply with ok false becomes an error carrying the server's message. A
// socket nothing answers on is an error wrapping attach.ErrServerGone.
func call(socket, cmd string, params map[string]any) (map[string]any, error) {
	return callWithin(socket, cmd, params, callDeadline)
}

// callWithin is call with its own deadline, for a request that waits on
// the server.
func callWithin(socket, cmd string, params map[string]any, deadline time.Duration) (map[string]any, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot reach the server at %s. Run: coppice server start",
			attach.ErrServerGone, socket)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(deadline))
	req := map[string]any{"id": "1", "cmd": cmd}
	for k, v := range params {
		req[k] = v
	}
	if err := proto.NewEncoder(conn).Send(req); err != nil {
		return nil, err
	}
	line, err := proto.NewDecoder(conn).Next()
	if err != nil {
		return nil, err
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, err
	}
	if r.OK == nil || !*r.OK {
		if r.Error != nil {
			return nil, errors.New(r.Error.Message)
		}
		return nil, fmt.Errorf("%s failed with no error message", cmd)
	}
	return r.Result, nil
}

// listPanes fetches pane.list as a list of rows.
func listPanes(socket string) ([]map[string]any, error) {
	res, err := call(socket, "pane.list", nil)
	if err != nil {
		return nil, err
	}
	raw, _ := res["panes"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows, nil
}

// listTasks fetches task.list as a list of rows.
func listTasks(socket string) ([]map[string]any, error) {
	res, err := call(socket, "task.list", nil)
	if err != nil {
		return nil, err
	}
	raw, _ := res["tasks"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows, nil
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// frameWriter writes a frame only when the lines or the cursor changed. A
// frame is a cursor home, every line cleared and written, then a cursor
// move to where the render put it. The screen is wiped first when the size
// changed or when a caller asked for a full clear.
type frameWriter struct {
	out          io.Writer
	last         []string
	cols         int
	lastX, lastY int
	lastHidden   bool
	clear        bool
}

// Cursor show and hide. The frame hides the cursor when the typing
// agent's cursor is outside its window, and shows it otherwise.
const (
	cursorShow = "\x1b[?25h"
	cursorHide = "\x1b[?25l"
)

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *frameWriter) write(lines []string, cols, x, y int, hidden bool) error {
	if !f.clear && f.cols == cols && sameLines(lines, f.last) && f.lastX == x && f.lastY == y &&
		f.lastHidden == hidden {
		return nil
	}
	var b strings.Builder
	if f.clear || f.cols != cols || len(lines) != len(f.last) {
		b.WriteString("\x1b[2J")
	}
	b.WriteString("\x1b[H")
	for i, l := range lines {
		b.WriteString("\x1b[K")
		b.WriteString(l)
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
	if hidden {
		b.WriteString(cursorHide)
	} else if f.lastHidden {
		b.WriteString(cursorShow)
	}
	f.last, f.cols, f.clear, f.lastX, f.lastY, f.lastHidden = lines, cols, false, x, y, hidden
	_, err := io.WriteString(f.out, b.String())
	return err
}

// key is one decoded keystroke. click is set for a keyMouse key, and
// kitty for a keyKitty key.
type key struct {
	kind  keyKind
	b     byte
	click Click
	kitty kittyKey
}

type keyKind int

const (
	keyByte keyKind = iota
	keyUp
	keyDown
	keyEsc
	keyShiftTab
	keyMouse
	// keyKitty is a CSI u report, which comes only while the talk key has
	// the kitty flags pushed, or as the terminal's answer to CSI ? u.
	keyKitty
	// keyDevice is the terminal's answer to the device query CSI c.
	keyDevice
	// keyLeft and keyRight move the cursor in a headless window's own
	// input line. Nothing else on the floor answers to them.
	keyLeft
	keyRight
)

// getByte returns the next key byte within wait. ok is false when the
// keys closed. late is true when no byte came in time.
type getByte func(wait time.Duration) (b byte, ok, late bool)

// fromChan reads key bytes from keys.
func fromChan(keys <-chan byte) getByte {
	return func(wait time.Duration) (byte, bool, bool) {
		select {
		case b, ok := <-keys:
			return b, ok, false
		case <-time.After(wait):
			return 0, true, true
		}
	}
}

// readKey turns the byte b and, for ESC, what follows it on keys into
// keystrokes. readKeyFrom says how.
func readKey(keys <-chan byte, b byte) (out []key, closed bool) {
	return readKeyFrom(fromChan(keys), b)
}

// readKeyFrom turns the byte b and, for ESC, what get returns after it
// into keystrokes. A bare ESC waits escWait for a next byte. A "[" or "O"
// starts a sequence that ends at the first byte in "@" to "~": "[A" is
// Up, "[B" is Down, "[C" is Right, "[D" is Left, "[Z" is shift-tab, "[<"
// opens a mouse report that ends at "M" or "m" and becomes a keyMouse
// key, and every other sequence is dropped. A report longer than
// mouseSeqMax bytes is dropped too. Any other byte means ESC was a key of
// its own, and that byte is returned after it. closed is true when the
// keys closed during the wait.
func readKeyFrom(get getByte, b byte) (out []key, closed bool) {
	if b != 0x1b {
		return []key{{kind: keyByte, b: b}}, false
	}
	c, ok, late := get(escWait)
	switch {
	case late:
		return []key{{kind: keyEsc}}, false
	case !ok:
		return []key{{kind: keyEsc}}, true
	case c != '[' && c != 'O':
		return []key{{kind: keyEsc}, {kind: keyByte, b: c}}, false
	}
	seq := []byte{0x1b, c}
	for {
		d, ok, late := get(escWait)
		if late {
			return nil, false
		}
		if !ok {
			return nil, true
		}
		seq = append(seq, d)
		if len(seq) == 3 && d == '<' {
			continue
		}
		if string(seq[:3]) == sgrMousePrefix {
			if len(seq) > mouseSeqMax {
				return nil, false
			}
			if d == 'M' || d == 'm' {
				if click, ok := ParseSGR(seq); ok {
					return []key{{kind: keyMouse, click: click}}, false
				}
				return nil, false
			}
			continue
		}
		if d >= '@' && d <= '~' {
			switch d {
			case 'A':
				return []key{{kind: keyUp}}, false
			case 'B':
				return []key{{kind: keyDown}}, false
			case 'C':
				return []key{{kind: keyRight}}, false
			case 'D':
				return []key{{kind: keyLeft}}, false
			case 'Z':
				return []key{{kind: keyShiftTab}}, false
			case 'u':
				if k, ok := parseKittyCSI(seq); ok {
					return []key{{kind: keyKitty, kitty: k}}, false
				}
			case 'c':
				if seq[2] == '?' {
					return []key{{kind: keyDevice}}, false
				}
			}
			return nil, false
		}
	}
}

// lineRes is one line from the event connection, or the error that ended
// it.
type lineRes struct {
	line []byte
	err  error
}

// floor is the state of one Run: the model, the frame writer, and the
// connections the loop drives. conn and enc are the event connection. The
// loop is the only goroutine that sends on it.
type floor struct {
	o     Options
	m     *Model
	fw    *frameWriter
	keys  <-chan byte
	lines <-chan lineRes
	conn  net.Conn
	enc   *proto.Encoder
	// detaching holds every pane whose pane.detach has been sent and not
	// yet answered. The server runs each request on its own goroutine, so
	// a new attach for such a pane waits for the reply, or it could land
	// before the detach and be undone by it.
	detaching map[string]bool
	// tty is true when In is a terminal, so mouse reporting was turned on
	// and must go off around a full-screen attach.
	tty bool
	// sizes holds the size each tile's pane was last attached or resized
	// to. A pane with no entry was attached with no size. sizing marks a
	// pane whose sized attach or resize waits for its reply. The server
	// runs each request on its own goroutine, so the floor sends the next
	// size only after that reply, and the last size sent is the one the
	// pane keeps.
	sizes  map[string]Size
	sizing map[string]bool
	// kr reads the bytes typed into the tile that owns the keyboard. It
	// holds the leave key and hands back mouse reports. holdC fires when
	// its hold runs out, and is nil while nothing is held.
	kr    *attach.KeyReader
	holdC <-chan time.Time
	// textQ is the typed text not yet sent, in order. textBusy is true
	// while one pane.send_text waits for its reply. The server runs each
	// request on its own goroutine, so the floor sends the next text only
	// after the reply, and the pane gets the bytes in the order typed.
	textQ    []typed
	textBusy bool
	// lastPress, lastSlot and lastPane are the last left press on a tile,
	// for the double click.
	lastPress time.Time
	lastSlot  int
	lastPane  string
	// promoted carries the message promote ends with. It has room for one,
	// so promote never waits on a floor that quit.
	promoted chan string
	// endedDue is true when the ended list must be read on the next
	// refresh, and endedRead is when it was last read. See endedWanted.
	endedDue  bool
	endedRead time.Time
	// replay holds bytes the typing reader held when the keyboard went
	// back to the roster, the start of a mouse report, say. They are read
	// before the next byte from keys, so the rest of the sequence is never
	// read as text.
	replay []byte
	// pauseUntil is when keys count again after the agent that had them
	// ended or left the screen. Until then every key is dropped, so type
	// ahead meant for that agent never becomes a rail command. pauseC
	// fires when the pause runs out, and is nil while none runs.
	pauseUntil time.Time
	pauseC     <-chan time.Time
	// dragFrom is the live row a left press landed on. A release over a
	// window puts that row there.
	dragFrom string
	// sp is the talk key's state, rec the clip that records now, and
	// recFor the pane its text goes to. voiceDone carries what a clip came
	// to once it was heard. awaitDevice is true from a device query until
	// its answer: keys go to the talk key until then, so an answer or a
	// late key report never reaches an agent as text. keysToTalk ends the
	// wait for a terminal that never answers.
	sp        speaker
	rec       *clip
	recFor    string
	recTo     *voiceTarget
	voiceDone chan voiceResult
	// checking is true while the check before a clip runs on the side,
	// and voiceChecked carries its answer.
	checking     bool
	voiceChecked chan voiceCheck
	keysToTalk   time.Time
	awaitDevice  bool
	// hearing counts clips sent and not yet heard back.
	hearing int
	// utf8Pending holds the bytes of a multi-byte rune typed into a
	// headless window's own input line, not yet whole. It is reset
	// whenever the keys change hands.
	utf8Pending []byte
}

// keyPause is how long keys are dropped after the agent that had them
// ended or left the screen.
const keyPause = 750 * time.Millisecond

// pauseKeys gives the keys to the rail and drops every key already typed
// and every key for keyPause, with text on the notice line. Keys typed
// for an agent that is gone must not act on its neighbour.
func (f *floor) pauseKeys(text string) {
	f.setTyping("")
	// Keys are dropped whole: each waiting byte is decoded first, so the
	// rest of an escape sequence, a held mouse report or an arrow that is
	// still arriving, goes with it and is never read as keys later.
	for {
		b, more, open := f.nextKey()
		if !more || !open {
			break
		}
		if _, closed := readKeyFrom(f.get, b); closed {
			break
		}
	}
	f.pauseUntil = time.Now().Add(keyPause)
	f.pauseC = time.After(keyPause)
	f.m.Paused = text
}

// paused reports whether keys are being dropped now.
func (f *floor) paused() bool { return time.Now().Before(f.pauseUntil) }

// keepTyping runs before every render. When the agent with the keys has
// ended, the keys pause and the rail cursor goes to its Recent entry.
// When its window is no longer shown on a screen that still has windows,
// it moves into a shown window and keeps the keys. With no windows left
// the keys pause.
func (f *floor) keepTyping(n int) {
	m := f.m
	id := m.Typing
	if id == "" || m.Tiles == nil {
		return
	}
	if !m.has(id) {
		name := m.nameOf(id)
		f.pauseKeys(name + " ended. Keys paused.")
		m.cursorToEnded(id)
		return
	}
	if slot, ok := m.Tiles.SlotOf(id); ok && slot < n {
		return
	}
	if n == 0 {
		f.pauseKeys(m.labelOf(id) + " left the screen. Keys paused.")
		return
	}
	target := min(max(m.Tiles.Focus, 0), n-1)
	if from, ok := m.Tiles.SlotOf(id); ok {
		m.Tiles.Slots[from] = m.Tiles.Slots[target]
	}
	m.Tiles.Slots[target] = id
	m.Tiles.Focus = target
}

// typed is text for one pane.
type typed struct {
	pane string
	b    []byte
}

// send writes one request on the event connection. A write that does not
// finish within callDeadline is an error.
func (f *floor) send(req map[string]any) error {
	_ = f.conn.SetWriteDeadline(time.Now().Add(callDeadline))
	defer func() { _ = f.conn.SetWriteDeadline(time.Time{}) }()
	return f.enc.Send(req)
}

// syncTiles attaches every shown pane the floor does not watch yet, with
// input rights and the tile's inner size, and detaches every watched pane
// no longer shown. A watched pane whose tile changed size gets
// pane.resize, once the reply to its last size came back. The tile that
// owns the keyboard gives it back to the roster when its pane is no longer
// shown. Replies come back on the event connection with ids va-<pane>,
// vr-<pane> and vd-<pane>. An attach or resize reply lets the next size go.
// A detach reply clears the pane from detaching, and a shown pane still in
// detaching is attached on the sync after that reply.
func (f *floor) syncTiles() error {
	cols, _ := f.o.Size()
	shown := map[string]bool{}
	for _, id := range f.m.Tiles.Shown(windowCount(cols)) {
		if id != "" {
			shown[id] = true
		}
	}
	if f.m.Typing != "" && !shown[f.m.Typing] {
		f.pauseKeys(f.m.nameOf(f.m.Typing) + " left the screen. Keys paused.")
	}
	for id := range shown {
		size, sized := f.m.TileSizes[id]
		if _, ok := f.m.Screens[id]; ok {
			if sized && f.sizes[id] != size && !f.sizing[id] {
				if err := f.send(map[string]any{
					"id": "vr-" + id, "cmd": "pane.resize", "pane": id, "cols": size.Cols, "rows": size.Rows,
				}); err != nil {
					return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
				}
				f.sizes[id] = size
				f.sizing[id] = true
			}
			continue
		}
		if f.detaching[id] {
			continue
		}
		req := map[string]any{"id": "va-" + id, "cmd": "pane.attach", "pane": id}
		if sized {
			req["cols"], req["rows"] = size.Cols, size.Rows
		}
		if err := f.send(req); err != nil {
			return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
		}
		f.m.Screens[id] = attach.NewScreen(0, 0)
		if sized {
			f.sizes[id] = size
			f.sizing[id] = true
		}
	}
	for id := range f.m.Screens {
		if shown[id] {
			continue
		}
		if err := f.detachTile(id); err != nil {
			return err
		}
	}
	return nil
}

// detachTile sends pane.detach for pane id, forgets its screen, and marks
// the pane as detaching until the reply arrives.
func (f *floor) detachTile(id string) error {
	delete(f.m.Screens, id)
	delete(f.sizes, id)
	delete(f.sizing, id)
	f.detaching[id] = true
	if err := f.send(map[string]any{"id": "vd-" + id, "cmd": "pane.detach", "pane": id}); err != nil {
		return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
	}
	return nil
}

// detachReply reports whether a line is the reply to a tile detach and,
// when it is, clears the pane from detaching.
func (f *floor) detachReply(ev map[string]any) bool {
	id := str(ev, "id")
	if !strings.HasPrefix(id, "vd-") {
		return false
	}
	delete(f.detaching, strings.TrimPrefix(id, "vd-"))
	return true
}

// replyError is the message of a reply with ok false, or "".
func replyError(ev map[string]any) string {
	if ok, isBool := ev["ok"].(bool); !isBool || ok {
		return ""
	}
	if e, _ := ev["error"].(map[string]any); e != nil {
		if msg := str(e, "message"); msg != "" {
			return msg
		}
	}
	return "the server refused with no message"
}

// inputReply reports whether a line is the reply to typed text, to a tile
// attach, or to a tile resize. A text reply lets the next text go. An
// attach or resize reply lets the next size go for a pane the floor still
// watches, and the render that follows sends it when the tile changed
// size meanwhile. A refusal of text or of a resize becomes the message
// line.
func (f *floor) inputReply(ev map[string]any) (bool, error) {
	id := str(ev, "id")
	if pane, ok := strings.CutPrefix(id, "va-"); ok {
		if _, watched := f.m.Screens[pane]; watched {
			delete(f.sizing, pane)
		}
		return true, nil
	}
	isText := strings.HasPrefix(id, "vt-")
	if !isText && !strings.HasPrefix(id, "vr-") {
		return false, nil
	}
	if msg := replyError(ev); msg != "" {
		f.m.Message = msg
	}
	if !isText {
		if _, watched := f.m.Screens[strings.TrimPrefix(id, "vr-")]; watched {
			delete(f.sizing, strings.TrimPrefix(id, "vr-"))
		}
		return true, nil
	}
	f.textBusy = false
	return true, f.pumpText()
}

// setTyping gives the keyboard to the tile of pane id, or to the roster
// when id is "". The reader starts fresh. Bytes the old reader held for a
// sequence move to replay, so the next reader sees the whole sequence. A
// headless agent gets no reader: its window edits its own input line
// instead of reading raw keystrokes.
func (f *floor) setTyping(id string) {
	f.holdC = nil
	if f.kr != nil {
		f.replay = append(f.replay, f.kr.Held()...)
	}
	f.kr = nil
	f.utf8Pending = nil
	f.m.Typing = id
	if id == "" || f.m.isHeadless(id) {
		return
	}
	f.kr = attach.NewKeyReader(f.o.Leave)
	f.kr.ReportMouse = true
	// Only the talk key turns key reports on, so any that reach a window
	// late are dropped, never typed.
	f.kr.DropReports = true
}

// armHold starts the hold timer when the reader holds a key, and stops it
// when the reader holds nothing. Only a typed byte arms it, so frames that
// arrive while a key is held never push the timer back.
func (f *floor) armHold() {
	if f.kr != nil && f.kr.Pending() {
		f.holdC = time.After(f.kr.Hold())
		return
	}
	f.holdC = nil
}

// typeKeys takes byte b for the tile that owns the keyboard, and every
// byte already waiting after it, then sends the text. It stops early when
// a byte gives the keyboard away. closed is true when the keys closed.
func (f *floor) typeKeys(b byte) (closed bool, err error) {
	for {
		if f.talkWants(b) {
			// The talk key never reaches the agent. It goes back to be
			// read again, now for the talk key.
			f.replay = append([]byte{b}, f.replay...)
			break
		}
		act, out := f.kr.Feed(b)
		if err := f.typedAction(act, out); err != nil {
			return false, err
		}
		if act != attach.ActNone || f.m.Typing == "" {
			break
		}
		next, more, open := f.nextKey()
		if !open {
			closed = true
			break
		}
		if !more {
			break
		}
		b = next
	}
	f.armHold()
	return closed, f.pumpText()
}

// headlessKeys takes byte b for the window that owns the keys, when its
// agent is headless, and every byte already waiting after it. It edits
// that window's own input line instead of forwarding raw bytes. A key
// that gives the keys away (Esc or the leave key) can arrive in the same
// batch as one that follows it (a bare Esc read together with the byte
// right after it, say), so every key after that one goes to the rail
// instead, exactly as it would have if the keys had already left before
// it arrived; a rail quit ends the floor. Once the keys move at all,
// whether to the rail or, on a click, to a different window, no further
// already-waiting byte is fed into this window's own edit: the next
// key() call reads f.m.Typing fresh and routes it correctly instead, the
// same as typeKeys stops on any action rather than guess where a
// following byte belongs. closed is true when the keys closed.
func (f *floor) headlessKeys(b byte) (closed bool, err error) {
	start := f.m.Typing
	for {
		if f.talkWants(b) {
			// The talk key never reaches the input line. It goes back to
			// be read again, now for the talk key.
			f.replay = append([]byte{b}, f.replay...)
			break
		}
		ks, cl := readKeyFrom(f.get, b)
		if cl {
			closed = true
		}
		for _, k := range ks {
			if f.m.Typing == "" {
				quit, err := f.handle(k)
				if err != nil {
					return false, err
				}
				if quit {
					return true, nil
				}
				continue
			}
			if f.m.Typing != start {
				break
			}
			if err := f.headlessKey(k); err != nil {
				return false, err
			}
		}
		if closed || f.m.Typing != start {
			break
		}
		next, more, open := f.nextKey()
		if !open {
			closed = true
			break
		}
		if !more {
			break
		}
		b = next
	}
	return closed, nil
}

// headlessKey applies one decoded key to the window's own input line. A
// click lands as it does anywhere else. The leave key gives the keys
// back to the rail, as it does for a pty window; Esc does too, but only
// here - a pty window has no line of its own for Esc to leave, so there
// it is just another byte forwarded to the pane. Enter sends the line as
// agent.prompt and clears it, unless another byte is already waiting
// behind it: that means a paste, not a real Enter, and it becomes a
// space instead, so pasted lines join with one rather than run together.
// Every other key edits the line: printable text and a whole multi-byte
// rune insert at the cursor, Backspace drops the rune before it, the
// arrows move it, and ctrl-u clears the line. Any key not named here
// does nothing.
func (f *floor) headlessKey(k key) error {
	m := f.m
	id := m.Typing
	switch k.kind {
	case keyMouse:
		return f.click(k.click)
	case keyEsc:
		f.setTyping("")
	case keyLeft:
		f.headlessMove(id, -1)
	case keyRight:
		f.headlessMove(id, 1)
	case keyByte:
		switch {
		case k.b == m.Leave:
			f.setTyping("")
		case k.b == 0x15: // ctrl-u
			m.setHeadlessBuf(id, "", 0)
		case k.b == 0x7f || k.b == 0x08:
			f.headlessBackspace(id)
		case k.b == '\r' || k.b == '\n':
			// A paste lands as many bytes already waiting one after
			// another, its own line breaks among them. Only a \r or \n
			// with nothing yet behind it is a real Enter; one with more
			// already queued is part of the paste, and becomes a space
			// so the lines it joins do not run together.
			if f.moreWaiting() {
				f.headlessInsert(id, " ")
				return nil
			}
			return f.sendHeadlessLine(id)
		case k.b >= 0x20 && k.b < 0x7f:
			f.headlessInsert(id, string(rune(k.b)))
		case k.b >= 0x80:
			f.feedHeadlessRune(id, k.b)
		}
	}
	return nil
}

// headlessInsert puts s at the cursor of pane id's own input line and
// moves the cursor past it.
func (f *floor) headlessInsert(id, s string) {
	line, cur := f.m.headlessBuf(id)
	f.m.setHeadlessBuf(id, line[:cur]+s+line[cur:], cur+len(s))
}

// feedHeadlessRune takes one byte of a multi-byte rune typed into a
// headless window's own input line. It inserts the rune once its bytes
// are whole, and drops a lead byte that can never start a valid one, the
// same rule wholeRunes applies to raw text for a pty pane.
func (f *floor) feedHeadlessRune(id string, b byte) {
	f.utf8Pending = append(f.utf8Pending, b)
	if !utf8.FullRune(f.utf8Pending) {
		return
	}
	r, size := utf8.DecodeRune(f.utf8Pending)
	if r == utf8.RuneError && size == 1 {
		f.utf8Pending = f.utf8Pending[1:]
		return
	}
	f.headlessInsert(id, string(f.utf8Pending[:size]))
	f.utf8Pending = f.utf8Pending[size:]
}

// headlessBackspace drops the rune before the cursor of pane id's own
// input line.
func (f *floor) headlessBackspace(id string) {
	line, cur := f.m.headlessBuf(id)
	if cur == 0 {
		return
	}
	_, size := utf8.DecodeLastRuneInString(line[:cur])
	f.m.setHeadlessBuf(id, line[:cur-size]+line[cur:], cur-size)
}

// headlessMove moves the cursor of pane id's own input line by one rune,
// left for dir -1 and right for dir 1. It does nothing at either end.
func (f *floor) headlessMove(id string, dir int) {
	line, cur := f.m.headlessBuf(id)
	switch {
	case dir < 0 && cur > 0:
		_, size := utf8.DecodeLastRuneInString(line[:cur])
		f.m.setHeadlessBuf(id, line, cur-size)
	case dir > 0 && cur < len(line):
		_, size := utf8.DecodeRuneInString(line[cur:])
		f.m.setHeadlessBuf(id, line, cur+size)
	}
}

// sendHeadlessLine sends pane id's own input line as agent.prompt and
// clears it once the server has taken it. An empty line does nothing: a
// headless agent takes whole messages, and an empty one says nothing to
// send. A refusal becomes the message line, and the line stays as typed,
// since the agent never got it: a turn already running is the common
// case, and the words typed for it are worth keeping to send again.
func (f *floor) sendHeadlessLine(id string) error {
	text, _ := f.m.headlessBuf(id)
	if text == "" {
		return nil
	}
	if _, err := call(f.o.Socket, "agent.prompt", map[string]any{"pane": id, "text": text}); err != nil {
		return f.fail(err)
	}
	f.m.setHeadlessBuf(id, "", 0)
	return nil
}

// get is the floor's getByte: replayed bytes first, then keys.
func (f *floor) get(wait time.Duration) (byte, bool, bool) {
	if len(f.replay) > 0 {
		b := f.replay[0]
		f.replay = f.replay[1:]
		return b, true, false
	}
	return fromChan(f.keys)(wait)
}

// nextKey returns a key byte that is already waiting, without blocking.
// Replayed bytes come first.
// more is false when none waits. open is false when the keys closed.
func (f *floor) nextKey() (b byte, more, open bool) {
	if len(f.replay) > 0 {
		b, f.replay = f.replay[0], f.replay[1:]
		return b, true, true
	}
	select {
	case b, ok := <-f.keys:
		return b, ok, ok
	default:
		return 0, false, true
	}
}

// moreWaiting reports whether another byte follows in the same read: it
// waits up to pasteWait, since the reader goroutine sends one byte at a
// time and a paste already sitting in the pipe can be a beat behind an
// instant check. Replay is read at once, with no wait, since that byte
// is already known to be there. A byte moreWaiting finds goes back onto
// replay, so whichever call reads next still sees it, in order: this is
// a peek, not a consume.
func (f *floor) moreWaiting() bool {
	b, ok, late := f.get(pasteWait)
	if late || !ok {
		return false
	}
	f.replay = append([]byte{b}, f.replay...)
	return true
}

// flushHold resolves the reader's hold once it ran out.
func (f *floor) flushHold() error {
	f.holdC = nil
	if f.kr == nil {
		return nil
	}
	if err := f.typedAction(f.kr.Flush()); err != nil {
		return err
	}
	return f.pumpText()
}

// typedAction applies what the reader decided. Text joins the queue for
// the typing pane. The leave key gives the keyboard to the roster. A
// mouse report is a click.
func (f *floor) typedAction(act attach.Action, out []byte) error {
	switch act {
	case attach.ActDetach:
		f.setTyping("")
	case attach.ActMouse:
		if c, ok := ParseSGR(out); ok {
			return f.click(c)
		}
	default:
		if f.m.Typing != "" && len(out) > 0 {
			f.queueText(f.m.Typing, out)
		}
	}
	return nil
}

// queueText adds typed bytes for pane to the queue.
func (f *floor) queueText(pane string, b []byte) {
	if n := len(f.textQ); n > 0 && f.textQ[n-1].pane == pane {
		f.textQ[n-1].b = append(f.textQ[n-1].b, b...)
		return
	}
	f.textQ = append(f.textQ, typed{pane: pane, b: append([]byte{}, b...)})
}

// pumpText sends the text at the head of the queue as one raw
// pane.send_text with no Enter added, when no text waits for a reply. A
// rune cut in half stays queued until its last byte comes.
func (f *floor) pumpText() error {
	for !f.textBusy && len(f.textQ) > 0 {
		head := f.textQ[0]
		text, rest := wholeRunes(head.b)
		kept := len(rest) > 0 && len(f.textQ) == 1
		if kept {
			f.textQ[0].b = rest
		} else {
			// A cut rune with text for another pane behind it never
			// completes, so it is dropped with its entry.
			f.textQ = f.textQ[1:]
		}
		if text == "" {
			if kept {
				return nil
			}
			continue
		}
		if err := f.send(map[string]any{
			"id": "vt-" + head.pane, "cmd": "pane.send_text", "pane": head.pane, "text": text, "enter": false,
		}); err != nil {
			return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
		}
		f.textBusy = true
	}
	return nil
}

// wholeRunes splits b into the text of its whole runes and the bytes of a
// rune not yet complete at its end. A byte that can never start a valid
// rune is dropped, since JSON would turn it into a replacement character.
func wholeRunes(b []byte) (string, []byte) {
	var out []byte
	for i := 0; i < len(b); {
		if !utf8.FullRune(b[i:]) {
			return string(out), append([]byte{}, b[i:]...)
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		out = append(out, b[i:i+size]...)
		i += size
	}
	return string(out), nil
}

// detachAll detaches every watched pane. A send that fails is ignored,
// since the connection is about to close or is already gone.
func (f *floor) detachAll() {
	for id := range f.m.Screens {
		_ = f.detachTile(id)
	}
}

// applyFrame folds one frame event into the screen of the pane it names.
// A frame for a pane the floor does not watch is dropped.
func (f *floor) applyFrame(line []byte, ev map[string]any) bool {
	sc := f.m.Screens[str(ev, "pane")]
	if sc == nil {
		return false
	}
	var fr proto.Frame
	if err := json.Unmarshal(line, &fr); err != nil {
		return false
	}
	sc.Apply(fr)
	return true
}

// Run opens the floor and drives it until the operator quits, In closes,
// or the server goes away. It restores the terminal on every exit path.
func Run(o Options) error {
	if o.Default == "" {
		o.Default = "claude"
	}
	if o.Size == nil {
		o.Size = func() (int, int) { return 120, 40 }
	}
	tty := false
	if f, ok := o.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		st, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return err
		}
		tty = true
		_, _ = io.WriteString(o.Out, mouseOn)
		restore := func() {
			_, _ = io.WriteString(o.Out, mouseOff+cursorShow)
			_ = term.Restore(int(f.Fd()), st)
		}
		defer func() {
			if r := recover(); r != nil {
				restore()
				panic(r)
			}
			restore()
		}()
	}

	conn, err := net.Dial("unix", o.Socket)
	if err != nil {
		return fmt.Errorf("%w: cannot reach the server at %s. Run: coppice server start",
			attach.ErrServerGone, o.Socket)
	}
	defer conn.Close()
	enc, dec := proto.NewEncoder(conn), proto.NewDecoder(conn)
	_ = conn.SetDeadline(time.Now().Add(callDeadline))
	if err := enc.Send(map[string]any{
		"id": "sub", "cmd": "events.subscribe", "kinds": []string{"state"}, "panes": "*",
	}); err != nil {
		return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
	}
	for {
		line, err := dec.Next()
		if err != nil {
			return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
		}
		var r reply
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.ID != "sub" {
			continue
		}
		if r.OK == nil || !*r.OK {
			msg := "no error message"
			if r.Error != nil {
				msg = r.Error.Message
			}
			return fmt.Errorf("events.subscribe: %s", msg)
		}
		break
	}
	// Notes and subagents come on a second subscribe. A server built
	// before them refuses it, and the floor goes on with state alone. The
	// reply is read and dropped by the loop below.
	if err := enc.Send(map[string]any{
		"id": "sub-more", "cmd": "events.subscribe", "kinds": []string{"note", "child"}, "panes": "*",
	}); err != nil {
		return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
	}
	_ = conn.SetDeadline(time.Time{})

	// done stops the decoder goroutine when Run returns, so a line it read
	// after the loop left never blocks it for the life of the process.
	done := make(chan struct{})
	defer close(done)
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

	resize := o.Resize
	if resize == nil && tty {
		c, stop := notifyResize()
		defer stop()
		resize = c
	}

	cols, _ := o.Size()
	layout, locked := readFloorState(o.DataDir)
	ts := tiles.New(layout, windowCount(cols))
	ts.Locked = locked
	f := &floor{
		o: o,
		m: &Model{
			Default: o.Default,
			Tiles:   ts,
			Screens: map[string]*attach.Screen{},
			Leave:   o.Leave,
			Talk:    o.Talk,
		},
		fw:        &frameWriter{out: o.Out},
		keys:      attach.ReadKeys(o.In),
		lines:     lines,
		conn:      conn,
		enc:       enc,
		detaching: map[string]bool{},
		tty:       tty,
		sizes:     map[string]Size{},
		sizing:    map[string]bool{},
		promoted:  make(chan string, 1),
		voiceDone: make(chan voiceResult, 4),
		// One check runs at a time, so one slot means it never waits on a
		// floor that quit.
		voiceChecked: make(chan voiceCheck, 1),
	}
	defer f.detachAll()
	// A clip still recording is thrown away and the kitty flags come off
	// before the terminal is given back.
	defer f.stopVoice()
	f.seedNotes()
	if err := f.refresh(); err != nil {
		return err
	}
	f.readFacts()
	f.startFill()
	if err := f.render(); err != nil {
		return err
	}

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		if len(f.replay) > 0 {
			b := f.replay[0]
			f.replay = f.replay[1:]
			end, err := f.key(b)
			if err != nil || end {
				return err
			}
			continue
		}
		select {
		case b, ok := <-f.keys:
			if !ok {
				return nil
			}
			end, err := f.key(b)
			if err != nil || end {
				return err
			}
		case <-resize:
			if err := f.render(); err != nil {
				return err
			}
		case <-f.pauseC:
			f.pauseC = nil
			f.m.Paused = ""
			if err := f.render(); err != nil {
				return err
			}
		case msg := <-f.promoted:
			f.m.Message = msg
			if err := f.render(); err != nil {
				return err
			}
		case c := <-f.voiceChecked:
			f.voiceCheckDone(c)
			if err := f.render(); err != nil {
				return err
			}
		case r := <-f.voiceDone:
			if err := f.voiceHeard(r); err != nil {
				return err
			}
			if err := f.render(); err != nil {
				return err
			}
		case <-f.holdC:
			if err := f.flushHold(); err != nil {
				return err
			}
			if err := f.render(); err != nil {
				return err
			}
		case r := <-f.lines:
			if r.err != nil {
				return fmt.Errorf("%w: the event connection closed", attach.ErrServerGone)
			}
			var ev map[string]any
			if err := json.Unmarshal(r.line, &ev); err != nil {
				continue
			}
			changed := f.detachReply(ev)
			input, err := f.inputReply(ev)
			if err != nil {
				return err
			}
			changed = changed || input
			switch ev["event"] {
			case "state":
				f.m.Event(ev)
				changed = true
				if ended(ev) {
					// The list tells an end on its own from an operator
					// close, and the refresh draws the ended line.
					if err := f.refresh(); err != nil {
						return err
					}
				}
			case "note":
				f.m.AddNote(str(ev, "text"))
				changed = true
			case "child":
				f.m.Child(ev)
				changed = true
			case "frame":
				changed = f.applyFrame(r.line, ev)
			}
			if changed {
				if err := f.render(); err != nil {
					return err
				}
			}
		case <-ticker.C:
			f.m.expireEnded(time.Now())
			if err := f.refresh(); err != nil {
				return err
			}
			f.readFacts()
			if err := f.render(); err != nil {
				return err
			}
		}
	}
}

// ended reports whether a state event says a pane's process or headless
// session is gone. A done from a hook or a manifest is only a turn that
// finished.
func ended(ev map[string]any) bool {
	if str(ev, "state") != proto.StateDone {
		return false
	}
	src := str(ev, "source")
	return src == proto.SrcProcess || src == proto.SrcHeadless
}

// startFill fills the windows once, when the floor opens: agents that
// need you first, then working agents, then the rest, in rail order. The
// keys go to the first agent that needs you, or else the first window, or
// else they stay on the rail. They never start on the prompt line.
func (f *floor) startFill() {
	m := f.m
	cols, _ := f.o.Size()
	n := windowCount(cols)
	if n == 0 || m.Tiles == nil {
		return
	}
	f.fillEmpty()
	target := ""
	for _, r := range m.treeRows() {
		if r.needsYou() {
			if slot, ok := m.Tiles.SlotOf(r.ID); ok && slot < n {
				target = r.ID
			}
			break
		}
	}
	if target == "" {
		if shown := m.Tiles.Shown(n); len(shown) > 0 {
			target = shown[0]
		}
	}
	if target == "" {
		return
	}
	m.Tiles.Fill(target)
	m.Cursor = m.indexOf(target, m.Cursor)
	f.setTyping(target)
}

// fillEmpty puts the agents that are in no window into the empty windows
// the screen shows, in start order, so a window an agent left does not
// stay empty while another agent waits. The keys do not move.
func (f *floor) fillEmpty() {
	m := f.m
	cols, _ := f.o.Size()
	n := windowCount(cols)
	if n == 0 || m.Tiles == nil {
		return
	}
	f.growSlots(n)
	for _, id := range m.startOrder() {
		if _, ok := m.Tiles.SlotOf(id); ok {
			continue
		}
		if !f.putInEmpty(id, n) {
			return
		}
	}
}

// putInEmpty puts pane id in the first empty slot among the first n, and
// reports whether there was one. Under the all layout a slot is added
// while there are fewer than n.
func (f *floor) putInEmpty(id string, n int) bool {
	ts := f.m.Tiles
	for i := 0; i < n && i < len(ts.Slots); i++ {
		if ts.Slots[i] == "" {
			ts.Slots[i] = id
			return true
		}
	}
	if ts.Layout == tiles.All && len(ts.Slots) < n {
		ts.Slots = append(ts.Slots, id)
		return true
	}
	return false
}

// growSlots gives a focus layout at least n slots, so a wider screen
// shows more windows. Slots past what the screen shows are kept.
func (f *floor) growSlots(n int) {
	ts := f.m.Tiles
	if ts == nil || ts.Layout != tiles.Focus {
		return
	}
	for len(ts.Slots) < n {
		ts.Slots = append(ts.Slots, "")
	}
}

// key takes one key byte: into the typing tile when one owns the
// keyboard, else through readKeyFrom to the roster. end is true when Run
// should return with no error: the keys closed or the operator quit. Any
// key clears an ended line that is not sticky.
func (f *floor) key(b byte) (end bool, err error) {
	if f.sp.recording || f.fenced() {
		return f.speakKey(b)
	}
	if !f.paused() && b == f.m.talkKey() {
		return f.speakKey(b)
	}
	if f.hearing == 0 && !f.checking {
		// The voice line has been read once the owner presses another key.
		f.m.Voice = ""
	}
	if f.paused() {
		// The whole key is decoded and dropped, so a sequence that began
		// in the pause never ends as keys after it.
		_, closed := readKeyFrom(f.get, b)
		return closed, nil
	}
	f.m.keyPressed()
	if f.m.Typing != "" {
		var closed bool
		var err error
		if f.m.isHeadless(f.m.Typing) {
			closed, err = f.headlessKeys(b)
		} else {
			closed, err = f.typeKeys(b)
		}
		if err != nil {
			return false, err
		}
		if err := f.render(); err != nil {
			return false, err
		}
		return closed, nil
	}
	ks, closed := readKeyFrom(f.get, b)
	for _, k := range ks {
		quit, err := f.handle(k)
		if err != nil {
			return false, err
		}
		if quit {
			return true, nil
		}
		if err := f.render(); err != nil {
			return false, err
		}
	}
	return closed, nil
}

// fail sorts an error from a call. A server that is gone ends the floor,
// so the error comes back to Run. Anything else becomes the message line
// and the floor goes on.
func (f *floor) fail(err error) error {
	if errors.Is(err, attach.ErrServerGone) {
		return err
	}
	f.m.Message = err.Error()
	return nil
}

// refresh replaces the rows with a fresh pane.list, then the tasks with a
// fresh task.list. A failed pane.list becomes the message line and the
// rows stay as they were. A failed task.list becomes the message line
// too, but the roster still shows: the tasks are emptied and the rows are
// applied, so a server that lacks the verb still gives the floor its
// panes.
func (f *floor) refresh() error {
	list, err := listPanes(f.o.Socket)
	if err != nil {
		return f.fail(err)
	}
	was := map[string]bool{}
	for _, r := range f.m.Rows {
		if r.Parent == "" {
			was[r.ID] = true
		}
	}
	tasks, err := listTasks(f.o.Socket)
	if err != nil {
		f.m.Tasks = nil
		f.m.Apply(list, nowSeconds())
		return f.fail(err)
	}
	keep := f.m.selectedKey()
	f.m.Tasks = DecodeTasks(tasks)
	f.m.Apply(list, nowSeconds())
	if f.endedWanted(was, time.Now()) {
		if err := f.refreshEnded(was); err != nil {
			return err
		}
	}
	if was[keep] && !f.m.has(keep) {
		// The selected row ended: the cursor goes to its Recent entry,
		// not to a live neighbour a stray key would then act on.
		f.m.cursorToEnded(keep)
	}
	f.fillEmpty()
	return nil
}

// endedEvery is the slow beat the ended list is read on when nothing
// else asks for it.
const endedEvery = 10 * time.Second

// endedWanted reports whether a refresh reads the ended list: when an
// agent left the live list since the last one, when an action marked it
// due, while Recent is open, or once it is endedEvery old. A quiet floor
// then asks for it once in ten seconds, not on every poll.
func (f *floor) endedWanted(was map[string]bool, now time.Time) bool {
	want := f.endedDue || f.m.RecentOpen || now.Sub(f.endedRead) >= endedEvery
	for id := range was {
		if !f.m.has(id) {
			want = true
		}
	}
	if want {
		f.endedDue, f.endedRead = false, now
	}
	return want
}

// refreshEnded fetches the ended agents for the Recent fold. An agent in
// was that left the live list and shows up there ended on its own, so
// the floor shows its ended line. One that left and is not there was
// stopped by an operator, and says nothing. A server that does not know
// the verb leaves the fold as it was. Any other error is the message
// line, and a server that is gone ends the floor.
func (f *floor) refreshEnded(was map[string]bool) error {
	res, err := call(f.o.Socket, "pane.list", map[string]any{"ended": true})
	if err != nil {
		if strings.HasPrefix(err.Error(), "no command ") {
			return nil
		}
		return f.fail(err)
	}
	raw, _ := res["panes"].([]any)
	list := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if p, ok := r.(map[string]any); ok {
			list = append(list, p)
		}
	}
	f.m.ApplyEnded(list, nowSeconds())
	for _, r := range f.m.Ended {
		if was[r.ID] && !f.m.has(r.ID) {
			f.m.noteEnded(r)
		}
	}
	return nil
}

// render draws the floor, then brings the tile attaches, each with input
// rights and its tile's size, in line with the tiles the frame shows. A
// wider screen gets more slots first.
func (f *floor) render() error {
	cols, rows := f.o.Size()
	n := windowCount(cols)
	f.growSlots(n)
	f.keepTyping(n)
	lines := Render(f.m, cols, rows)
	if err := f.fw.write(lines, cols, f.m.CursorX, f.m.CursorY, f.m.CursorHidden); err != nil {
		return err
	}
	return f.syncTiles()
}

// answerPeek is y, t or n on a peek that shows an ask. n denies on either
// tier. On an undoable ask, y allows once and t allows for the task, which
// the gate records as a proposed envelope edit. On a permanent ask, y and t
// only say to type the pane name.
func (f *floor) answerPeek(b byte) error {
	p := f.m.Peek
	switch {
	case b == 'n':
		return f.sendAnswer("agent.deny", nil, "Denied.")
	case p.Permanent():
		f.m.Message = "type the pane name to allow"
		return nil
	case b == 'y':
		return f.sendAnswer("agent.allow", nil, "Allowed once.")
	default:
		return f.sendAnswer("agent.allow", map[string]any{"scope": "task"},
			"Allowed for this task. The gate records it as a proposal.")
	}
}

// answerTrust answers Claude's folder trust screen in the peek: yes
// trusts the folder, and no presses Esc, which ends Claude. On success
// the peek closes. A refusal stays on the message line.
func (f *floor) answerTrust(yes bool) error {
	p := f.m.Peek
	if _, err := call(f.o.Socket, "pane.trust", map[string]any{"pane": p.Pane, "trust": yes}); err != nil {
		return f.fail(err)
	}
	f.m.ClosePeek()
	if yes {
		f.m.Message = "Trusted the folder. Claude starts."
	} else {
		f.m.Message = "Not now. Claude ends."
	}
	return nil
}

// sendAnswer sends verb for the peek's ask with the extra params. On
// success the peek closes and the message line says done. A refusal stays
// on the message line and the peek stays open.
func (f *floor) sendAnswer(verb string, extra map[string]any, done string) error {
	p := f.m.Peek
	params := map[string]any{"pane": p.Pane, "ask": p.AskID}
	for k, v := range extra {
		params[k] = v
	}
	if _, err := call(f.o.Socket, verb, params); err != nil {
		return f.fail(err)
	}
	f.m.ClosePeek()
	f.m.Message = done
	return nil
}

// treeKey applies one keystroke while the tree is open. Up and Down move
// among the task lines. Enter pushes the roster into the task under the
// cursor and closes the tree. Esc closes it. Every other key does
// nothing.
func (f *floor) treeKey(k key) {
	m := f.m
	switch k.kind {
	case keyUp:
		m.Tree.Move(-1)
	case keyDown:
		m.Tree.Move(1)
	case keyEsc:
		m.Tree = nil
	case keyByte:
		if k.b == '\r' || k.b == '\n' {
			if id := m.Tree.Selected(); id != "" {
				m.Push(id)
			}
			m.Tree = nil
		}
	}
}

// up is Esc: it goes up one level. A tree closes. A peek closes. A prompt
// that has the keyboard lets it go and empties. A pushed roster pops one
// task. On the bare top floor nothing changes, and the message line stays.
func (f *floor) up() {
	m := f.m
	switch {
	case m.Tree != nil:
		m.Tree = nil
	case m.Peek != nil:
		m.ClosePeek()
	case m.Talking || m.Prompt != "":
		m.Talking = false
		m.Prompt = ""
	case len(m.Path) > 0:
		m.Pop()
	}
}

// typeInTile fills pane id into the tiles and gives that tile the
// keyboard. It is false when the screen shows no tiles, and then nothing
// changes. It is also false when the pane lands in a slot the screen does
// not show. The pane keeps that slot, and the keyboard stays with the
// roster.
func (f *floor) typeInTile(id string) bool {
	m := f.m
	cols, _ := f.o.Size()
	n := windowCount(cols)
	if n == 0 || m.Tiles == nil {
		return false
	}
	m.Tiles.Fill(id)
	for _, shown := range m.Tiles.Shown(n) {
		if shown == id {
			f.setTyping(id)
			return true
		}
	}
	return false
}

// space is the Space key on an empty prompt. When the screen shows tiles
// it fills the row's pane into them. Otherwise it opens the peek.
func (f *floor) space() error {
	if it, ok := f.m.SelectedItem(); ok && it.kind == itemGroup {
		f.m.ToggleGroup(it.group.Key)
		return nil
	}
	if r, ok := f.m.Selected(); ok && r.Parent != "" {
		f.m.Message = ChildMessage
		return nil
	}
	cols, _ := f.o.Size()
	if windowCount(cols) == 0 || f.m.Tiles == nil {
		return f.peek()
	}
	if r, ok := f.m.Selected(); ok {
		f.m.Tiles.Fill(r.ID)
	}
	return nil
}

// createPane opens a pty pane running argv in Cwd and returns its id.
func (f *floor) createPane(argv []string) (string, error) {
	return f.create(map[string]any{"cmd_argv": argv})
}

// createHarnessPane opens a pty pane for the harness named name in Cwd
// and returns its id. It sends no command: the server reads the command
// from the config file, so an edit there takes effect on the next pane.
// args, when given, follow the command from the file.
func (f *floor) createHarnessPane(name string, args ...string) (string, error) {
	params := map[string]any{"harness": name}
	if len(args) > 0 {
		params["args"] = args
	}
	return f.create(params)
}

// newSize is the size a new agent starts at: the inner size of the window
// it will fill, the first empty one or else the focused one, when the
// last render showed windows. Otherwise it is the screen less attach's
// status row, since the agent will be attached full screen.
func (f *floor) newSize() (cols, rows int) {
	m := f.m
	if m.Tiles != nil && len(m.TileW) > 0 && m.WinRows > 0 {
		slot := -1
		for i := 0; i < len(m.TileW) && i < len(m.Tiles.Slots); i++ {
			if m.Tiles.Slots[i] == "" {
				slot = i
				break
			}
		}
		if slot < 0 {
			slot = min(max(m.Tiles.Focus, 0), len(m.TileW)-1)
		}
		return m.TileW[slot], m.WinRows
	}
	cols, rows = f.o.Size()
	if rows > 1 {
		rows--
	}
	return cols, rows
}

// create sends pane.create for a pty pane in Cwd, sized to the screen
// less attach's status row, with the extra params added, and returns
// the new pane id.
func (f *floor) create(extra map[string]any) (string, error) {
	cols, rows := f.newSize()
	params := map[string]any{"cwd": f.o.Cwd, "kind": "pty", "cols": cols, "rows": rows}
	for k, v := range extra {
		params[k] = v
	}
	res, err := call(f.o.Socket, "pane.create", params)
	if err != nil {
		return "", err
	}
	id, _ := res["pane"].(string)
	if id == "" {
		return "", errors.New("the server created no pane. Run: coppice pane list")
	}
	return id, nil
}

// peek opens the peek on the row under the cursor: the pane's visible text
// and the ask it holds, read fresh from the server.
func (f *floor) peek() error {
	m := f.m
	r, ok := m.Selected()
	if !ok {
		return nil
	}
	if r.Parent != "" {
		m.Message = ChildMessage
		return nil
	}
	res, err := call(f.o.Socket, "pane.read", map[string]any{"pane": r.ID, "source": "visible"})
	if err != nil {
		return f.fail(err)
	}
	text, _ := res["text"].(string)
	list, err := listPanes(f.o.Socket)
	if err != nil {
		return f.fail(err)
	}
	var ask map[string]any
	for _, p := range list {
		if str(p, "id") == r.ID {
			ask = askOf(p)
		}
	}
	m.Apply(list, nowSeconds())
	if cur, ok := m.Selected(); !ok || cur.ID != r.ID {
		m.Message = "pane " + r.ID + " is gone"
		return nil
	}
	m.OpenPeek(text, ask)
	return nil
}

// seedNotes fills the notes from floor.notes, so the floor opens with the
// last lines the foreman ran. A server that has no floor.notes leaves them
// empty.
func (f *floor) seedNotes() {
	res, err := call(f.o.Socket, "floor.notes", nil)
	if err != nil {
		return
	}
	raw, _ := res["notes"].([]any)
	for _, n := range raw {
		if m, ok := n.(map[string]any); ok {
			f.m.AddNote(str(m, "text"))
		}
	}
}

// talk sends one sentence to the floor's foreman through floor.talk. The
// server starts a foreman when none runs and keeps the words in order, so
// this floor, the web page and a voice tool never start two. When tiles
// show, the foreman's pane fills a tile, so its reply shows there.
func (f *floor) talk(line string) error {
	m := f.m
	res, err := call(f.o.Socket, "floor.talk", map[string]any{"text": line})
	if err != nil {
		return f.fail(err)
	}
	id := str(res, "pane")
	m.Message = talkMessage(res)
	if err := f.refresh(); err != nil {
		return err
	}
	cols, _ := f.o.Size()
	if id != "" && windowCount(cols) > 0 && m.Tiles != nil {
		if _, shown := m.Tiles.SlotOf(id); !shown {
			m.Tiles.Fill(id)
		}
	}
	return nil
}

// talkMessage is the one line the floor shows for a floor.talk reply: the
// server's note when it has one, else where the words went.
func talkMessage(res map[string]any) string {
	label := str(res, "label")
	if label == "" {
		label = "the foreman"
	}
	if note := str(res, "note"); note != "" {
		return note
	}
	if b, _ := res["started"].(bool); b {
		return label + " starts and gets its page. Your words go once it is ready."
	}
	if n, _ := res["queued"].(float64); n > 1 {
		return fmt.Sprintf("Sent to %s. %d sentences wait for it, yours last.", label, int(n))
	}
	return "Sent to " + label + "."
}

// runPrompt runs one prompt line. Plumbing runs here. Talk goes to the
// foreman.
func (f *floor) runPrompt(line string) error {
	m := f.m
	if ClassifyWith(line, Harnesses, f.o.Views) == TalkLine {
		return f.talk(line)
	}
	words := strings.Fields(line)
	if f.viewWord(words) {
		return nil
	}
	switch words[0] {
	case "list":
		return f.refresh()
	case "read":
		if len(words) < 2 {
			m.Message = "read needs a pane id. Run: list"
			return nil
		}
		if !m.has(words[1]) {
			m.Message = "no pane " + words[1] + ". Run: list"
			return nil
		}
		m.Cursor = m.indexOf(words[1], m.Cursor)
		return f.peek()
	case "close":
		if len(words) < 2 {
			m.Message = "close needs a pane id. Run: list"
			return nil
		}
		if _, err := call(f.o.Socket, "pane.close", map[string]any{"pane": words[1]}); err != nil {
			return f.fail(err)
		}
		return f.refresh()
	case "open":
		if len(words) < 2 {
			m.Message = "open needs a harness name, one of " + strings.Join(Harnesses, " ")
			return nil
		}
		if _, err := f.createHarnessPane(words[1], words[2:]...); err != nil {
			return f.fail(err)
		}
		return f.refresh()
	case "swap":
		if len(words) != 3 {
			m.Message = swapNeedsMessage
			return nil
		}
		i, erri := strconv.Atoi(words[1])
		j, errj := strconv.Atoi(words[2])
		if erri != nil || errj != nil {
			m.Message = swapNeedsMessage
			return nil
		}
		n := len(m.Tiles.Slots)
		if i < 1 || i > n || j < 1 || j > n {
			m.Message = fmt.Sprintf("swap needs slot numbers from 1 to %d", n)
			return nil
		}
		m.Message = tilesMessage(m.Tiles.Swap(i-1, j-1))
	case "rotate":
		m.Message = tilesMessage(m.Tiles.Rotate())
	case "reset":
		m.Message = tilesMessage(m.Tiles.Reset(m.rosterOrder()))
	case "lock", "unlock":
		m.Tiles.Locked = words[0] == "lock"
		return f.saveFloor()
	case "layout":
		return f.setLayout(words[1:])
	case "zoom":
		id := m.Typing
		if id == "" {
			if r, ok := m.Selected(); ok {
				if r.Parent != "" {
					m.Message = ChildMessage
					return nil
				}
				id = r.ID
			}
		}
		if id == "" {
			m.Message = "zoom needs a pane. Run: list"
			return nil
		}
		return f.attachTo(id)
	case "foreman":
		return f.foremanWord(words[1:])
	case "tree":
		if err := f.refresh(); err != nil {
			return err
		}
		cols, _ := f.o.Size()
		m.Tree = m.NewTree(cols)
	default:
		if _, err := f.createPane(words); err != nil {
			return f.fail(err)
		}
		return f.refresh()
	}
	return nil
}

// viewWord answers a view id typed alone with the floor page address of
// that view. A plumbing verb of the same name, tree among them, keeps its
// own meaning. It reports whether it answered.
func (f *floor) viewWord(words []string) bool {
	if len(words) != 1 {
		return false
	}
	for _, v := range Verbs {
		if words[0] == v {
			return false
		}
	}
	for _, id := range f.o.Views {
		if words[0] != id {
			continue
		}
		if f.o.WebURL == "" {
			f.m.Message = "views live on the floor page, and the web server is off. Run: coppice web serve --persist"
			return true
		}
		f.m.Message = "views live on the floor page: " + strings.TrimRight(f.o.WebURL, "/") + "/#/view/" + id
		return true
	}
	return false
}

// foremanWord is the prompt word foreman. Alone it names the foreman, or
// says there is none. With a harness name it asks the server to start the
// floor's foreman with that harness through floor.foreman. The server
// pages it before any sentence reaches it, and refuses while a foreman
// runs, so talk never has two places to go.
func (f *floor) foremanWord(args []string) error {
	m := f.m
	if len(m.Path) > 0 {
		return f.taskForemanWord(args)
	}
	if len(args) == 0 {
		return f.nameForeman()
	}
	if len(args) > 1 {
		m.Message = "foreman takes one harness name, as in foreman claude"
		return nil
	}
	res, err := call(f.o.Socket, "floor.foreman", map[string]any{"harness": args[0]})
	if err != nil {
		return f.fail(err)
	}
	id := str(res, "pane")
	m.Message = ForemanLabel + " opens. It gets the page once it is idle, and then your sentences."
	if err := f.refresh(); err != nil {
		return err
	}
	cols, _ := f.o.Size()
	if id != "" && windowCount(cols) > 0 && m.Tiles != nil {
		if _, shown := m.Tiles.SlotOf(id); !shown {
			m.Tiles.Fill(id)
		}
	}
	return nil
}

// nameForeman says which pane is the floor's foreman: the one the server
// tracks and runs now, as floor.facts names it, or that there is none.
func (f *floor) nameForeman() error {
	res, err := call(f.o.Socket, "floor.facts", nil)
	if err != nil {
		return f.fail(err)
	}
	fm, _ := res["foreman"].(map[string]any)
	if id := str(fm, "pane"); id != "" {
		f.m.Message = "foreman: " + str(fm, "label") + " (" + id + ")"
		return nil
	}
	f.m.Message = noForemanAnswer
	return nil
}

// taskForemanWord is the prompt word foreman while the roster is pushed
// into a task. Alone it names that task's foreman. With a harness name it
// opens that harness in a pane of the task, labelled for the task, names
// it the task's foreman, and sends it TaskPromotionLine. The floor's own
// foreman in the config file does not change.
func (f *floor) taskForemanWord(args []string) error {
	m := f.m
	sc := m.scope()
	tk, ok := m.task(sc)
	if !ok {
		m.Message = "task " + sc + " is gone"
		return nil
	}
	label := tk.Label
	if label == "" {
		label = tk.ID
	}
	if len(args) == 0 {
		if tk.Foreman == "" {
			m.Message = "no foreman for " + label + ". Type: foreman " + m.defaultHarness()
			return nil
		}
		m.Message = "foreman of " + label + ": " + m.labelOf(tk.Foreman)
		return nil
	}
	if len(args) > 1 {
		m.Message = "foreman takes one harness name, as in foreman claude"
		return nil
	}
	name := label + "-foreman"
	if err := f.refresh(); err != nil {
		return err
	}
	for _, r := range m.Rows {
		if r.Label == name && r.Parent == "" {
			m.Message = "a pane labelled " + name + " runs already. Close " + r.ID + " first."
			return nil
		}
	}
	// The foreman runs where the task works: its worktree, or its own
	// directory, or where the floor opened when the task has neither.
	params := map[string]any{"harness": args[0], "label": name, "task": sc}
	switch {
	case tk.Worktree != "":
		params["cwd"] = tk.Worktree
	case tk.Cwd != "":
		params["cwd"] = tk.Cwd
	}
	id, err := f.create(params)
	if err != nil {
		return f.fail(err)
	}
	if _, err := call(f.o.Socket, "task.set_foreman", map[string]any{"task": sc, "pane": id}); err != nil {
		_ = f.fail(err)
		return f.refresh()
	}
	go promote(f.o.Socket, id, name, TaskPromotionLine(label), promotedMessage(name, label), f.promoted)
	m.Message = name + " opens. It gets the page once it is idle."
	return f.refresh()
}

// promoteWait is how long promote waits for the new foreman pane to go
// idle. A var so a test can shrink it.
var promoteWait = 30 * time.Second

// promote waits for pane id's first idle state after it drew something,
// then sends it line with Enter. label names the pane in the messages, and
// finished is the message once the line is sent. A new pane reads idle before its
// first byte, so promote waits for working first, then for idle. A harness
// that opens on a question of its own is blocked or busy, not idle, so it
// gets no line and no Enter: an Enter there would answer the question for
// the operator. The message for the floor goes on done, which has room for
// one, so promote never blocks after the floor quits.
func promote(socket, id, label, line, finished string, done chan<- string) {
	secs := int(promoteWait / time.Second)
	end := time.Now().Add(promoteWait)
	var err error
	for _, until := range []string{"working", "idle"} {
		left := time.Until(end)
		if left <= 0 {
			err = errors.New("timed out")
			break
		}
		if _, err = callWithin(socket, "agent.wait", map[string]any{
			"pane": id, "until": until, "timeout_ms": int(left / time.Millisecond),
		}, left+callDeadline); err != nil {
			break
		}
	}
	if err != nil {
		done <- fmt.Sprintf("%s did not go idle in %d s, so nothing was sent. "+
			"Answer it in its tile, then type there: %s", label, secs, line)
		return
	}
	if _, err := call(socket, "pane.send_text", map[string]any{
		"pane": id, "text": line, "enter": true,
	}); err != nil {
		done <- "cannot send the page to " + label + ": " + err.Error()
		return
	}
	done <- finished
}

// promotedMessage is what the floor says once pane label has the page.
// The floor's foreman takes the operator's sentences. A task's foreman
// does not: it hears the task's undoable asks first.
func promotedMessage(label, task string) string {
	if task == "" {
		return label + " has its page. Type a sentence and it goes there."
	}
	return label + " is the foreman of " + task + ". The task's undoable asks go to it first."
}

// setLayout rebuilds the tiles with the named layout. The panes in the
// current slots keep their order and come first, and the rest of the
// roster follows, through Reset. A locked layout refuses. Any word that
// is not a layout name teaches the three names.
func (f *floor) setLayout(words []string) error {
	m := f.m
	if len(words) != 1 {
		m.Message = layoutNeedsMessage
		return nil
	}
	layout, ok := parseLayout(words[0])
	if !ok {
		m.Message = layoutNeedsMessage
		return nil
	}
	if m.Tiles.Locked {
		m.Message = lockedMessage
		return nil
	}
	cols, _ := f.o.Size()
	var order []string
	seen := map[string]bool{}
	for _, id := range m.Tiles.Slots {
		if id != "" && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	for _, id := range m.rosterOrder() {
		if !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	ts := tiles.New(layout, windowCount(cols))
	if err := ts.Reset(order); err != nil {
		m.Message = tilesMessage(err)
		return nil
	}
	m.Tiles = ts
	return f.saveFloor()
}

// saveFloor writes the layout and the lock to floor.json. A failed write
// becomes the message line.
func (f *floor) saveFloor() error {
	if err := writeFloorState(f.o.DataDir, f.m.Tiles.Layout, f.m.Tiles.Locked); err != nil {
		f.m.Message = "cannot write " + filepath.Join(f.o.DataDir, floorFile) + ": " + err.Error()
	}
	return nil
}

// attachTo hands the terminal to attach for pane id until the leave key
// brings it back. Every tile is detached first, and the event connection
// is drained the whole time, so the server never drops the floor for
// falling behind and no frame is lost behind a tile's back. Mouse
// reporting goes off for the attach and on again after it, on a terminal,
// so a click never reaches the pane as text. Back on the roster the screen
// is cleared in full, the list fetched again, and the next render attaches
// the shown tiles afresh, each with its size. The roster owns the keyboard
// after an attach. A headless pane never reaches attach.Run at all: see
// attachHeadless.
func (f *floor) attachTo(id string) error {
	if f.m.isHeadless(id) {
		return f.attachHeadless(id)
	}
	defer func() { f.fw.clear = true }()
	attached := id
	f.setTyping("")
	f.detachAll()
	// attach draws its own cursor, so a cursor the floor hid must show.
	_, _ = io.WriteString(f.o.Out, cursorShow)
	f.fw.lastHidden = false
	if f.tty {
		_, _ = io.WriteString(f.o.Out, mouseOff)
		defer func() { _, _ = io.WriteString(f.o.Out, mouseOn) }()
	}
	cols, rows := f.o.Size()
	stop := make(chan struct{})
	// The drain eats every line and keeps the ids of the detach and text
	// replies it saw, so the floor can clear detaching and send the text
	// still queued once it is back.
	type drainRes struct {
		err     error
		replied []string
	}
	drained := make(chan drainRes, 1)
	go func() {
		var replied []string
		for {
			select {
			case r := <-f.lines:
				if r.err != nil {
					drained <- drainRes{err: r.err}
					return
				}
				var ev map[string]any
				if json.Unmarshal(r.line, &ev) == nil {
					if id := str(ev, "id"); strings.HasPrefix(id, "vd-") || strings.HasPrefix(id, "vt-") {
						replied = append(replied, id)
					}
				}
			case <-stop:
				drained <- drainRes{replied: replied}
				return
			}
		}
	}()
	err := attach.Run(attach.Options{
		Socket: f.o.Socket, Pane: id, In: f.o.In, Out: f.o.Out,
		Cols: cols, Rows: rows, Keys: f.keys, Leave: f.o.Leave, EndOnExit: true,
	})
	endedHere := errors.Is(err, attach.ErrPaneEnded)
	if endedHere {
		// The refresh below finds the pane in Recent and draws the ended
		// line. The keys pause after it, so type ahead meant for the
		// pane never reaches the rail.
		err = nil
		defer func() {
			f.pauseKeys(f.m.nameOf(attached) + " ended. Keys paused.")
			f.m.cursorToEnded(attached)
		}()
	}
	close(stop)
	res := <-drained
	for _, id := range res.replied {
		if strings.HasPrefix(id, "vt-") {
			f.textBusy = false
			continue
		}
		delete(f.detaching, strings.TrimPrefix(id, "vd-"))
	}
	if res.err != nil {
		return fmt.Errorf("%w: the event connection closed", attach.ErrServerGone)
	}
	if err != nil {
		if errors.Is(err, attach.ErrServerGone) {
			return err
		}
		f.m.Message = err.Error()
	}
	if err := f.pumpText(); err != nil {
		return err
	}
	return f.refresh()
}

// attachHeadless stands in for attachTo on a headless pane. attach.Run
// only ever forwards raw keystrokes, which a headless agent either
// refuses outright (codex, opencode, sprig) or, for claude in
// stream-json mode, takes straight onto its stdin past the ask guard -
// the same gap agent.prompt exists to close. It goes into a window
// instead, where its own input line already answers keys correctly; a
// double click or the prompt word "zoom" on a pane already showing there
// lands here too, and simply leaves it as it was. When no window can
// show it, the screen is too narrow, or every shown slot already holds
// something else, the message line says how to reach it instead.
func (f *floor) attachHeadless(id string) error {
	if f.typeInTile(id) {
		return nil
	}
	f.m.Message = f.m.labelOf(id) + " takes whole messages. Widen the terminal, or run: coppice agent prompt " +
		id + ` "..."`
	return nil
}
