package tui

import (
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/textwidth"
	"github.com/opendaisugi/coppice/internal/tiles"
)

// typingHeader returns the tile header line of the last render that holds
// the typing words, or "".
func typingHeader(lines []string) string {
	for _, l := range lines {
		if strings.Contains(stripSGR(l), "typing here · ") {
			return l
		}
	}
	return ""
}

func TestATypingTileDrawsItsHeaderInReverseWithTheLeaveKey(t *testing.T) {
	m := tiledModel()
	if typingHeader(Render(m, 140, 30)) != "" {
		t.Fatal("a tile says typing while the roster has the keyboard")
	}
	m.Typing = "a"
	head := typingHeader(Render(m, 140, 30))
	if head == "" {
		t.Fatal("no tile header says typing")
	}
	if !strings.Contains(head, sgrReverse) {
		t.Fatalf("the typing header is not in reverse video: %q", head)
	}
	if !strings.Contains(stripSGR(head), "typing here · ctrl-space leaves") {
		t.Fatalf("the header does not name the leave key: %q", stripSGR(head))
	}
	m.Leave = 0x1d
	if !strings.Contains(stripSGR(typingHeader(Render(m, 140, 30))), "typing here · ctrl-] leaves") {
		t.Fatal("the header does not follow the configured leave key")
	}
}

func TestTheTypingWordsSurviveANarrowTile(t *testing.T) {
	m := tiledModel()
	m.Rows[0].Label = strings.Repeat("a-very-long-label-", 6)
	m.Tiles = tiles.New(tiles.Focus, 2)
	m.Tiles.Fill("a")
	m.Tiles.Fill("b")
	m.Typing = "a"
	lines := Render(m, 200, 20)
	if !strings.Contains(stripSGR(typingHeader(lines)), "typing here · ctrl-space leaves") {
		t.Fatalf("the typing words were cut: %q", stripSGR(typingHeader(lines)))
	}
	for _, l := range lines {
		if w := textwidth.Width(stripSGR(l)); w != 200 {
			t.Fatalf("a line is %d cells, want 200: %q", w, stripSGR(l))
		}
	}
}

func TestTheFooterWhileTypingNamesOnlyTheWayBack(t *testing.T) {
	m := tiledModel()
	m.Typing = "a"
	joined := stripSGR(strings.Join(Render(m, 140, 30), "\n"))
	if !strings.Contains(joined, "ctrl-space back to the rail, then ctrl-c quits  click another window to type there") {
		t.Fatalf("the typing footer is missing: %q", joined)
	}
	if strings.Contains(joined, Keys()) {
		t.Fatal("the roster keys still show while a tile types")
	}
}

func TestTheRosterRowsDrawDimWhileATileTypes(t *testing.T) {
	m := tiledModel()
	m.Typing = "a"
	for _, l := range Render(m, 140, 30) {
		if rail, _, _ := strings.Cut(strings.ReplaceAll(stripSGR(l), accentDivider, divider), divider); strings.Contains(rail, "gate-refactor  claude") {
			if !strings.HasPrefix(l, sgrFaint) || strings.HasPrefix(l, sgrAmber) {
				t.Fatalf("a roster row is not dim: %q", l)
			}
			return
		}
	}
	t.Fatal("row a is not drawn")
}

func TestRenderRecordsTheInnerSizeOfEveryShownTile(t *testing.T) {
	m := tiledModel()
	Render(m, 140, 30)
	_, widths := splitWidths(140, 1)
	want := Size{Cols: widths[0], Rows: 30 - 3 - 1}
	if got := m.TileSizes["a"]; got != want {
		t.Fatalf("tile size = %+v, want %+v", got, want)
	}
	Render(m, 80, 30)
	if len(m.TileSizes) != 0 {
		t.Fatalf("a narrow screen recorded tile sizes: %+v", m.TileSizes)
	}
}

// typingFloor is a floor with two tiles, a in slot 0 and b in slot 1, at
// 200 columns, with no server behind it.
func typingFloor() (*floor, *Model) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}},
		Tiles: tiles.New(tiles.Focus, 2), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("a")
	m.Tiles.Fill("b")
	f := testFloor(m, 200)
	Render(m, 200, 20)
	return f, m
}

func TestALeftClickOnATileGivesItTheKeyboard(t *testing.T) {
	f, m := typingFloor()
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 0, X: 199, Y: 5}}); err != nil {
		t.Fatal(err)
	}
	if m.Typing != "b" || m.Tiles.Focus != 1 {
		t.Fatalf("typing %q focus %d, want b in slot 1", m.Typing, m.Tiles.Focus)
	}
}

func TestAClickOnTheRosterOrThePromptGivesTheKeyboardBack(t *testing.T) {
	f, m := typingFloor()
	for _, c := range []Click{{X: 3, Y: 10}, {X: 150, Y: 20}, {X: 150, Y: 19}} {
		m.Typing = "a"
		if err := f.click(c); err != nil {
			t.Fatal(err)
		}
		if m.Typing != "" {
			t.Fatalf("a click at %d,%d left the keyboard with %q", c.X, c.Y, m.Typing)
		}
	}
}

func TestAClickOnAnEmptySlotGivesTheKeyboardToTheRoster(t *testing.T) {
	f, m := typingFloor()
	m.Tiles.CloseSlot(1)
	Render(m, 200, 20)
	m.Typing = "a"
	if err := f.click(Click{X: 199, Y: 5}); err != nil {
		t.Fatal(err)
	}
	if m.Typing != "" {
		t.Fatalf("an empty slot took the keyboard for %q", m.Typing)
	}
}

func TestZoomNeedsAPane(t *testing.T) {
	f, m := typingFloor()
	m.Rows = nil
	if err := f.runPrompt("zoom"); err != nil {
		t.Fatal(err)
	}
	if m.Message == "" {
		t.Fatal("zoom on an empty floor said nothing")
	}
	if Classify("zoom", Harnesses) != Plumbing {
		t.Fatal("zoom is not a plumbing word")
	}
}

func TestWholeRunesKeepsASplitRuneForLater(t *testing.T) {
	b := []byte("hé")
	text, rest := wholeRunes(b[:2])
	if text != "h" || string(rest) != string(b[1:2]) {
		t.Fatalf("got %q rest %q", text, rest)
	}
	text, rest = wholeRunes(append(rest, b[2]))
	if text != "é" || len(rest) != 0 {
		t.Fatalf("got %q rest %q", text, rest)
	}
	text, rest = wholeRunes([]byte{'a', 0xff, 'b'})
	if text != "ab" || len(rest) != 0 {
		t.Fatalf("an invalid byte was kept: %q rest %q", text, rest)
	}
	text, _ = wholeRunes([]byte("\r\x03\x00\x1b[A"))
	if text != "\r\x03\x00\x1b[A" {
		t.Fatalf("control bytes changed: %q", text)
	}
}

// sentReqs returns every request the client sent through p for cmd, in
// order.
func sentReqs(p *recordingProxy, cmd string) []map[string]any {
	var out []map[string]any
	for _, l := range p.Lines() {
		if !strings.HasPrefix(l, ">") {
			continue
		}
		var req map[string]any
		if json.Unmarshal([]byte(l[1:]), &req) == nil && req["cmd"] == cmd {
			out = append(out, req)
		}
	}
	return out
}

// textSent joins the text of every pane.send_text for pane, and fails the
// test on one that would add an Enter.
func textSent(t *testing.T, p *recordingProxy, pane string) string {
	t.Helper()
	var b strings.Builder
	for _, req := range sentReqs(p, "pane.send_text") {
		if req["pane"] != pane {
			continue
		}
		if enter, ok := req["enter"].(bool); !ok || enter {
			t.Fatalf("typed text asks the server for an Enter: %v", req)
		}
		text, _ := req["text"].(string)
		b.WriteString(text)
	}
	return b.String()
}

// startTypingFloor starts a floor at 140 columns through a recording
// proxy, with one shell pane. The floor opens with that pane in its window
// and typing there. The leave key gives the keys back to the rail, so
// every test starts with the rail keys.
// typingTileRows is the inner height of the one window on a 140 by 40
// floor with a server: 40 rows less the bar, the facts row, the footer and
// the prompt, less the window's own header.
const typingTileRows = 40 - 4 - 1

func startTypingFloor(t *testing.T) (string, *recordingProxy, *io.PipeWriter, *signalWriter, chan error) {
	t.Helper()
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startFloor(t, p.sock, 140)
	if !waitFor(t, 5*time.Second, func() bool { return helloInATile(out.String()) }) {
		t.Fatalf("hello never showed in a tile: %q", lastFrame(out.String()))
	}
	if typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) == "" {
		t.Fatalf("the floor did not open typing in the window: %q", stripSGR(lastFrame(out.String())))
	}
	leaveTile(t, pw, out)
	return id, p, pw, out, done
}

// clickTile left-clicks the middle of the one tile and waits for its
// header to say typing.
func clickTile(t *testing.T, pw *io.PipeWriter, out *signalWriter) {
	t.Helper()
	_, _ = io.WriteString(pw, "\x1b[<0;100;10M\x1b[<0;100;10m")
	if !waitFor(t, 5*time.Second, func() bool { return typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != "" }) {
		t.Fatalf("the tile never took the keyboard: %q", stripSGR(lastFrame(out.String())))
	}
}

// leaveTile presses the leave key and waits past the hold for the roster
// footer.
func leaveTile(t *testing.T, pw *io.PipeWriter, out *signalWriter) {
	t.Helper()
	_, _ = io.WriteString(pw, "\x00")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(lastFrame(out.String()), Keys()) }) {
		t.Fatalf("the leave key did not give the keyboard back: %q", stripSGR(lastFrame(out.String())))
	}
}

func TestAClickOnATileThenTypedBytesReachThatPane(t *testing.T) {
	id, p, pw, out, done := startTypingFloor(t)
	clickTile(t, pw, out)
	_, _ = io.WriteString(pw, "hi\r")
	if !waitFor(t, 5*time.Second, func() bool { return textSent(t, p, id) == "hi\r" }) {
		t.Fatalf("the pane got %q, want hi and a carriage return", textSent(t, p, id))
	}
	if strings.Contains(stripSGR(lastFrame(out.String())), promptMark+"hi") {
		t.Fatal("the typed text reached the prompt too")
	}
	leaveTile(t, pw, out)
	quit(t, pw, done)
}

func TestTheLeaveKeyGivesTheKeyboardBackAndTheNextByteEditsThePrompt(t *testing.T) {
	id, p, pw, out, done := startTypingFloor(t)
	clickTile(t, pw, out)
	leaveTile(t, pw, out)
	_, _ = io.WriteString(pw, "\x14x")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), promptMark+"x")
	}) {
		t.Fatalf("x did not reach the prompt: %q", stripSGR(lastFrame(out.String())))
	}
	if got := textSent(t, p, id); got != "" {
		t.Fatalf("the pane got %q after the leave key", got)
	}
	quit(t, pw, done)
}

func TestTheLeaveKeyTwiceSendsOneLeaveByteToThePane(t *testing.T) {
	id, p, pw, out, done := startTypingFloor(t)
	clickTile(t, pw, out)
	_, _ = io.WriteString(pw, "\x00\x00")
	if !waitFor(t, 5*time.Second, func() bool { return textSent(t, p, id) == "\x00" }) {
		t.Fatalf("the pane got %q, want one leave byte", textSent(t, p, id))
	}
	if typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) == "" {
		t.Fatal("the pair gave the keyboard away")
	}
	leaveTile(t, pw, out)
	quit(t, pw, done)
}

func TestCtrlCInATypingTileReachesThePaneAndDoesNotQuit(t *testing.T) {
	id, p, pw, out, done := startTypingFloor(t)
	clickTile(t, pw, out)
	_, _ = io.WriteString(pw, "\x03")
	if !waitFor(t, 5*time.Second, func() bool { return textSent(t, p, id) == "\x03" }) {
		t.Fatalf("the pane got %q, want ctrl-c", textSent(t, p, id))
	}
	select {
	case err := <-done:
		t.Fatalf("ctrl-c in a typing tile quit the floor: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// The shell dies of the ctrl-c, and the floor brings the keys back
	// to the rail with the ended line.
	if !waitFor(t, 5*time.Second, func() bool {
		last := stripSGR(lastFrame(out.String()))
		return strings.Contains(last, id+" ended") && strings.Contains(last, "Recent (1)")
	}) {
		t.Fatalf("the floor did not come back to the rail: %q", stripSGR(lastFrame(out.String())))
	}
	if typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != "" {
		t.Fatal("a window still types after its agent ended")
	}
	quit(t, pw, done)
}

func TestATileAttachCarriesItsInnerSize(t *testing.T) {
	id, p, pw, _, done := startTypingFloor(t)
	_, widths := splitWidths(140, 1)
	wantCols, wantRows := float64(widths[0]), float64(typingTileRows)
	var got map[string]any
	for _, req := range sentReqs(p, "pane.attach") {
		if req["pane"] == id {
			got = req
		}
	}
	if got == nil {
		t.Fatal("no attach was sent for the tile")
	}
	if got["cols"] != wantCols || got["rows"] != wantRows {
		t.Fatalf("attach size %v x %v, want %v x %v", got["cols"], got["rows"], wantCols, wantRows)
	}
	if v, ok := got["view_only"]; ok && v != false {
		t.Fatalf("the tile attach is view only: %v", got)
	}
	quit(t, pw, done)
}

func TestEnterOnARowWithTilesShowingTypesIntoThatPane(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startFloor(t, p.sock, 140)
	if !waitFor(t, 5*time.Second, func() bool { return helloInATile(out.String()) }) {
		t.Fatalf("hello never showed in a tile: %q", lastFrame(out.String()))
	}
	leaveTile(t, pw, out)
	_, _ = io.WriteString(pw, "\r")
	if !waitFor(t, 5*time.Second, func() bool { return typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != "" }) {
		t.Fatalf("Enter did not give a tile the keyboard: %q", stripSGR(lastFrame(out.String())))
	}
	_, _ = io.WriteString(pw, "ab")
	if !waitFor(t, 5*time.Second, func() bool { return textSent(t, p, id) == "ab" }) {
		t.Fatalf("the pane got %q, want ab", textSent(t, p, id))
	}
	for _, l := range p.Lines() {
		if strings.Contains(l, `"id":"attach"`) {
			t.Fatalf("Enter attached full screen: %q", l)
		}
	}
	leaveTile(t, pw, out)
	quit(t, pw, done)
}

func TestANewTileSizeSendsPaneResize(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	p := newRecordingProxy(t, s.Socket())
	var cols atomic.Int64
	cols.Store(140)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: p.sock, In: pr, Out: out,
			Size: func() (int, int) { return int(cols.Load()), 40 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	if !waitFor(t, 5*time.Second, func() bool { return helloInATile(out.String()) }) {
		t.Fatalf("hello never showed in a tile: %q", lastFrame(out.String()))
	}
	cols.Store(160)
	_, widths := splitWidths(160, 1)
	want := strconv.Itoa(widths[0])
	resized := func() bool {
		for _, req := range sentReqs(p, "pane.resize") {
			if req["pane"] == id && req["cols"] == float64(widths[0]) && req["rows"] == float64(typingTileRows) {
				return true
			}
		}
		return false
	}
	if !waitFor(t, pollEvery+3*time.Second, resized) {
		t.Fatalf("no pane.resize to %s columns: %v", want, sentReqs(p, "pane.resize"))
	}
	quit(t, pw, done)
}

func TestZoomAttachesFullScreenAndTheTilesAttachWithTheirSizeAfter(t *testing.T) {
	id, p, pw, out, done := startTypingFloor(t)
	_, _ = io.WriteString(pw, "\x14zoom\r")
	if !waitFor(t, 5*time.Second, func() bool {
		for _, l := range p.Lines() {
			if strings.Contains(l, `"id":"attach"`) {
				return true
			}
		}
		return false
	}) {
		t.Fatal("zoom did not attach full screen")
	}
	_, _ = io.WriteString(pw, "\x00")
	const cleared = "\x1b[2J\x1b[H\x1b[Kcoppice ·"
	if !waitFor(t, 5*time.Second, func() bool { return strings.Count(out.String(), cleared) >= 2 }) {
		t.Fatal("the roster did not come back after the zoom")
	}
	_, widths := splitWidths(140, 1)
	if !waitFor(t, 5*time.Second, func() bool {
		n := 0
		for _, req := range sentReqs(p, "pane.attach") {
			if req["id"] == "va-"+id && req["cols"] == float64(widths[0]) && req["rows"] == float64(typingTileRows) {
				n++
			}
		}
		return n >= 2
	}) {
		t.Fatalf("the tile did not attach with its size again: %v", sentReqs(p, "pane.attach"))
	}
	quit(t, pw, done)
}

func TestADoubleClickOnATileAttachesFullScreen(t *testing.T) {
	_, p, pw, out, done := startTypingFloor(t)
	_, _ = io.WriteString(pw, "\x1b[<0;100;10M\x1b[<0;100;10m\x1b[<0;100;10M\x1b[<0;100;10m")
	if !waitFor(t, 5*time.Second, func() bool {
		for _, l := range p.Lines() {
			if strings.Contains(l, `"id":"attach"`) {
				return true
			}
		}
		return false
	}) {
		t.Fatal("a double click did not attach full screen")
	}
	_, _ = io.WriteString(pw, "\x00")
	const cleared = "\x1b[2J\x1b[H\x1b[Kcoppice ·"
	if !waitFor(t, 5*time.Second, func() bool { return strings.Count(out.String(), cleared) >= 2 }) {
		t.Fatal("the roster did not come back after the attach")
	}
	quit(t, pw, done)
}

// sizeOrder checks that no tile attach or resize for pane went out while
// the one before it still waited for its reply. It returns the first
// violation, or "".
func sizeOrder(lines []string, pane string) string {
	waiting := ""
	for _, l := range lines {
		var msg struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(l[1:]), &msg) != nil {
			continue
		}
		if msg.ID != "va-"+pane && msg.ID != "vr-"+pane {
			continue
		}
		switch l[0] {
		case '>':
			if waiting != "" {
				return msg.ID + " went out before the reply to " + waiting
			}
			waiting = msg.ID
		case '<':
			waiting = ""
		}
	}
	return ""
}

func TestATileSendsOneSizeAtATimeAndEndsAtTheLastSize(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	p := newRecordingProxy(t, s.Socket())
	p.holdSize = 300 * time.Millisecond
	var cols atomic.Int64
	cols.Store(140)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: p.sock, In: pr, Out: out,
			Size: func() (int, int) { return int(cols.Load()), 40 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	if !waitFor(t, 5*time.Second, func() bool { return len(sentReqs(p, "pane.attach")) > 0 }) {
		t.Fatal("no tile attach was sent")
	}
	for _, c := range []int64{150, 160, 170} {
		cols.Store(c)
		_, _ = io.WriteString(pw, "x\x7f")
		time.Sleep(50 * time.Millisecond)
	}
	_, widths := splitWidths(170, 1)
	last := func() bool {
		reqs := sentReqs(p, "pane.resize")
		if len(reqs) == 0 {
			return false
		}
		r := reqs[len(reqs)-1]
		return r["pane"] == id && r["cols"] == float64(widths[0]) && r["rows"] == float64(typingTileRows)
	}
	if !waitFor(t, pollEvery+5*time.Second, last) {
		t.Fatalf("the last resize is not the last tile size %d: %v", widths[0], sentReqs(p, "pane.resize"))
	}
	if v := sizeOrder(p.Lines(), id); v != "" {
		t.Fatal(v)
	}
	quit(t, pw, done)
}

// startSizedFloor runs Run through a recording proxy with a width the test
// can change and a resize channel it can signal, with one shell pane
// filled into the tile by Space.
func startSizedFloor(t *testing.T, cols *atomic.Int64) (string, *recordingProxy, *io.PipeWriter, *signalWriter, chan error, chan os.Signal) {
	t.Helper()
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	p := newRecordingProxy(t, s.Socket())
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	done := make(chan error, 1)
	resize := make(chan os.Signal, 1)
	go func() {
		done <- Run(Options{Socket: p.sock, In: pr, Out: out, Resize: resize,
			Size: func() (int, int) { return int(cols.Load()), 40 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	if !waitFor(t, 5*time.Second, func() bool { return helloInATile(out.String()) }) {
		t.Fatalf("hello never showed in a tile: %q", lastFrame(out.String()))
	}
	return id, p, pw, out, done, resize
}

func TestATerminalResizeSendsTheNewTileSizeAtOnce(t *testing.T) {
	var cols atomic.Int64
	cols.Store(140)
	id, p, pw, _, done, resize := startSizedFloor(t, &cols)
	cols.Store(160)
	resize <- syscall.SIGWINCH
	_, widths := splitWidths(160, 1)
	resized := func() bool {
		for _, req := range sentReqs(p, "pane.resize") {
			if req["pane"] == id && req["cols"] == float64(widths[0]) {
				return true
			}
		}
		return false
	}
	if !waitFor(t, time.Second, resized) {
		t.Fatalf("no pane.resize within a second of the resize: %v", sentReqs(p, "pane.resize"))
	}
	quit(t, pw, done)
}

func TestNarrowingBelowTilesWhileTypingGivesTheKeyboardBack(t *testing.T) {
	var cols atomic.Int64
	cols.Store(140)
	id, p, pw, out, done, resize := startSizedFloor(t, &cols)
	clickTile(t, pw, out)
	cols.Store(100)
	resize <- syscall.SIGWINCH
	if !waitFor(t, time.Second, func() bool { return strings.Contains(lastFrame(out.String()), "Enter go in") }) {
		t.Fatalf("the roster did not take the keyboard back: %q", stripSGR(lastFrame(out.String())))
	}
	if !strings.Contains(stripSGR(lastFrame(out.String())), "left the screen. Keys paused.") {
		t.Fatalf("no pause notice: %q", stripSGR(lastFrame(out.String())))
	}
	// Keys count again once the notice goes.
	if !waitFor(t, 3*time.Second, func() bool { return !strings.Contains(lastFrame(out.String()), "Keys paused") }) {
		t.Fatal("the pause never ended")
	}
	_, _ = io.WriteString(pw, "\x14x")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), promptMark+"x")
	}) {
		t.Fatalf("x did not reach the prompt: %q", stripSGR(lastFrame(out.String())))
	}
	if got := textSent(t, p, id); got != "" {
		t.Fatalf("the hidden pane got %q", got)
	}
	quit(t, pw, done)
}

func TestAClickRightAfterTheLeaveKeyLandsAsAClick(t *testing.T) {
	_, _, pw, out, done := startTypingFloor(t)
	clickTile(t, pw, out)
	_, _ = io.WriteString(pw, "\x00\x1b[<0;3;20M\x1b[<0;3;20m")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(lastFrame(out.String()), Keys()) }) {
		t.Fatalf("the roster did not take the keyboard back: %q", stripSGR(lastFrame(out.String())))
	}
	time.Sleep(200 * time.Millisecond)
	for _, l := range strings.Split(stripSGR(lastFrame(out.String())), "\r\n") {
		l = strings.TrimPrefix(l, "\x1b[K")
		if i := strings.Index(l, "\x1b"); i >= 0 {
			l = l[:i]
		}
		if strings.HasPrefix(l, promptMark) && strings.TrimSpace(strings.TrimPrefix(l, promptMark)) != "" {
			t.Fatalf("part of the click reached the prompt: %q", l)
		}
	}
	quit(t, pw, done)
}
