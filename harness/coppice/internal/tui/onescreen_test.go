package tui

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/tiles"
)

// fakeHarness writes a config file whose default harness is a shell that
// prints hi and waits, so n and N start something harmless.
func fakeHarness(t *testing.T, projects ...string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Projects: projects, Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "echo hi; sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
}

// livePanes is every pane.list row, live only.
func livePanes(t *testing.T, socket string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range rawCall(t, socket, "pane.list", nil)["panes"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

// startModel is three live rows: one working, one that needs you, one
// idle, in no window yet.
func startModel() *Model {
	return &Model{Rows: []Row{
		{ID: "w", Label: "work", State: "working", Age: 1},
		{ID: "b", Label: "blocked", State: "blocked", Age: 2},
		{ID: "i", Label: "idle", State: "idle", Age: 3},
	}, Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
}

func TestStartFillsWindowsNeedsYouFirstAndTypesThere(t *testing.T) {
	m := startModel()
	f := testFloor(m, 200)
	f.startFill()
	if got := strings.Join(m.Tiles.Shown(2), " "); got != "b w" {
		t.Fatalf("windows %q, want the one that needs you, then the working one", got)
	}
	if m.Typing != "b" {
		t.Fatalf("keys on %q, want b", m.Typing)
	}
	if r, _ := m.Selected(); r.ID != "b" {
		t.Fatalf("rail cursor on %q, want b", r.ID)
	}
	if m.Talking {
		t.Fatal("the floor started on the prompt line")
	}
}

func TestStartWithNobodyBlockedTypesInTheFirstWindow(t *testing.T) {
	m := startModel()
	m.Rows[1].State = "idle"
	f := testFloor(m, 120)
	f.startFill()
	if got := strings.Join(m.Tiles.Shown(1), " "); got != "w" {
		t.Fatalf("windows %q, want the working one", got)
	}
	if m.Typing != "w" {
		t.Fatalf("keys on %q, want w", m.Typing)
	}
}

// A working agent fills a window before an idle one, even when the idle
// one is younger and sits above it on the rail.
func TestStartPutsWorkingBeforeIdle(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "i", State: "idle", Age: 1},
		{ID: "w", State: "working", Age: 9},
	}, Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	f := testFloor(m, 200)
	f.startFill()
	if got := strings.Join(m.Tiles.Shown(2), " "); got != "w i" {
		t.Fatalf("windows %q, want the working one first", got)
	}
	if m.Typing != "w" {
		t.Fatalf("keys on %q, want w", m.Typing)
	}
}

// A window an agent left takes the next agent in no window, and the rail
// cursor stays on a live row, never the Recent fold.
func TestALeftWindowFillsAndTheCursorStaysOnALiveRow(t *testing.T) {
	m := recentModel()
	m.Rows = []Row{{ID: "a", State: "working"}, {ID: "b", State: "idle"}, {ID: "c", State: "idle"}}
	m.Tiles = tiles.New(tiles.Focus, 1)
	m.Screens = map[string]*attach.Screen{}
	f := testFloor(m, 200)
	f.startFill()
	if got := strings.Join(m.Tiles.Shown(2), " "); got != "a b" {
		t.Fatalf("windows %q, want a b", got)
	}
	m.Cursor = m.indexOf("c", 0)
	m.Apply([]map[string]any{{"id": "a", "state": "working"}, {"id": "b", "state": "idle"}}, 0)
	if it, _ := m.SelectedItem(); it.kind != itemRow {
		t.Fatalf("the cursor went to %v, want a live row", it.key())
	}
	m.Apply([]map[string]any{{"id": "a", "state": "working"}, {"id": "c", "state": "idle"}}, 0)
	f.fillEmpty()
	if got := strings.Join(m.Tiles.Shown(2), " "); got != "a c" {
		t.Fatalf("windows %q, want c in the window b left", got)
	}
}

func TestStartOnANarrowScreenKeepsTheKeysOnTheRail(t *testing.T) {
	m := startModel()
	f := testFloor(m, 100)
	f.startFill()
	if m.Typing != "" || m.Talking {
		t.Fatalf("typing %q talking %v, want the rail", m.Typing, m.Talking)
	}
}

// The hardware cursor sits at the typing agent's own cursor, inside its
// window, below the header and the stack line.
func TestTheCursorSitsAtTheTypingAgentsCursor(t *testing.T) {
	m := tiledModel()
	m.Typing = "a"
	sc := attach.NewScreen(10, 5)
	sc.Apply(proto.Frame{Cols: 10, Rows: 5, Cursor: [2]int{3, 2}})
	m.Screens["a"] = sc
	Render(m, 140, 30)
	if m.CursorX != m.TileX[0]+3 || m.CursorY != m.HeaderY+1+2 {
		t.Fatalf("cursor %d,%d, want %d,%d", m.CursorX, m.CursorY, m.TileX[0]+3, m.HeaderY+3)
	}
	m.Rows[0].Stack = "claude · opus"
	Render(m, 140, 30)
	if m.CursorY != m.HeaderY+1+1+2 {
		t.Fatalf("with a stack line the cursor row is %d, want %d", m.CursorY, m.HeaderY+4)
	}
}

func TestTheCursorSitsOnTheSelectedRowOrThePrompt(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}}, Cursor: 2}
	lines := Render(m, 80, 12)
	if !strings.HasPrefix(stripSGR(lines[m.CursorY]), "  › b") {
		t.Fatalf("cursor on line %d: %q", m.CursorY, lines[m.CursorY])
	}
	m.Talking = true
	Render(m, 80, 12)
	if m.CursorY != 11 {
		t.Fatalf("while talking the cursor is on line %d, want the prompt", m.CursorY)
	}
}

func TestTheFrameMovesTheHardwareCursorWhereTheRenderSaid(t *testing.T) {
	var b strings.Builder
	fw := &frameWriter{out: &b}
	if err := fw.write([]string{"a", "b"}, 1, 4, 1, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "\x1b[2;5H") {
		t.Fatalf("frame ends %q", b.String())
	}
	b.Reset()
	if err := fw.write([]string{"a", "b"}, 1, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "\x1b[1;1H") {
		t.Fatal("a cursor move alone drew nothing")
	}
	b.Reset()
	if err := fw.write([]string{"a", "b"}, 1, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), cursorHide) {
		t.Fatalf("a hidden cursor was not hidden: %q", b.String())
	}
	b.Reset()
	if err := fw.write([]string{"a", "b"}, 1, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), cursorShow) {
		t.Fatalf("the cursor did not show again: %q", b.String())
	}
}

// An agent cursor outside what its window shows hides the hardware
// cursor, where a clamp would point at the wrong cell.
func TestACursorOutsideTheWindowHides(t *testing.T) {
	m := tiledModel()
	m.Typing = "a"
	sc := attach.NewScreen(200, 5)
	sc.Apply(proto.Frame{Cols: 200, Rows: 5, Cursor: [2]int{150, 2}})
	m.Screens["a"] = sc
	Render(m, 140, 30)
	if !m.CursorHidden {
		t.Fatal("a cursor past the window's width still shows")
	}
	sc.Apply(proto.Frame{Cols: 200, Rows: 5, Cursor: [2]int{3, 2}})
	Render(m, 140, 30)
	if m.CursorHidden {
		t.Fatal("a cursor inside the window is hidden")
	}
}

// When the agent with the keys ends, keys already typed are dropped and
// keys for keyPause do nothing on the rail, the notice says so, and the
// rail cursor goes to the Recent fold, not a live neighbour.
func TestKeysPauseWhenTheTypingAgentEnds(t *testing.T) {
	m := recentModel()
	m.Rows = []Row{{ID: "a", Label: "live", State: "working"}, {ID: "b", Label: "next", State: "working"}}
	m.Tiles = tiles.New(tiles.Focus, 1)
	m.Screens = map[string]*attach.Screen{}
	keys := make(chan byte, 8)
	f := testFloor(m, 140)
	f.keys = keys
	f.setTyping("a")
	m.Tiles.Fill("a")
	for _, b := range []byte("\rnr1") {
		keys <- b
	}
	m.Rows = m.Rows[1:]
	m.Ended = append([]Row{{ID: "a", Label: "live", State: "ended"}}, m.Ended...)
	m.vacateGone()
	f.keepTyping(1)
	if m.Typing != "" || m.Paused != "live ended. Keys paused." {
		t.Fatalf("typing %q notice %q", m.Typing, m.Paused)
	}
	if len(keys) != 0 {
		t.Fatalf("%d typed keys were kept", len(keys))
	}
	if it, _ := m.SelectedItem(); it.kind != itemFold {
		t.Fatalf("the rail cursor is on %q, want the Recent fold", it.key())
	}
	for _, b := range []byte("\rn1\x17\x03") {
		if end, err := f.key(b); err != nil || end {
			t.Fatalf("key %q during the pause: end %v err %v", b, end, err)
		}
	}
	if m.Confirm != "" || m.Picker != nil || m.RecentOpen || m.Message != "" {
		t.Fatalf("a key acted during the pause: %+v", m)
	}
}

// A sequence split across the pause is dropped whole: PageUp's tail
// arriving after the pause ends, or after the drain, is never read as
// the keys 5 and ~.
func TestASequenceSplitAcrossThePauseIsDroppedWhole(t *testing.T) {
	for _, name := range []string{"drain", "pause end"} {
		m := tiledModel()
		m.Tiles = tiles.New(tiles.Focus, 2)
		m.Tiles.Fill("a")
		keys := make(chan byte, 8)
		f := testFloor(m, 200)
		f.keys = keys
		first := byte(0x1b)
		if name == "drain" {
			for _, b := range []byte("\x1b[5") {
				keys <- b
			}
		} else {
			f.pauseUntil = time.Now().Add(20 * time.Millisecond)
			keys <- '['
			keys <- '5'
		}
		go func() {
			time.Sleep(30 * time.Millisecond)
			keys <- '~'
		}()
		if name == "drain" {
			f.pauseKeys("a ended. Keys paused.")
		} else if _, err := f.key(first); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
		f.pauseUntil = time.Time{}
		for len(keys) > 0 {
			if _, err := f.key(<-keys); err != nil {
				t.Fatal(err)
			}
		}
		if m.Message != "" || m.Tiles.Slots[1] != "" {
			t.Fatalf("%s: the tail acted: message %q slots %v", name, m.Message, m.Tiles.Slots)
		}
	}
}

// Keys typed right after the agent with the keys ends reach no other pane
// and act on nothing: no attach, no new agent, no rename, no stop.
func TestTypeAheadAfterTheEndReachesNoOtherPane(t *testing.T) {
	s := newTestServer(t)
	other := createShellPane(t, s.Socket(), "sh", "-c", "echo other; sleep 30")
	short := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 2; exit 0")
	rawCall(t, s.Socket(), "pane.report_state", map[string]any{"pane": short, "event": map[string]any{
		"v": 1, "ts": nowSeconds(), "session_id": "s", "harness": "claude-code", "pane": short, "state": "blocked", "source": "gate",
		"ask": map[string]any{"id": "toolu_1", "tool": "Bash", "summary": "ls", "deadline": 9999999999.0},
	}})
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startFloor(t, p.sock, 140)
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(typingHeader(strings.Split(lastFrame(out.String()), "\r\n"))), short)
	}) {
		t.Fatalf("the floor did not type into the short agent: %q", stripSGR(lastFrame(out.String())))
	}
	if !waitFor(t, 8*time.Second, func() bool { return strings.Contains(lastFrame(out.String()), "Keys paused") }) {
		t.Fatalf("no pause notice: %q", stripSGR(lastFrame(out.String())))
	}
	_, _ = io.WriteString(pw, "\rn1r\x17\r")
	time.Sleep(keyPause + 500*time.Millisecond)
	if got := textSent(t, p, other); got != "" {
		t.Fatalf("the other pane got %q", got)
	}
	for _, l := range p.Lines() {
		if strings.Contains(l, `"id":"attach"`) || strings.Contains(l, `"cmd":"pane.create"`) ||
			strings.Contains(l, `"cmd":"pane.close"`) || strings.Contains(l, `"cmd":"pane.rename"`) {
			t.Fatalf("type ahead acted: %s", l)
		}
	}
	if len(livePanes(t, s.Socket())) != 1 {
		t.Fatal("the live pane count changed")
	}
	quit(t, pw, done)
}

// One click on a live row puts it in a window and gives it the keys. A
// click on the row's × only asks to stop it.
func TestAClickOnARowFillsAWindowAndTypesThere(t *testing.T) {
	f, m := typingFloor()
	Render(m, 200, 20)
	y := 0
	for i, r := range m.RowAt {
		if r == m.indexOf("b", 0) {
			y = i + 1
		}
	}
	if err := f.click(Click{X: 5, Y: y}); err != nil {
		t.Fatal(err)
	}
	if m.Typing != "" {
		t.Fatalf("the press alone typed into %q", m.Typing)
	}
	if err := f.click(Click{X: 5, Y: y, Release: true}); err != nil {
		t.Fatal(err)
	}
	if m.Typing != "b" {
		t.Fatalf("typing %q, want b", m.Typing)
	}
	f.setTyping("")
	if err := f.click(Click{X: m.RailW, Y: y}); err != nil {
		t.Fatal(err)
	}
	if m.Typing != "" || m.Confirm != "b" {
		t.Fatalf("a click on × typed %q confirm %q", m.Typing, m.Confirm)
	}
}

// A left press on a row and a release over a window puts the row in that
// window, trading places with what was there.
func TestDraggingARowOntoAWindowPutsItThere(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}, {ID: "c", State: "idle"}},
		Tiles: tiles.New(tiles.Focus, 2), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("a")
	m.Tiles.Fill("b")
	f := testFloor(m, 200)
	Render(m, 200, 20)
	y := 0
	for i, r := range m.RowAt {
		if r == m.indexOf("c", 0) {
			y = i + 1
		}
	}
	if err := f.click(Click{X: 5, Y: y}); err != nil {
		t.Fatal(err)
	}
	// The press moves nothing, so b in the focused window 2 stays put.
	if got := strings.Join(m.Tiles.Slots, " "); got != "a b" {
		t.Fatalf("after the press slots %q, want a b", got)
	}
	if err := f.click(Click{X: m.TileX[0] + 5, Y: 6, Release: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Tiles.Slots, " "); got != "c b" {
		t.Fatalf("slots %q, want c in window 1 and b left in window 2", got)
	}
	if err := f.click(Click{X: m.TileX[1] + 5, Y: 6, Release: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Tiles.Slots, " "); got != "c b" {
		t.Fatalf("a second release moved things: %q", got)
	}
	// Dragging c, now in window 1, onto window 2 trades the two.
	Render(m, 200, 20)
	for i, r := range m.RowAt {
		if r == m.indexOf("c", 0) {
			y = i + 1
		}
	}
	_ = f.click(Click{X: 5, Y: y})
	_ = f.click(Click{X: m.TileX[1] + 5, Y: 6, Release: true})
	if got := strings.Join(m.Tiles.Slots, " "); got != "b c" {
		t.Fatalf("slots %q, want the two windows traded", got)
	}
}

// A new agent starts at the size of the window it will fill, or the
// screen less the status row when no windows show.
func TestANewAgentStartsAtItsWindowsSize(t *testing.T) {
	m := tiledModel()
	m.Tiles = tiles.New(tiles.Focus, 2)
	m.Tiles.Fill("a")
	f := testFloor(m, 200)
	Render(m, 200, 30)
	if cols, rows := f.newSize(); cols != m.TileW[1] || rows != m.WinRows || rows != 30-3-1 {
		t.Fatalf("size %dx%d, want window 2's %dx%d", cols, rows, m.TileW[1], m.WinRows)
	}
	n := testFloor(&Model{}, 80)
	Render(n.m, 80, 20)
	if cols, rows := n.newSize(); cols != 80 || rows != 19 {
		t.Fatalf("with no windows %dx%d, want 80x19", cols, rows)
	}
}

func TestABatchSaysHowManyFailedAndTheFirstError(t *testing.T) {
	var b batch
	_ = b.add(nil)
	_ = b.add(errors.New("no pane w1:p9"))
	_ = b.add(errors.New("later"))
	if got := b.say("Forgot %d ended agents."); got != "Forgot 1 ended agents. 2 failed: no pane w1:p9" {
		t.Fatalf("message %q", got)
	}
	if err := b.add(fmt.Errorf("%w: down", attach.ErrServerGone)); err == nil {
		t.Fatal("a gone server did not end the batch")
	}
}

// On a narrower screen the typing agent moves into a shown window and
// keeps the keys.
func TestNarrowingMovesTheTypingAgentIntoAShownWindow(t *testing.T) {
	m := tiledModel()
	m.Tiles = tiles.New(tiles.Focus, 2)
	m.Tiles.Fill("a")
	m.Tiles.Fill("b")
	f := testFloor(m, 140)
	f.setTyping("b")
	f.keepTyping(1)
	if m.Typing != "b" || m.Tiles.Slots[0] != "b" {
		t.Fatalf("typing %q slots %v", m.Typing, m.Tiles.Slots)
	}
}

func TestTheTypingWindowHasAThickAccentBorder(t *testing.T) {
	m := tiledModel()
	m.Typing = "a"
	lines := Render(m, 140, 20)
	if !strings.Contains(lines[3], sgrAccent+accentDivider) {
		t.Fatalf("no thick border beside the typing window: %q", lines[3])
	}
	m.Typing = ""
	lines = Render(m, 140, 20)
	if strings.Contains(lines[3], accentDivider) {
		t.Fatal("a thick border with the keys on the rail")
	}
}

func TestARowShowsItsWindowNumber(t *testing.T) {
	m := tiledModel()
	m.Tiles = tiles.New(tiles.Focus, 2)
	m.Tiles.Fill("b")
	m.Tiles.Fill("a")
	joined := stripSGR(strings.Join(Render(m, 200, 20), "\n"))
	if !strings.Contains(joined, "  › 2 gate-refactor") || !strings.Contains(joined, "    1 docs") {
		t.Fatalf("window numbers missing: %s", joined)
	}
}

func TestANumberKeyPutsTheRowInThatWindow(t *testing.T) {
	m := tiledModel()
	m.Tiles = tiles.New(tiles.Focus, 2)
	m.Tiles.Fill("a")
	m.Cursor = m.indexOf("b", 0)
	f := testFloor(m, 200)
	handleAll(t, f, key{kind: keyByte, b: '1'})
	if got := strings.Join(m.Tiles.Slots, " "); got != "b " {
		t.Fatalf("slots %q, want b in window 1", got)
	}
	m.Cursor = m.indexOf("a", 0)
	handleAll(t, f, key{kind: keyByte, b: '1'})
	if got := strings.Join(m.Tiles.Slots, ","); got != "a," {
		t.Fatalf("slots %q, want a in window 1", got)
	}
	handleAll(t, f, key{kind: keyByte, b: '2'})
	if got := strings.Join(m.Tiles.Slots, ","); got != ",a" {
		t.Fatalf("slots %q, want a moved to window 2", got)
	}
	handleAll(t, f, key{kind: keyByte, b: '3'})
	if !strings.Contains(m.Message, "2 windows") {
		t.Fatalf("window 3 of 2: message %q", m.Message)
	}
	if m.Typing != "" {
		t.Fatal("a number key moved the keys off the rail")
	}
}

func TestAWiderScreenGrowsTheWindows(t *testing.T) {
	m := startModel()
	f := testFloor(m, 120)
	f.growSlots(windowCount(290))
	if len(m.Tiles.Slots) != 3 {
		t.Fatalf("%d slots at 290 columns, want 3", len(m.Tiles.Slots))
	}
}

func TestNStartsANewAgentNearTheSelectedRow(t *testing.T) {
	s := newTestServer(t)
	fakeHarness(t)
	first := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	if _, err := runFloor(t, s, scripted("n", "\x03")); err != nil {
		t.Fatal(err)
	}
	rows := livePanes(t, s.Socket())
	if len(rows) != 2 {
		t.Fatalf("%d panes after n, want 2", len(rows))
	}
	var cwd, newCwd string
	for _, r := range rows {
		if r["id"] == first {
			cwd, _ = r["cwd"].(string)
		} else {
			newCwd, _ = r["cwd"].(string)
			if r["harness"] != "fakeh" {
				t.Fatalf("the new agent runs %v, want the default harness", r["harness"])
			}
		}
	}
	if cwd == "" || newCwd != cwd {
		t.Fatalf("the new agent opened in %q, want %q", newCwd, cwd)
	}
}

func TestShiftNPicksAProjectByNumber(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	fakeHarness(t, dir)
	var buf strings.Builder
	err := Run(Options{Socket: s.Socket(), In: scripted("N", "1", "\x03"), Out: &buf,
		Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "fakeh"})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(stripSGR(out), "1 "+filepath.Base(dir)) {
		t.Fatalf("the picker did not list the project: %q", stripSGR(out))
	}
	rows := livePanes(t, s.Socket())
	if len(rows) != 1 || rows[0]["cwd"] != dir || rows[0]["harness"] != "fakeh" {
		t.Fatalf("panes after N 1: %v", rows)
	}
}

func TestThePickerClosesOnEsc(t *testing.T) {
	m := &Model{Picker: &Picker{Projects: []Project{{Path: "/a", Name: "a"}}}}
	f := testFloor(m, 80)
	lines := Render(m, 80, 10)
	if !strings.Contains(stripSGR(strings.Join(lines, "\n")), "Esc closes") {
		t.Fatalf("the picker footer is missing: %q", lines)
	}
	handleAll(t, f, key{kind: keyEsc})
	if m.Picker != nil {
		t.Fatal("Esc did not close the picker")
	}
}

func TestRRenamesTheSelectedAgent(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	rawCall(t, s.Socket(), "pane.rename", map[string]any{"pane": id, "label": "old"})
	out, err := runFloor(t, s, scripted("r", "\x7f\x7f\x7fnew\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripSGR(out), "› rename old: old") {
		t.Fatalf("the rename field is not on the prompt line: %q", stripSGR(out))
	}
	if got := livePanes(t, s.Socket())[0]["label"]; got != "new" {
		t.Fatalf("label %v, want new", got)
	}
}

func TestEscKeepsTheOldLabel(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: "old", State: "working"}}}
	f := testFloor(m, 80)
	handleAll(t, f, key{kind: keyByte, b: 'r'}, key{kind: keyByte, b: 'x'}, key{kind: keyEsc})
	if m.Renaming != "" || m.Prompt != "" {
		t.Fatalf("renaming %q prompt %q after Esc", m.Renaming, m.Prompt)
	}
}

// An agent that ends on its own leaves the rail and the windows, the keys
// come back to the rail, and the floor says so in amber for a non-zero
// exit. The line stays.
func TestAnAgentThatEndsLeavesWithTheEndedLine(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 1; exit 3")
	rawCall(t, s.Socket(), "pane.rename", map[string]any{"pane": id, "label": "flaky"})
	pw, out, done := startFloor(t, s.Socket(), 140)
	if !waitFor(t, 5*time.Second, func() bool { return typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != "" }) {
		t.Fatal("the floor did not open typing in the window")
	}
	if !waitFor(t, 8*time.Second, func() bool {
		return strings.Contains(lastFrame(out.String()), sgrAmber+"flaky ended (exit 3)")
	}) {
		t.Fatalf("no amber ended line: %q", stripSGR(lastFrame(out.String())))
	}
	last := stripSGR(lastFrame(out.String()))
	if typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != "" || strings.Contains(last, "hello") {
		t.Fatalf("the ended agent still shows in a window: %q", last)
	}
	if !strings.Contains(last, "▸ Recent (1)") {
		t.Fatalf("no closed Recent fold with its count: %q", last)
	}
	time.Sleep(pollEvery + 500*time.Millisecond)
	if !strings.Contains(stripSGR(lastFrame(out.String())), "flaky ended (exit 3)") {
		t.Fatal("the amber line went away on its own")
	}
	quit(t, pw, done)
}

// An agent that ends while it is attached full screen brings the floor
// back with the ended line.
func TestAnAttachedAgentThatEndsReturnsToTheFloor(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 1; exit 0")
	pw, out, done := startFloor(t, s.Socket(), 80)
	_, _ = io.WriteString(pw, "\r")
	if !waitFor(t, 8*time.Second, func() bool {
		last := stripSGR(lastFrame(out.String()))
		return strings.Contains(last, "ended (exit 0)") && strings.Contains(last, "Recent (1)")
	}) {
		t.Fatalf("the floor did not come back: %q", stripSGR(lastFrame(out.String())))
	}
	quit(t, pw, done)
}

// recentModel is one live row and two ended rows, the fold closed.
func recentModel() *Model {
	code := 2
	return &Model{
		Rows:  []Row{{ID: "a", Label: "live", State: "working"}},
		Ended: []Row{{ID: "e1", Label: "gone", State: "ended", ExitCode: &code}, {ID: "e2", Label: "old", State: "ended"}},
	}
}

func TestRecentIsClosedByDefaultAndShowsItsCount(t *testing.T) {
	m := recentModel()
	joined := stripSGR(strings.Join(Render(m, 80, 14), "\n"))
	if !strings.Contains(joined, "▸ Recent (2)") || strings.Contains(joined, "gone") {
		t.Fatalf("the fold is not closed with its count: %s", joined)
	}
	m.Cursor = m.indexOf("recent", 0)
	joined = stripSGR(strings.Join(Render(m, 80, 14), "\n"))
	if !strings.Contains(joined, "Enter opens Recent") {
		t.Fatalf("the fold footer is missing: %s", joined)
	}
}

func TestEnterOpensRecentAndClearsTheAmberLine(t *testing.T) {
	m := recentModel()
	m.EndedLine, m.EndedSticky = "gone ended (exit 2)", true
	m.Cursor = m.indexOf("recent", 0)
	f := testFloor(m, 80)
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	if !m.RecentOpen || m.EndedLine != "" {
		t.Fatalf("open %v line %q", m.RecentOpen, m.EndedLine)
	}
	joined := stripSGR(strings.Join(Render(m, 80, 16), "\n"))
	for _, want := range []string{"▾ Recent (2)", "Resume all", "Clear all", "gone", "exit 2", "old"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the open fold lacks %q: %s", want, joined)
		}
	}
	m.Cursor = m.indexOf("ended:e1", 0)
	joined = stripSGR(strings.Join(Render(m, 80, 16), "\n"))
	if !strings.Contains(joined, "Enter resume  ctrl-w forget") {
		t.Fatalf("the ended row footer is missing: %s", joined)
	}
}

func TestAStickyLineStaysAndAPlainOneGoesAfterItsTime(t *testing.T) {
	m := recentModel()
	code := 2
	m.noteEnded(Row{ID: "x", Label: "gone", ExitCode: &code})
	m.keyPressed()
	if m.EndedLine != "gone ended (exit 2)" || !m.EndedSticky {
		t.Fatalf("a key changed the amber line: %q", m.EndedLine)
	}
	m.noteEnded(Row{ID: "y", Label: "fine", ExitCode: new(int)})
	if m.EndedLine != "gone ended (exit 2)" {
		t.Fatalf("a plain line replaced the amber one: %q", m.EndedLine)
	}
	m.clearEnded()
	m.noteEnded(Row{ID: "y", Label: "fine", ExitCode: new(int)})
	if m.EndedLine != "fine ended (exit 0)" || m.EndedSticky {
		t.Fatalf("line %q sticky %v", m.EndedLine, m.EndedSticky)
	}
	m.keyPressed()
	if m.EndedLine != "fine ended (exit 0)" {
		t.Fatal("a key cleared a plain ended line before its time")
	}
	m.EndedAt = time.Now().Add(-EndedClear)
	if !m.expireEnded(time.Now()) || m.EndedLine != "" {
		t.Fatalf("a plain ended line stayed past its time: %q", m.EndedLine)
	}
	m.noteEnded(Row{ID: "x", Label: "gone", ExitCode: &code})
	m.EndedAt = time.Now().Add(-time.Hour)
	if m.expireEnded(time.Now()) || m.EndedLine == "" {
		t.Fatal("time cleared an amber line")
	}
}

func TestAClickOnTheEndedLineOpensRecent(t *testing.T) {
	m := recentModel()
	m.EndedLine, m.EndedSticky = "gone ended (exit 2)", true
	f := testFloor(m, 80)
	Render(m, 80, 14)
	if m.EndedY < 0 {
		t.Fatal("the ended line has no place")
	}
	if err := f.click(Click{X: 3, Y: m.EndedY + 1}); err != nil {
		t.Fatal(err)
	}
	if m.EndedLine != "" || !m.RecentOpen {
		t.Fatalf("line %q open %v", m.EndedLine, m.RecentOpen)
	}
}

func TestResumeAndForgetFromRecent(t *testing.T) {
	s := newTestServer(t)
	// Each shell ends the first time and stays up once resumed, so the
	// resumed one is still live when the test looks.
	once := func() string {
		mark := filepath.Join(t.TempDir(), "ran")
		return "if [ -e " + mark + " ]; then echo back; sleep 30; else touch " + mark + "; echo hello; exit 4; fi"
	}
	createShellPane(t, s.Socket(), "sh", "-c", once())
	createShellPane(t, s.Socket(), "sh", "-c", once())
	waitEnded := func(n int) {
		t.Helper()
		if !waitFor(t, 5*time.Second, func() bool {
			return len(rawCall(t, s.Socket(), "pane.list", map[string]any{"ended": true})["panes"].([]any)) == n
		}) {
			t.Fatalf("never %d ended panes", n)
		}
	}
	waitEnded(2)
	// Down to the fold, open it, down past the two actions to the newest
	// ended row, forget it, then resume the other.
	keys := "\r" + strings.Repeat("\x1b[B", 3) + "\x17" + "\r"
	if _, err := runFloor(t, s, scripted(keys, "\x03")); err != nil {
		t.Fatal(err)
	}
	ended := rawCall(t, s.Socket(), "pane.list", map[string]any{"ended": true})["panes"].([]any)
	if len(ended) != 0 {
		t.Fatalf("%d ended panes left, want none: %v", len(ended), ended)
	}
	live := livePanes(t, s.Socket())
	if len(live) != 1 {
		t.Fatalf("%d live panes after resume, want 1", len(live))
	}
	// No windows at 80 columns, so the agent starts at the screen less
	// the status row, the size a full-screen attach gives it.
	if live[0]["cols"] != 80.0 || live[0]["rows"] != 23.0 {
		t.Fatalf("resumed at %vx%v, want 80x23", live[0]["cols"], live[0]["rows"])
	}
}

func TestTheStackLineShowsWhatTheServerSends(t *testing.T) {
	rows := RowsFrom([]map[string]any{{
		"id": "a", "state": "working",
		"stack": map[string]any{"loop": "claude", "model": "opus", "router": "gateway",
			"daisugi": map[string]any{"mode": "enforcing", "armed": false}},
		"gate":   map[string]any{"decision": "allow", "tool": "Bash"},
		"tokens": map[string]any{"fresh": 950.0, "cache_read": 1200000.0, "out": 12000.0},
	}, {"id": "b", "state": "working"}}, 0)
	want := "claude · opus · gateway · daisugi enforcing disarmed · gate allow Bash · tokens 950 fresh 1.2M cache read 12k out"
	if rows[0].Stack != want {
		t.Fatalf("stack %q\nwant  %q", rows[0].Stack, want)
	}
	if rows[1].Stack != "" {
		t.Fatalf("a row with no fields has stack %q", rows[1].Stack)
	}
	m := tiledModel()
	m.Rows[0].Stack = "claude · opus"
	lines := Render(m, 140, 30)
	right := rightOfDivider(lines)
	if !strings.Contains(right[1], "claude · opus") {
		t.Fatalf("no stack line under the header: %q", right[:3])
	}
	if got := m.TileSizes["a"].Rows; got != 30-3-2 {
		t.Fatalf("inner rows %d, want the stack line taken off", got)
	}
}

func TestAWindowHeaderAndALiveRowCarryTheCloseMark(t *testing.T) {
	m := tiledModel()
	lines := Render(m, 140, 20)
	right := rightOfDivider(lines)
	if !strings.HasSuffix(strings.TrimRight(right[0], " "), closeMark) {
		t.Fatalf("the header has no close mark: %q", right[0])
	}
	f := testFloor(m, 140)
	if err := f.click(Click{X: m.TileX[0] + m.TileW[0], Y: m.HeaderY + 1}); err != nil {
		t.Fatal(err)
	}
	if m.Confirm != "a" {
		t.Fatalf("a click on the header close mark asked %q", m.Confirm)
	}
	m.Confirm = ""
	Render(m, 140, 20)
	y := 0
	for i, r := range m.RowAt {
		if r == m.indexOf("b", 0) {
			y = i + 1
		}
	}
	if err := f.click(Click{X: m.RailW, Y: y}); err != nil {
		t.Fatal(err)
	}
	if m.Confirm != "b" {
		t.Fatalf("a click on the row close mark asked %q", m.Confirm)
	}
}

func TestEveryFooterFitsAndNamesItsKeys(t *testing.T) {
	m := recentModel()
	for _, set := range []func(){
		func() { m.Cursor = 0 },
		func() { m.Cursor = 1 },
		func() { m.Confirm = "a" },
		func() { m.Confirm, m.Renaming = "", "a" },
		func() { m.Renaming, m.Talking = "", true },
		func() { m.Talking, m.Picker = false, &Picker{} },
	} {
		set()
		for _, cols := range []int{60, 120} {
			for _, l := range footerLines(m, cols) {
				if strings.TrimSpace(l) == "" {
					t.Fatalf("an empty footer at %d columns: %+v", cols, m)
				}
			}
		}
	}
	if got := fmt.Sprint(len(footerLines(&Model{}, 40))); got == "0" {
		t.Fatal("no footer at 40 columns")
	}
}
