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
	"github.com/opendaisugi/coppice/internal/textwidth"
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
	// pollEvery is how often the floor refetches pane.list.
	pollEvery = 2 * time.Second
	// callDeadline bounds every short-lived request the floor makes.
	callDeadline = 10 * time.Second
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
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot reach the server at %s. Run: coppice server start",
			attach.ErrServerGone, socket)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(callDeadline))
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

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// shellArgv is the command a new shell pane runs.
func shellArgv() []string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh}
	}
	return []string{"/bin/sh"}
}

// frameWriter writes a frame only when the lines changed. A frame is a
// cursor home, every line cleared and written, then a cursor move to the
// end of the prompt. The screen is wiped first when the size changed or
// when a caller asked for a full clear.
type frameWriter struct {
	out   io.Writer
	last  []string
	cols  int
	clear bool
}

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

func (f *frameWriter) write(lines []string, cols, cursorCol int) error {
	if !f.clear && f.cols == cols && sameLines(lines, f.last) {
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
	fmt.Fprintf(&b, "\x1b[%d;%dH", len(lines), cursorCol+1)
	f.last, f.cols, f.clear = lines, cols, false
	_, err := io.WriteString(f.out, b.String())
	return err
}

// key is one decoded keystroke. click is set for a keyMouse key.
type key struct {
	kind  keyKind
	b     byte
	click Click
}

type keyKind int

const (
	keyByte keyKind = iota
	keyUp
	keyDown
	keyEsc
	keyMouse
)

// readKey turns the byte b and, for ESC, what follows it on keys into
// keystrokes. A bare ESC waits escWait for a next byte. A "[" or "O"
// starts a sequence that ends at the first byte in "@" to "~": "[A" is
// Up, "[B" is Down, "[<" opens a mouse report that ends at "M" or "m" and
// becomes a keyMouse key, and every other sequence is dropped. A report
// longer than mouseSeqMax bytes is dropped too. Any other byte means ESC
// was a key of its own, and that byte is returned after it. closed is
// true when keys closed during the wait.
func readKey(keys <-chan byte, b byte) (out []key, closed bool) {
	if b != 0x1b {
		return []key{{kind: keyByte, b: b}}, false
	}
	select {
	case c, ok := <-keys:
		if !ok {
			return []key{{kind: keyEsc}}, true
		}
		if c != '[' && c != 'O' {
			return []key{{kind: keyEsc}, {kind: keyByte, b: c}}, false
		}
		seq := []byte{0x1b, c}
		for {
			select {
			case d, ok := <-keys:
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
					}
					return nil, false
				}
			case <-time.After(escWait):
				return nil, false
			}
		}
	case <-time.After(escWait):
		return []key{{kind: keyEsc}}, false
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
}

// send writes one request on the event connection. A write that does not
// finish within callDeadline is an error.
func (f *floor) send(req map[string]any) error {
	_ = f.conn.SetWriteDeadline(time.Now().Add(callDeadline))
	defer func() { _ = f.conn.SetWriteDeadline(time.Time{}) }()
	return f.enc.Send(req)
}

// syncTiles attaches, view-only, every shown pane the floor does not watch
// yet, and detaches every watched pane no longer shown. Replies come back
// on the event connection with ids va-<pane> and vd-<pane>. An attach
// reply is ignored. A detach reply clears the pane from detaching, and a
// shown pane still in detaching is attached on the sync after that reply.
func (f *floor) syncTiles() error {
	cols, _ := f.o.Size()
	shown := map[string]bool{}
	for _, id := range f.m.Tiles.Shown(tileCols(cols)) {
		if id != "" {
			shown[id] = true
		}
	}
	for id := range shown {
		if _, ok := f.m.Screens[id]; ok || f.detaching[id] {
			continue
		}
		if err := f.send(map[string]any{
			"id": "va-" + id, "cmd": "pane.attach", "pane": id, "view_only": true,
		}); err != nil {
			return fmt.Errorf("%w: %v", attach.ErrServerGone, err)
		}
		f.m.Screens[id] = attach.NewScreen(0, 0)
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
			_, _ = io.WriteString(o.Out, mouseOff)
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

	cols, _ := o.Size()
	layout, locked := readFloorState(o.DataDir)
	ts := tiles.New(layout, tileCols(cols))
	ts.Locked = locked
	f := &floor{
		o: o,
		m: &Model{
			Default: o.Default,
			Tiles:   ts,
			Screens: map[string]*attach.Screen{},
		},
		fw:        &frameWriter{out: o.Out},
		keys:      attach.ReadKeys(o.In),
		lines:     lines,
		conn:      conn,
		enc:       enc,
		detaching: map[string]bool{},
		tty:       tty,
	}
	defer f.detachAll()
	if err := f.refresh(); err != nil {
		return err
	}
	if err := f.render(); err != nil {
		return err
	}

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case b, ok := <-f.keys:
			if !ok {
				return nil
			}
			ks, closed := readKey(f.keys, b)
			for _, k := range ks {
				quit, err := f.handle(k)
				if err != nil {
					return err
				}
				if quit {
					return nil
				}
				if err := f.render(); err != nil {
					return err
				}
			}
			if closed {
				return nil
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
			switch ev["event"] {
			case "state":
				f.m.Event(ev)
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
			if err := f.refresh(); err != nil {
				return err
			}
			if err := f.render(); err != nil {
				return err
			}
		}
	}
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

// refresh replaces the rows with a fresh pane.list. A failed list becomes
// the message line and the rows stay as they were.
func (f *floor) refresh() error {
	list, err := listPanes(f.o.Socket)
	if err != nil {
		return f.fail(err)
	}
	f.m.Apply(list, nowSeconds())
	return nil
}

// render draws the floor, then brings the view-only attaches in line with
// the tiles the frame shows.
func (f *floor) render() error {
	cols, rows := f.o.Size()
	if err := f.fw.write(Render(f.m, cols, rows), cols, textwidth.Width(promptMark+f.m.Prompt)); err != nil {
		return err
	}
	return f.syncTiles()
}

// handle applies one keystroke. quit is true when the floor should close.
func (f *floor) handle(k key) (quit bool, err error) {
	m := f.m
	switch k.kind {
	case keyUp:
		m.Move(-1)
	case keyDown:
		m.Move(1)
	case keyEsc:
		m.Message = ""
		if m.Peek != nil {
			m.ClosePeek()
		} else {
			m.Prompt = ""
		}
	case keyMouse:
		f.click(k.click)
	case keyByte:
		switch {
		case k.b == 0x03:
			return true, nil
		case k.b == '\r' || k.b == '\n':
			return false, f.enter()
		case k.b == 0x7f || k.b == 0x08:
			if m.Prompt != "" {
				_, size := utf8.DecodeLastRuneInString(m.Prompt)
				m.Prompt = m.Prompt[:len(m.Prompt)-size]
			}
		case k.b == ' ' && m.Prompt == "":
			return false, f.space()
		case k.b == 't' && m.Prompt == "":
			if r, ok := m.Selected(); ok && m.Tiles != nil {
				m.Tiles.Open(r.ID)
			}
		case k.b == 'q' && m.Prompt == "":
			return true, nil
		case k.b >= 0x20:
			m.Prompt += string([]byte{k.b})
		}
	}
	return false, nil
}

// enter runs the prompt when it holds text. With an empty prompt it
// attaches the row under the cursor, or opens the default harness when the
// floor is empty.
func (f *floor) enter() error {
	m := f.m
	m.Message = ""
	if m.Prompt != "" {
		line := m.Prompt
		m.Prompt = ""
		return f.runPrompt(line)
	}
	if r, ok := m.Selected(); ok {
		return f.attachTo(r.ID)
	}
	id, err := f.createPane([]string{f.o.Default})
	if err != nil {
		return f.fail(err)
	}
	return f.attachTo(id)
}

// click applies one mouse report. A release and a wheel event do nothing.
// A left press inside a tile focuses that slot. A left press on a roster
// row moves the cursor there and fills the pane into the tiles when tiles
// show. A middle press on a row opens a tile for its pane.
func (f *floor) click(c Click) {
	m := f.m
	if c.Release || c.Button >= 64 {
		return
	}
	if c.Button == 0 && m.Tiles != nil {
		if slot, ok := HitTile(m, c.X); ok {
			m.Tiles.Focus = slot
			return
		}
	}
	i, ok := HitRow(m, c.Y)
	if !ok {
		return
	}
	m.Cursor = i
	r, ok := m.Selected()
	if !ok || m.Tiles == nil {
		return
	}
	cols, _ := f.o.Size()
	switch c.Button {
	case 0:
		if tileCols(cols) > 0 {
			m.Tiles.Fill(r.ID)
		}
	case 1:
		m.Tiles.Open(r.ID)
	}
}

// space is the Space key on an empty prompt. When the screen shows tiles
// it fills the row's pane into them. Otherwise it opens the peek.
func (f *floor) space() error {
	cols, _ := f.o.Size()
	if tileCols(cols) == 0 || f.m.Tiles == nil {
		return f.peek()
	}
	if r, ok := f.m.Selected(); ok {
		f.m.Tiles.Fill(r.ID)
	}
	return nil
}

// createPane opens a pty pane running argv in Cwd, sized to the screen
// less attach's status row, and returns its id.
func (f *floor) createPane(argv []string) (string, error) {
	cols, rows := f.o.Size()
	if rows > 1 {
		rows--
	}
	res, err := call(f.o.Socket, "pane.create", map[string]any{
		"cwd": f.o.Cwd, "kind": "pty", "cmd_argv": argv, "cols": cols, "rows": rows,
	})
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

// runPrompt runs one prompt line. Plumbing runs here. Talk has no foreman
// to go to yet, and says so.
func (f *floor) runPrompt(line string) error {
	m := f.m
	if Classify(line, Harnesses) == Talk {
		m.Message = NoForeman(f.o.Default)
		return nil
	}
	words := strings.Fields(line)
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
		if _, err := f.createPane(words[1:]); err != nil {
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
	default:
		if _, err := f.createPane(words); err != nil {
			return f.fail(err)
		}
		return f.refresh()
	}
	return nil
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
	ts := tiles.New(layout, tileCols(cols))
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

// attachTo hands the terminal to attach for pane id and follows every Next
// it asks for. Every tile is detached first, and the event connection is
// drained the whole time, so the server never drops the floor for falling
// behind and no frame is lost behind a tile's back. Mouse reporting goes
// off for the attach and on again after it, on a terminal, so a click
// never reaches the pane as text. Back on the roster the screen is
// cleared in full, the list fetched again, and the next render attaches
// the shown tiles afresh.
func (f *floor) attachTo(id string) error {
	defer func() { f.fw.clear = true }()
	f.detachAll()
	if f.tty {
		_, _ = io.WriteString(f.o.Out, mouseOff)
		defer func() { _, _ = io.WriteString(f.o.Out, mouseOn) }()
	}
	for id != "" {
		cols, rows := f.o.Size()
		stop := make(chan struct{})
		// The drain eats every line and keeps the ids of the detach replies
		// it saw, so the loop can clear detaching once it is back.
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
						if id := str(ev, "id"); strings.HasPrefix(id, "vd-") {
							replied = append(replied, strings.TrimPrefix(id, "vd-"))
						}
					}
				case <-stop:
					drained <- drainRes{replied: replied}
					return
				}
			}
		}()
		next, err := attach.Run(attach.Options{
			Socket: f.o.Socket, Pane: id, In: f.o.In, Out: f.o.Out,
			Cols: cols, Rows: rows, Keys: f.keys,
		})
		close(stop)
		res := <-drained
		for _, p := range res.replied {
			delete(f.detaching, p)
		}
		if res.err != nil {
			return fmt.Errorf("%w: the event connection closed", attach.ErrServerGone)
		}
		if err != nil {
			if errors.Is(err, attach.ErrServerGone) {
				return err
			}
			f.m.Message = err.Error()
			break
		}
		switch {
		case next.Pane != "":
			id = next.Pane
		case next.Create:
			nid, err := f.createPane(shellArgv())
			if err != nil {
				if err := f.fail(err); err != nil {
					return err
				}
				id = ""
				break
			}
			id = nid
		default:
			id = ""
		}
	}
	return f.refresh()
}
