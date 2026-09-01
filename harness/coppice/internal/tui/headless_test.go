package tui

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/textwidth"
	"github.com/opendaisugi/coppice/internal/tiles"

	// Registers the sprig adapter with the shared registry, the same way
	// cmd/coppice/main.go does via internal/adapters/all. newTestServer's
	// server otherwise knows no headless harness at all.
	_ "github.com/opendaisugi/coppice/internal/adapters/sprig"
)

// createHeadlessSprigPane starts a headless pane on the fake sprig script,
// never a real agent. COPPICE_SPRIG_BIN is process-wide, so it reaches the
// in-process server newTestServer starts.
func createHeadlessSprigPane(t *testing.T, socket string) string {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("..", "..", "testdata", "adapters", "fake-sprig.sh"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_SPRIG_BIN", bin)
	res := rawCall(t, socket, "pane.create", map[string]any{
		"cwd": t.TempDir(), "kind": "headless", "harness": "sprig",
		"cmd_argv": []string{"--session-dir", t.TempDir()},
	})
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create returned no pane: %v", res)
	}
	return id
}

// startHeadlessTypingFloor starts a floor at 140 columns through a
// recording proxy, with one headless sprig pane. The floor opens with
// that pane in its window and typing there, the same as startTypingFloor
// does for a pty pane.
func startHeadlessTypingFloor(t *testing.T) (string, *recordingProxy, *io.PipeWriter, *signalWriter, chan error) {
	t.Helper()
	s := newTestServer(t)
	id := createHeadlessSprigPane(t, s.Socket())
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startFloor(t, p.sock, 140)
	if !waitFor(t, 5*time.Second, func() bool {
		return typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != ""
	}) {
		t.Fatalf("the floor did not open typing in the window: %q", stripSGR(lastFrame(out.String())))
	}
	return id, p, pw, out, done
}

// promptsSent returns every agent.prompt request the client sent through
// p for pane, in order.
func promptsSent(p *recordingProxy, pane string) []map[string]any {
	var out []map[string]any
	for _, req := range sentReqs(p, "agent.prompt") {
		if req["pane"] == pane {
			out = append(out, req)
		}
	}
	return out
}

func TestTypingIntoAHeadlessWindowShowsThePlaceholder(t *testing.T) {
	_, _, pw, out, done := startHeadlessTypingFloor(t)
	if !strings.Contains(stripSGR(lastFrame(out.String())), headlessPlaceholder) {
		t.Fatalf("no placeholder in the window: %q", stripSGR(lastFrame(out.String())))
	}
	quit(t, pw, done)
}

func TestEnterOnAHeadlessWindowSendsOneAgentPromptAndNoRawText(t *testing.T) {
	id, p, pw, out, done := startHeadlessTypingFloor(t)
	_, _ = io.WriteString(pw, "hello sprig\r")
	if !waitFor(t, 5*time.Second, func() bool { return len(promptsSent(p, id)) == 1 }) {
		t.Fatalf("agent.prompt was not sent once: %v", promptsSent(p, id))
	}
	got := promptsSent(p, id)[0]
	if got["text"] != "hello sprig" {
		t.Fatalf("agent.prompt text = %v, want %q", got["text"], "hello sprig")
	}
	for _, cmd := range []string{"pane.send_text", "pane.send_keys"} {
		if reqs := sentReqs(p, cmd); len(reqs) != 0 {
			t.Fatalf("the headless pane got a raw %s: %v", cmd, reqs)
		}
	}
	// The proxy logs the request the moment the client writes it, well
	// before the round trip that clears the line and redraws finishes, so
	// this waits rather than checking the very next frame.
	if !waitFor(t, 5*time.Second, func() bool {
		return !strings.Contains(stripSGR(lastFrame(out.String())), headlessMark+"hello sprig")
	}) {
		t.Fatalf("the input line did not clear after Enter: %q", stripSGR(lastFrame(out.String())))
	}
	// A settle, then the same check again: nothing about the turn
	// finishing or the next poll sends agent.prompt a second time.
	time.Sleep(300 * time.Millisecond)
	if n := len(promptsSent(p, id)); n != 1 {
		t.Fatalf("agent.prompt count is %d after settling, want 1: %v", n, promptsSent(p, id))
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

// leaveHeadlessWindow presses the leave key and waits past it for the
// rail footer, the same way leaveTile does for a pty window.
func leaveHeadlessWindow(t *testing.T, pw *io.PipeWriter, out *signalWriter) {
	t.Helper()
	_, _ = io.WriteString(pw, "\x00")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(lastFrame(out.String()), Keys()) }) {
		t.Fatalf("the leave key did not give the keys back: %q", stripSGR(lastFrame(out.String())))
	}
}

func TestEscLeavesAHeadlessWindow(t *testing.T) {
	_, _, pw, out, done := startHeadlessTypingFloor(t)
	_, _ = io.WriteString(pw, "part of a line\x1b")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(lastFrame(out.String()), Keys()) }) {
		t.Fatalf("Esc did not give the keys back: %q", stripSGR(lastFrame(out.String())))
	}
	quit(t, pw, done)
}

// TestEscAndCtrlCInOneBurstStillQuits is Esc immediately followed by
// ctrl-c, with no gap: readKeyFrom decodes both from one read, since a
// bare Esc not followed by "[" or "O" returns that next byte as a key of
// its own in the same slice. The ctrl-c must reach the rail once Esc
// gives the keys back, not be dropped with it.
func TestEscAndCtrlCInOneBurstStillQuits(t *testing.T) {
	_, _, pw, _, done := startHeadlessTypingFloor(t)
	_, _ = io.WriteString(pw, "\x1b\x03")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Esc then ctrl-c in one burst did not quit the floor")
	}
}

func TestBackspaceAndCtrlUEditTheHeadlessLineLocally(t *testing.T) {
	id, p, pw, out, done := startHeadlessTypingFloor(t)
	_, _ = io.WriteString(pw, "abx\x7f")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), headlessMark+"ab")
	}) {
		t.Fatalf("backspace did not drop the last rune: %q", stripSGR(lastFrame(out.String())))
	}
	_, _ = io.WriteString(pw, "\x15")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), headlessPlaceholder)
	}) {
		t.Fatalf("ctrl-u did not clear the line: %q", stripSGR(lastFrame(out.String())))
	}
	_, _ = io.WriteString(pw, "\r")
	time.Sleep(100 * time.Millisecond)
	if len(promptsSent(p, id)) != 0 {
		t.Fatalf("Enter on an empty line sent agent.prompt: %v", promptsSent(p, id))
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

// TestAMultiLinePasteJoinsItsLinesWithSpacesInsteadOfSubmittingEach is a
// paste of three lines arriving in one read, its own \r between them: a
// \r or \n with another byte already waiting behind it is part of the
// paste, not a real Enter, and becomes a space. No agent.prompt goes out
// until a real Enter, on its own, follows.
func TestAMultiLinePasteJoinsItsLinesWithSpacesInsteadOfSubmittingEach(t *testing.T) {
	id, p, pw, out, done := startHeadlessTypingFloor(t)
	_, _ = io.WriteString(pw, "line1\rline2\rline3")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), headlessMark+"line1 line2 line3")
	}) {
		t.Fatalf("the pasted lines did not join with spaces: %q", stripSGR(lastFrame(out.String())))
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(promptsSent(p, id)); n != 0 {
		t.Fatalf("the paste's own embedded \\r sent agent.prompt %d times before any real Enter: %v", n, promptsSent(p, id))
	}
	// A real Enter, on its own with nothing behind it, submits the
	// joined line whole.
	_, _ = io.WriteString(pw, "\r")
	if !waitFor(t, 5*time.Second, func() bool { return len(promptsSent(p, id)) == 1 }) {
		t.Fatalf("the real Enter did not submit: %v", promptsSent(p, id))
	}
	if got := promptsSent(p, id)[0]["text"]; got != "line1 line2 line3" {
		t.Fatalf("agent.prompt text = %v, want %q", got, "line1 line2 line3")
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

// TestSeparateEntersEachSubmitTheirOwnLine is the control for the paste
// case above: three lines, each with its own Enter in a read of its
// own, submit as three separate agent.prompt calls, never joined.
func TestSeparateEntersEachSubmitTheirOwnLine(t *testing.T) {
	id, p, pw, out, done := startHeadlessTypingFloor(t)
	for i, line := range []string{"line1", "line2", "line3"} {
		_, _ = io.WriteString(pw, line+"\r")
		want := i + 1
		if !waitFor(t, 5*time.Second, func() bool { return len(promptsSent(p, id)) == want }) {
			t.Fatalf("agent.prompt count after %q is %d, want %d: %v",
				line, len(promptsSent(p, id)), want, promptsSent(p, id))
		}
		if got := promptsSent(p, id)[want-1]["text"]; got != line {
			t.Fatalf("agent.prompt %d text = %v, want %q", want, got, line)
		}
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

func TestLeftArrowMovesTheHeadlessCursorForAMidLineInsert(t *testing.T) {
	id, p, pw, out, done := startHeadlessTypingFloor(t)
	// "ac", left once, "b" typed at the cursor: the line reads "abc".
	_, _ = io.WriteString(pw, "ac\x1b[Db")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), headlessMark+"abc")
	}) {
		t.Fatalf("the arrow did not move the cursor for a mid-line insert: %q", stripSGR(lastFrame(out.String())))
	}
	_, _ = io.WriteString(pw, "\r")
	if !waitFor(t, 5*time.Second, func() bool { return len(promptsSent(p, id)) == 1 }) {
		t.Fatalf("agent.prompt was not sent: %v", promptsSent(p, id))
	}
	if got := promptsSent(p, id)[0]["text"]; got != "abc" {
		t.Fatalf("agent.prompt text = %v, want abc", got)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(promptsSent(p, id)); n != 1 {
		t.Fatalf("agent.prompt count is %d after settling, want 1: %v", n, promptsSent(p, id))
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

// TestHeadlessKeysStopsEditingOnceAClickMovesTheKeysElsewhere is the
// regression for the click case: a mouse report always decodes as one
// key on its own, but the outer read-ahead loop must still stop once it
// moves the keys to a different window, or a byte already waiting for
// that other window would edit this one's own input line instead - a
// headless line for a pty window's raw stream, say. X and Y here are the
// exact coordinates TestALeftClickOnATileGivesItTheKeyboard already uses
// to land on the second of two windows at 200 columns.
func TestHeadlessKeysStopsEditingOnceAClickMovesTheKeysElsewhere(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "h", State: "working", Kind: KindHeadless},
		{ID: "p", State: "working"},
	}, Tiles: tiles.New(tiles.Focus, 2), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("h")
	m.Tiles.Fill("p")
	f := testFloor(m, 200)
	Render(m, 200, 20)
	f.setTyping("h")
	m.setHeadlessBuf("h", "a", 1)

	keys := make(chan byte, 32)
	for _, b := range []byte("\x1b[<0;199;5Mb") {
		keys <- b
	}
	f.keys = keys
	first := <-keys
	if _, err := f.headlessKeys(first); err != nil {
		t.Fatal(err)
	}
	if m.Typing != "p" {
		t.Fatalf("the click did not move the keys to p: Typing = %q", m.Typing)
	}
	if got, _ := m.headlessBuf("h"); got != "a" {
		t.Fatalf("h's own input line changed: %q", got)
	}
	if got, ok := m.HeadlessLine["p"]; ok {
		t.Fatalf("the byte waiting for p, a pty window, was written into a headless input line for it: %q", got)
	}
}

// TestARefusalFromAgentPromptShowsAsTheMessageLine sends a second Enter
// while sprig's own one-turn-at-a-time rule still holds the first: sprig
// refuses "a turn is already running" outright, never queues. The line
// stays as typed, since the agent never got it.
func TestARefusalFromAgentPromptShowsAsTheMessageLine(t *testing.T) {
	s := newTestServer(t)
	id := createHeadlessSprigPane(t, s.Socket())
	t.Setenv("COPPICE_SPRIG_SLEEP", "2")
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startFloor(t, p.sock, 140)
	if !waitFor(t, 5*time.Second, func() bool {
		return typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != ""
	}) {
		t.Fatalf("the floor did not open typing: %q", stripSGR(lastFrame(out.String())))
	}
	_, _ = io.WriteString(pw, "a\r")
	if !waitFor(t, 5*time.Second, func() bool { return len(promptsSent(p, id)) == 1 }) {
		t.Fatalf("the first agent.prompt was not sent: %v", promptsSent(p, id))
	}
	_, _ = io.WriteString(pw, "b\r")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), "a turn is already running")
	}) {
		t.Fatalf("the refusal did not show on the message line: %q", stripSGR(lastFrame(out.String())))
	}
	if strings.Contains(stripSGR(lastFrame(out.String())), headlessPlaceholder) {
		t.Fatal("the refused line was cleared, though the agent never got it")
	}
	// Both attempts reach the wire; the server, not the client, is what
	// refuses the second one.
	if n := len(promptsSent(p, id)); n != 2 {
		t.Fatalf("agent.prompt went out %d times, want 2: %v", n, promptsSent(p, id))
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

// TestAPtyWindowStillForwardsRawKeys is the control: a pty window is
// unaffected by the headless input line, and never sees agent.prompt.
func TestAPtyWindowStillForwardsRawKeys(t *testing.T) {
	id, p, pw, out, done := startTypingFloor(t)
	clickTile(t, pw, out)
	_, _ = io.WriteString(pw, "hi\r")
	if !waitFor(t, 5*time.Second, func() bool { return textSent(t, p, id) == "hi\r" }) {
		t.Fatalf("the pane got %q, want hi and a carriage return", textSent(t, p, id))
	}
	if reqs := sentReqs(p, "agent.prompt"); len(reqs) != 0 {
		t.Fatalf("a pty window sent agent.prompt: %v", reqs)
	}
	leaveTile(t, pw, out)
	quit(t, pw, done)
}

// noRawAttach fails t when any request in p's lines is the full-screen
// attach.Run makes: cmd pane.attach with id "attach", never the tile
// attaches windows send, which carry other ids (va-<pane>, and so on).
func noRawAttach(t *testing.T, p *recordingProxy) {
	t.Helper()
	for _, l := range p.Lines() {
		if strings.Contains(l, `"id":"attach"`) {
			t.Fatalf("a headless pane was attached full screen, raw keystrokes and all: %q", l)
		}
	}
}

// TestEnterOnANarrowFloorRefusesToAttachAHeadlessPaneRaw is Enter on a
// screen too narrow for any window: typeInTile has nowhere to put the
// pane, and the old code fell through to attach.Run. 100 columns is
// under windowCount's own 119-column floor (railCols 38 plus one
// minWindow 80 plus its divider), so no window ever shows here.
func TestEnterOnANarrowFloorRefusesToAttachAHeadlessPaneRaw(t *testing.T) {
	s := newTestServer(t)
	id := createHeadlessSprigPane(t, s.Socket())
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startFloor(t, p.sock, 100)
	_, _ = io.WriteString(pw, "\r")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), "takes whole messages")
	}) {
		t.Fatalf("no refusal message: %q", stripSGR(lastFrame(out.String())))
	}
	if !strings.Contains(stripSGR(lastFrame(out.String())), "coppice agent prompt "+id) {
		t.Fatalf("the message does not say how: %q", stripSGR(lastFrame(out.String())))
	}
	noRawAttach(t, p)
	quit(t, pw, done)
}

// TestZoomOnAHeadlessPaneOpensItsWindowNotRawAttach is the prompt word
// "zoom" on a headless pane that already has a window, reached from the
// rail (ctrl-t opens the prompt line there; it does nothing while a
// headless window has the keys, so this leaves first). typeInTile finds
// the pane already shown and simply gives its window the keys again,
// never attach.Run.
func TestZoomOnAHeadlessPaneOpensItsWindowNotRawAttach(t *testing.T) {
	_, p, pw, out, done := startHeadlessTypingFloor(t)
	leaveHeadlessWindow(t, pw, out)
	_, _ = io.WriteString(pw, "\x14zoom\r")
	if !waitFor(t, 5*time.Second, func() bool {
		return typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) != ""
	}) {
		t.Fatalf("zoom did not give the window the keys again: %q", stripSGR(lastFrame(out.String())))
	}
	noRawAttach(t, p)
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

// TestADoubleClickOnAHeadlessWindowDoesNotAttachFullScreen mirrors
// TestADoubleClickOnATileAttachesFullScreen, the pty case, for a
// headless one: attachTo still runs, but attachHeadless finds the pane
// already shown and leaves it exactly as it was.
func TestADoubleClickOnAHeadlessWindowDoesNotAttachFullScreen(t *testing.T) {
	_, p, pw, out, done := startHeadlessTypingFloor(t)
	_, _ = io.WriteString(pw, "\x1b[<0;100;10M\x1b[<0;100;10m\x1b[<0;100;10M\x1b[<0;100;10m")
	time.Sleep(200 * time.Millisecond)
	noRawAttach(t, p)
	if typingHeader(strings.Split(lastFrame(out.String()), "\r\n")) == "" {
		t.Fatal("the double click took the window away from the headless pane")
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}

func TestHeadlessVisibleDoesNotScrollWhenTheLineFits(t *testing.T) {
	text, col := headlessVisible("hi", 2, 20)
	if want := headlessMark + "hi"; !strings.HasPrefix(text, want) {
		t.Fatalf("shown text = %q, want it to start with %q", text, want)
	}
	if want := len([]rune(headlessMark)) + 2; col != want {
		t.Fatalf("cursor column = %d, want %d", col, want)
	}
}

func TestHeadlessVisibleScrollsToKeepTheCursorOnScreen(t *testing.T) {
	line := "0123456789ABCDEF"
	text, col := headlessVisible(line, len(line), 10)
	if col != 9 {
		t.Fatalf("cursor column = %d, want 9 (the last cell of a 10-cell window)", col)
	}
	if got := strings.TrimRight(text, " "); !strings.HasSuffix(got, "F") {
		t.Fatalf("the rune at the cursor is not at the visible edge: %q", text)
	}
	if strings.Contains(text, "0") {
		t.Fatalf("the scrolled-off head of the line is still shown: %q", text)
	}
}

func TestHeadlessVisibleNeverScrollsPastWhatTheLineHas(t *testing.T) {
	// A cursor in the middle of a long line, well inside the window, does
	// not scroll at all: the head of the line stays exactly where it was.
	line := strings.Repeat("x", 50)
	text, col := headlessVisible(line, 3, 10)
	if !strings.HasPrefix(text, headlessMark+"xxx") {
		t.Fatalf("a mid-line cursor still inside the window scrolled: %q", text)
	}
	if want := len([]rune(headlessMark)) + 3; col != want {
		t.Fatalf("cursor column = %d, want %d", col, want)
	}
}

// TestHeadlessVisibleCursorNeverLandsInsideAWideRune is dropCells' own
// straddle case: a CJK rune is two cells wide, so the byte-boundary cut
// scroll asks for can land inside one, dropping one more cell than asked
// to keep the cut at a whole rune. The column must come from what was
// actually dropped, not from the ask, or it ends up a cell too far
// right - the second, off-screen half of the rune the cut kept whole.
func TestHeadlessVisibleCursorNeverLandsInsideAWideRune(t *testing.T) {
	line := strings.Repeat("日", 10)
	text, col := headlessVisible(line, len(line), 10)
	if col != 8 {
		t.Fatalf("cursor column = %d, want 8", col)
	}
	if got := textwidth.Width(text); got != 10 {
		t.Fatalf("shown width = %d, want 10: %q", got, text)
	}
	if strings.Contains(text, "日") && !strings.HasPrefix(strings.TrimRight(text, " "), "日") {
		t.Fatalf("the shown text starts mid-glyph: %q", text)
	}
}

// TestHeadlessVisibleCursorSitsAtAGlyphsOwnStartNotItsSecondCell covers
// the case where the scrolled window fills exactly to its own edge with
// whole glyphs and no padding: the cursor still belongs at the start of
// the last one, not one cell further into its own second column.
func TestHeadlessVisibleCursorSitsAtAGlyphsOwnStartNotItsSecondCell(t *testing.T) {
	line := strings.Repeat("日", 20)
	cur := 19 * len("日") // 19 whole runes typed
	text, col := headlessVisible(line, cur, 10)
	if col != 8 {
		t.Fatalf("cursor column = %d, want 8", col)
	}
	if want := strings.Repeat("日", 5); text != want {
		t.Fatalf("shown text = %q, want %q (five whole glyphs, no partial one)", text, want)
	}
}

// TestTypingPastTheWindowWidthScrollsToKeepTheCursorVisible is the
// end-to-end case, through a real render: typing well past a 140-column
// floor's one window width keeps the cursor's own rune on screen and
// scrolls the line's head out of view.
func TestTypingPastTheWindowWidthScrollsToKeepTheCursorVisible(t *testing.T) {
	id, p, pw, out, done := startHeadlessTypingFloor(t)
	long := strings.Repeat("x", 90) + strings.Repeat("y", 40)
	_, _ = io.WriteString(pw, long)
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stripSGR(lastFrame(out.String())), "yyy")
	}) {
		t.Fatalf("the line did not scroll into view: %q", stripSGR(lastFrame(out.String())))
	}
	if strings.Contains(stripSGR(lastFrame(out.String())), headlessMark+"xxx") {
		t.Fatal("the head of an overlong line is still shown; it should have scrolled off")
	}
	// The scroll is display only: the full line, unscrolled, still goes
	// out as agent.prompt.
	_, _ = io.WriteString(pw, "\r")
	if !waitFor(t, 5*time.Second, func() bool { return len(promptsSent(p, id)) == 1 }) {
		t.Fatalf("agent.prompt was not sent once: %v", promptsSent(p, id))
	}
	if got := promptsSent(p, id)[0]["text"]; got != long {
		t.Fatalf("agent.prompt text lost the scrolled-off part: got %d chars, want %d", len(got.(string)), len(long))
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(promptsSent(p, id)); n != 1 {
		t.Fatalf("agent.prompt count is %d after settling, want 1: %v", n, promptsSent(p, id))
	}
	leaveHeadlessWindow(t, pw, out)
	quit(t, pw, done)
}
