package tui

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/tiles"
)

func TestParseSGRReadsPressReleaseAndButtons(t *testing.T) {
	c, ok := ParseSGR([]byte("\x1b[<0;3;4M"))
	if !ok || c != (Click{Button: 0, X: 3, Y: 4}) {
		t.Fatalf("press = %+v %v", c, ok)
	}
	c, ok = ParseSGR([]byte("\x1b[<0;3;4m"))
	if !ok || !c.Release || c.X != 3 || c.Y != 4 {
		t.Fatalf("release = %+v %v", c, ok)
	}
	c, ok = ParseSGR([]byte("\x1b[<1;10;2M"))
	if !ok || c.Button != 1 || c.X != 10 || c.Y != 2 {
		t.Fatalf("middle = %+v %v", c, ok)
	}
	c, ok = ParseSGR([]byte("\x1b[<64;1;1M"))
	if !ok || c.Button != 64 {
		t.Fatalf("wheel = %+v %v", c, ok)
	}
}

func TestParseSGRRefusesAMalformedSequence(t *testing.T) {
	for _, in := range []string{"", "\x1b[<0;3M", "\x1b[<a;b;cM", "\x1b[<0;3;4", "\x1b[<0;3;4X", "0;3;4M", "\x1b[<0;3;4;5M"} {
		if c, ok := ParseSGR([]byte(in)); ok {
			t.Fatalf("%q parsed as %+v", in, c)
		}
	}
}

// feed returns a channel holding every byte of s after the first, so a
// readKey call on the first byte reads the rest from it.
func feed(s string) <-chan byte {
	ch := make(chan byte, len(s))
	for _, b := range []byte(s)[1:] {
		ch <- b
	}
	return ch
}

func TestReadKeyReturnsAMouseKeyForAnSGRSequence(t *testing.T) {
	ks, closed := readKey(feed("\x1b[<0;3;4M"), 0x1b)
	if closed || len(ks) != 1 || ks[0].kind != keyMouse {
		t.Fatalf("keys = %+v closed %v", ks, closed)
	}
	if ks[0].click != (Click{Button: 0, X: 3, Y: 4}) {
		t.Fatalf("click = %+v", ks[0].click)
	}
	ks, _ = readKey(feed("\x1b[<0;3;4m"), 0x1b)
	if len(ks) != 1 || !ks[0].click.Release {
		t.Fatalf("release keys = %+v", ks)
	}
}

func TestReadKeyStillReturnsEscThenTheByte(t *testing.T) {
	ks, closed := readKey(feed("\x1bq"), 0x1b)
	if closed || len(ks) != 2 || ks[0].kind != keyEsc || ks[1].kind != keyByte || ks[1].b != 'q' {
		t.Fatalf("keys = %+v closed %v", ks, closed)
	}
	ks, _ = readKey(feed("\x1b[B"), 0x1b)
	if len(ks) != 1 || ks[0].kind != keyDown {
		t.Fatalf("down keys = %+v", ks)
	}
}

func TestRenderFillsRowAtAndTileAt(t *testing.T) {
	m := tiledModel()
	m.Rows[0].State = "working"
	lines := Render(m, 140, 20)
	if len(m.RowAt) != len(lines) {
		t.Fatalf("RowAt has %d entries for %d lines", len(m.RowAt), len(lines))
	}
	if len(m.TileAt) != 140 {
		t.Fatalf("TileAt has %d entries for 140 columns", len(m.TileAt))
	}
	rows := 0
	for y, l := range lines {
		plain := stripSGR(l)
		if strings.HasPrefix(plain, "  › ") || strings.HasPrefix(plain, "    ") && strings.Contains(plain, "  working  ") ||
			strings.Contains(plain, "▾ ") {
			if m.RowAt[y] < 0 {
				t.Fatalf("line %d holds a row but RowAt says %d: %q", y, m.RowAt[y], plain)
			}
			rows++
		} else if m.RowAt[y] != -1 {
			t.Fatalf("line %d is not a row but RowAt says %d: %q", y, m.RowAt[y], plain)
		}
	}
	if rows != 3 {
		t.Fatalf("%d row and header lines found, want 3", rows)
	}
	if m.TileAt[0] != -1 || m.TileAt[139] != 0 {
		t.Fatalf("TileAt = %v", m.TileAt)
	}
	if _, ok := HitTile(m, 1); ok {
		t.Fatal("column 1 hit a tile")
	}
	if slot, ok := HitTile(m, 140); !ok || slot != 0 {
		t.Fatalf("HitTile(140) = %d %v", slot, ok)
	}
	if _, ok := HitRow(m, 1); ok {
		t.Fatal("the bar hit a row")
	}
	if _, ok := HitRow(m, 0); ok {
		t.Fatal("row 0 is outside the screen")
	}
	if _, ok := HitRow(m, 21); ok {
		t.Fatal("row 21 is outside the screen")
	}
}

func TestHitTileIsFalseWhenNoTilesShow(t *testing.T) {
	m := tiledModel()
	Render(m, 80, 20)
	for x := 1; x <= 80; x++ {
		if _, ok := HitTile(m, x); ok {
			t.Fatalf("column %d hit a tile at 80 columns", x)
		}
	}
}

// rowY returns the 1-based screen row of the roster row for pane id in
// the last frame of out.
func rowY(t *testing.T, out, id string) int {
	t.Helper()
	for i, l := range strings.Split(stripSGR(lastFrame(out)), "\r\n") {
		l = strings.TrimPrefix(l, "\x1b[K")
		if strings.HasPrefix(l, "  ") && len(strings.Fields(l)) > 0 && rowName(l) == id {
			return i + 1
		}
	}
	t.Fatalf("pane %s is not on the last frame: %q", id, lastFrame(out))
	return 0
}

// startFloor runs Run at cols columns on socket with a pipe for keys and
// waits for the first frame.
func startFloor(t *testing.T, socket string, cols int) (*io.PipeWriter, *signalWriter, chan error) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: socket, In: pr, Out: out,
			Size: func() (int, int) { return cols, 40 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	select {
	case <-out.first:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame within 5 s")
	}
	return pw, out, done
}

// quit leaves any window that has the keys, sends ctrl-c, and waits for
// Run to return. On the rail the leave key does nothing. A ctrl-c that
// lands while the leave key is still held only ends the hold, so a busy
// floor gets the pair again.
func quit(t *testing.T, pw *io.PipeWriter, done chan error) {
	t.Helper()
	for try := 0; try < 5; try++ {
		_, _ = io.WriteString(pw, "\x00")
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(pw, "\x03")
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-time.After(time.Second):
		}
	}
	t.Fatal("Run did not return after ctrl-c")
}

// unmarkedPane returns the pane id of the row the cursor is not on.
func unmarkedPane(t *testing.T, out string, ids ...string) string {
	t.Helper()
	marked := markedRow(t, lastFrame(out))
	for _, id := range ids {
		if id != marked {
			return id
		}
	}
	t.Fatal("every pane is marked")
	return ""
}

// clickRow left-clicks the roster row of pane id, reading its screen row
// from the latest frame. A state event can reorder the rows between the
// read and the click, so the click is repeated until landed reports true.
func clickRow(t *testing.T, pw *io.PipeWriter, out *signalWriter, id string, landed func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		y := rowY(t, out.String(), id)
		_, _ = io.WriteString(pw, "\x1b[<0;3;"+strconv.Itoa(y)+"M\x1b[<0;3;"+strconv.Itoa(y)+"m")
		if waitFor(t, 300*time.Millisecond, landed) {
			return
		}
	}
	t.Fatalf("the click on %s never landed: %q", id, lastFrame(out.String()))
}

func TestALeftClickOnARowMovesTheCursorThere(t *testing.T) {
	s := newTestServer(t)
	a := createShellPane(t, s.Socket(), "sh", "-c", "echo one; sleep 30")
	b := createShellPane(t, s.Socket(), "sh", "-c", "echo two; sleep 30")
	pw, out, done := startFloor(t, s.Socket(), 80)
	target := unmarkedPane(t, out.String(), a, b)
	clickRow(t, pw, out, target, func() bool { return markedRow(t, lastFrame(out.String())) == target })
	quit(t, pw, done)
}

func TestALeftClickOnARowFillsTheTileOnAWideFloor(t *testing.T) {
	s := newTestServer(t)
	a := createShellPane(t, s.Socket(), "sh", "-c", "echo one; sleep 30")
	b := createShellPane(t, s.Socket(), "sh", "-c", "echo two; sleep 30")
	pw, out, done := startFloor(t, s.Socket(), 140)
	target := unmarkedPane(t, out.String(), a, b)
	inTile := func() bool {
		for _, seg := range rightOfDivider(strings.Split(lastFrame(out.String()), "\r\n")) {
			if strings.Contains(seg, promptMark+target) {
				return true
			}
		}
		return false
	}
	clickRow(t, pw, out, target, inTile)
	if markedRow(t, lastFrame(out.String())) != target {
		t.Fatal("the cursor did not follow the click")
	}
	quit(t, pw, done)
}

// testFloor builds a floor with no server behind it, for key handling
// that never reaches the socket.
func testFloor(m *Model, cols int) *floor {
	return &floor{o: Options{Size: func() (int, int) { return cols, 20 }}, m: m}
}

func TestAMiddleClickOnARowOpensASecondSlot(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}},
		Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("a")
	f := testFloor(m, 140)
	Render(m, 140, 20)
	y := 0
	for i, r := range m.RowAt {
		if r == m.indexOf("b", 0) {
			y = i + 1
		}
	}
	if y == 0 {
		t.Fatal("row b not drawn")
	}
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 1, X: 3, Y: y}}); err != nil {
		t.Fatal(err)
	}
	if len(m.Tiles.Slots) != 2 || m.Tiles.Slots[1] != "b" || m.Tiles.Focus != 1 {
		t.Fatalf("tiles = %+v", m.Tiles)
	}
}

func TestALeftPressInATileFocusesItAndAReleaseDoesNothing(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}},
		Tiles: tiles.New(tiles.Focus, 2), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("a")
	m.Tiles.Fill("b")
	m.Tiles.Focus = 0
	f := testFloor(m, 200)
	Render(m, 200, 20)
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 0, X: 199, Y: 5}}); err != nil {
		t.Fatal(err)
	}
	if m.Tiles.Focus != 1 {
		t.Fatalf("focus = %d, want 1", m.Tiles.Focus)
	}
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 0, X: 100, Y: 5, Release: true}}); err != nil {
		t.Fatal(err)
	}
	if m.Tiles.Focus != 1 {
		t.Fatalf("a release moved focus to %d", m.Tiles.Focus)
	}
	cursor := m.Cursor
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 64, X: 3, Y: 3}}); err != nil {
		t.Fatal(err)
	}
	if m.Cursor != cursor || m.Tiles.Focus != 1 {
		t.Fatal("a wheel event changed the floor")
	}
}

func TestAClickDuringAFullScreenAttachIsNotTypedIntoThePane(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	if _, err := runFloor(t, s, scripted("\r", "\x1b[<0;3;4M", "\x1b[<0;3;4m", "\x00", "\x03")); err != nil {
		t.Fatal(err)
	}
	res := rawCall(t, s.Socket(), "pane.read", map[string]any{"pane": id, "source": "visible"})
	text, _ := res["text"].(string)
	if strings.Contains(text, "0;3;4") || strings.Contains(text, "[<") {
		t.Fatalf("the pane received the click: %q", text)
	}
}

func TestAClickAfterTheAttachReturnsStillFillsATile(t *testing.T) {
	s := newTestServer(t)
	a := createShellPane(t, s.Socket(), "sh", "-c", "echo one; sleep 30")
	b := createShellPane(t, s.Socket(), "sh", "-c", "echo two; sleep 30")
	pw, out, done := startFloor(t, s.Socket(), 140)
	if !waitFor(t, 5*time.Second, func() bool { return typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != "" }) {
		t.Fatal("the floor did not open typing in a window")
	}
	leaveTile(t, pw, out)
	_, _ = io.WriteString(pw, "\x14zoom\r")
	_, _ = io.WriteString(pw, "\x00")
	const cleared = "\x1b[2J\x1b[H\x1b[Kcoppice ·"
	if !waitFor(t, 5*time.Second, func() bool { return strings.Count(out.String(), cleared) >= 2 }) {
		t.Fatal("the roster did not come back after the attach")
	}
	target := unmarkedPane(t, out.String(), a, b)
	inTile := func() bool {
		for _, seg := range rightOfDivider(strings.Split(lastFrame(out.String()), "\r\n")) {
			if strings.Contains(seg, promptMark+target) {
				return true
			}
		}
		return false
	}
	clickRow(t, pw, out, target, inTile)
	quit(t, pw, done)
}

func TestTAndOAreTextAfterCtrlT(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}},
		Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("a")
	m.Cursor = 1
	f := testFloor(m, 140)
	for _, b := range []byte("\x14to") {
		if _, err := f.handle(key{kind: keyByte, b: b}); err != nil {
			t.Fatal(err)
		}
	}
	if m.Prompt != "to" || !m.Talking {
		t.Fatalf("t and o are not plain text: prompt %q talking %v", m.Prompt, m.Talking)
	}
	if got := strings.Join(m.Tiles.Slots, " "); got != "a" {
		t.Fatalf("t changed the tiles: %q", got)
	}
}

func TestOIsPlainTextOnThePromptLine(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	var out bytes.Buffer
	err := Run(Options{Socket: s.Socket(), In: scripted("\x14open", "\x03"), Out: &out,
		Size: func() (int, int) { return 100, 40 }, Cwd: t.TempDir(), Default: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	last := stripSGR(lastFrame(out.String()))
	if !strings.Contains(last, "› open") {
		t.Fatalf("open did not reach the prompt whole: %q", last)
	}
}

func TestReadKeyDropsAnOverlongMouseSequence(t *testing.T) {
	ks, closed := readKey(feed("\x1b[<"+strings.Repeat("1", 40)+"M"), 0x1b)
	if closed || ks != nil {
		t.Fatalf("keys = %+v closed %v, want nothing", ks, closed)
	}
}

func TestKeysAndClicksWithNoTilesDoNotPanic(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}}}
	f := testFloor(m, 140)
	Render(m, 140, 20)
	if _, err := f.handle(key{kind: keyByte, b: 't'}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 1, X: 3, Y: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 0, X: 100, Y: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handle(key{kind: keyMouse, click: Click{Button: 0, X: 3, Y: 4}}); err != nil {
		t.Fatal(err)
	}
	if m.Cursor != m.indexOf("b", 0) {
		t.Fatalf("cursor = %d, want the clicked row", m.Cursor)
	}
	// Esc lets the prompt go, so Space peeks. With no tiles the peek
	// reaches for the server and fails here. The failure must come back
	// as an error, never as a panic.
	if _, err := f.handle(key{kind: keyEsc}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handle(key{kind: keyByte, b: ' '}); err == nil {
		t.Fatal("space with no server gave no error")
	}
}
