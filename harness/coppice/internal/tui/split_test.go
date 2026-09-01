package tui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/textwidth"
	"github.com/opendaisugi/coppice/internal/tiles"
)

func TestTileColsFollowsTheWidth(t *testing.T) {
	for cols, want := range map[int]int{80: 0, 119: 0, 120: 1, 199: 1, 200: 2, 300: 2} {
		if got := tileCols(cols); got != want {
			t.Fatalf("tileCols(%d) = %d, want %d", cols, got, want)
		}
	}
}

// tiledModel is a roster of two panes with pane a in the one tile slot.
func tiledModel() *Model {
	m := &Model{Rows: []Row{
		{ID: "a", Label: "gate-refactor", Harness: "claude", State: "blocked", Line: "wants git push --force"},
		{ID: "b", Label: "docs", Harness: "pi", State: "working", Line: "editing README"},
	}, NeedYou: 1, Tiles: tiles.New(tiles.Focus, 1), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("a")
	return m
}

// rightOfDivider returns every segment drawn right of a divider.
func rightOfDivider(lines []string) []string {
	var out []string
	for _, l := range lines {
		parts := strings.Split(stripSGR(l), "│")
		if len(parts) > 1 {
			out = append(out, parts[1:]...)
		}
	}
	return out
}

func TestAWideScreenSplitsAndTheTileCarriesAHeader(t *testing.T) {
	lines := Render(tiledModel(), 140, 30)
	right := rightOfDivider(lines)
	if len(right) == 0 {
		t.Fatal("no divider at 140 columns")
	}
	if !strings.Contains(strings.Join(right, "\n"), "gate-refactor  claude  blocked") {
		t.Fatalf("tile header missing right of the divider: %q", right)
	}
	if strings.Contains(strings.Join(lines[:1], ""), "│") {
		t.Fatalf("the bar is split: %q", lines[0])
	}
	if !strings.Contains(stripSGR(strings.Join(lines, "\n")), "  › gate-refactor") {
		t.Fatal("the roster lost its cursor row")
	}
}

func TestANarrowScreenHasNoDivider(t *testing.T) {
	for _, l := range Render(tiledModel(), 80, 24) {
		if strings.Contains(l, "│") {
			t.Fatalf("divider at 80 columns: %q", l)
		}
	}
}

func TestEverySplitLineIsExactlyColsWide(t *testing.T) {
	m := tiledModel()
	m.Message = "hello"
	m.Screens["a"] = attach.NewScreen(3, 1)
	for _, cols := range []int{120, 140, 200, 201} {
		for _, l := range Render(m, cols, 20) {
			if w := textwidth.Width(stripSGR(l)); w != cols {
				t.Fatalf("at %d columns a line is %d cells: %q", cols, w, l)
			}
		}
	}
}

func TestTwoTilesAt200Columns(t *testing.T) {
	m := tiledModel()
	m.Tiles = tiles.New(tiles.Focus, 2)
	m.Tiles.Fill("a")
	m.Tiles.Fill("b")
	lines := Render(m, 200, 20)
	joined := strings.Join(rightOfDivider(lines), "\n")
	if !strings.Contains(joined, "gate-refactor  claude  blocked") || !strings.Contains(joined, "docs  pi  working") {
		t.Fatalf("two tile headers missing: %q", joined)
	}
	for _, l := range lines[1:] {
		if strings.Count(l, "│") != 2 && strings.TrimSpace(stripSGR(l)) != "" && !strings.Contains(l, Hints()) &&
			!strings.HasPrefix(stripSGR(l), promptMark) {
			t.Fatalf("a body line does not carry two dividers: %q", l)
		}
	}
}

func TestATileShowsTheScreenRowsCutToItsWidth(t *testing.T) {
	m := tiledModel()
	long := strings.Repeat("x", 100)
	sc := attach.NewScreen(100, 2)
	cells := make([]proto.Cell, 100)
	for i := range cells {
		cells[i] = proto.Cell{Text: "x", FG: "#ff0000"}
	}
	sc.Apply(proto.Frame{Cols: 100, Rows: 2, RowsChanged: map[int][]proto.Cell{0: cells}})
	m.Screens["a"] = sc
	lines := Render(m, 140, 20)
	right := rightOfDivider(lines)
	found := false
	for _, r := range right {
		if strings.HasPrefix(r, "xxxx") {
			found = true
			if strings.Contains(r, long) {
				t.Fatalf("row not cut: %q", r)
			}
			if !strings.Contains(r, textwidth.Ellipsis) {
				t.Fatalf("cut row has no ellipsis: %q", r)
			}
		}
	}
	if !found {
		t.Fatalf("screen row missing from the tile: %q", right)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "\x1b[38;2;255;0;0m") {
		t.Fatal("the tile lost the row's color")
	}
}

func TestATileCutsOrPadsTheScreenToItsHeight(t *testing.T) {
	m := tiledModel()
	sc := attach.NewScreen(4, 50)
	rows := map[int][]proto.Cell{}
	for y := 0; y < 50; y++ {
		rows[y] = []proto.Cell{{Text: "r"}, {Text: "o"}, {Text: "w"}, {}}
	}
	sc.Apply(proto.Frame{Cols: 4, Rows: 50, RowsChanged: rows})
	m.Screens["a"] = sc
	lines := Render(m, 140, 12)
	if len(lines) != 12 {
		t.Fatalf("%d lines, want 12", len(lines))
	}
	n := 0
	for _, r := range rightOfDivider(lines) {
		if strings.HasPrefix(r, "row") {
			n++
		}
	}
	if n == 0 || n > 9 {
		t.Fatalf("%d screen rows shown, want between 1 and 9", n)
	}
}

func TestAnEmptySlotSaysWhatToDo(t *testing.T) {
	m := tiledModel()
	m.Tiles = tiles.New(tiles.Focus, 1)
	right := strings.Join(rightOfDivider(Render(m, 140, 20)), "\n")
	if !strings.Contains(right, "Empty. Click a row or press Space.") {
		t.Fatalf("empty slot does not teach: %q", right)
	}
}

func TestAShownSlotWithNoScreenDrawsTheHeaderOnly(t *testing.T) {
	right := rightOfDivider(Render(tiledModel(), 140, 20))
	body := 0
	for _, r := range right {
		if strings.TrimSpace(r) != "" {
			body++
		}
	}
	if body != 1 {
		t.Fatalf("%d non-blank tile lines, want the header only: %q", body, right)
	}
}

func TestAPaneGoneFromTheRosterLeavesItsSlot(t *testing.T) {
	m := tiledModel()
	m.Apply([]map[string]any{{"id": "b", "state": "working"}}, 0)
	if _, ok := m.Tiles.SlotOf("a"); ok {
		t.Fatal("a closed pane kept its slot")
	}
}

func TestCutSGRKeepsCodesAndCutsCells(t *testing.T) {
	in := "\x1b[31mabc\x1b[0mdef\x1b[0m"
	got := cutSGR(in, 4)
	if plain := stripSGR(got); plain != "abc…" {
		t.Fatalf("plain = %q, want %q", plain, "abc…")
	}
	if !strings.HasPrefix(got, "\x1b[31mabc") {
		t.Fatalf("color lost: %q", got)
	}
	if w := textwidth.Width(stripSGR(cutSGR(in, 10))); w != 10 {
		t.Fatalf("short line padded to %d cells, want 10", w)
	}
	if !strings.Contains(cutSGR(in, 10), sgrReset) {
		t.Fatal("padded line has no reset")
	}
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// lastFrame is the last frame in out, or "" when there is none.
func lastFrame(out string) string {
	frames := splitFrames(out)
	if len(frames) == 0 {
		return ""
	}
	return frames[len(frames)-1]
}

// helloInATile reports whether the last frame of out shows hello right of
// the divider.
func helloInATile(out string) bool {
	for _, seg := range rightOfDivider(strings.Split(lastFrame(out), "\r\n")) {
		if strings.Contains(seg, "hello") {
			return true
		}
	}
	return false
}

// startWideFloor runs Run at 140 columns on socket with a pipe for keys.
func startWideFloor(t *testing.T, socket string) (*io.PipeWriter, *signalWriter, chan error) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: socket, In: pr, Out: out,
			Size: func() (int, int) { return 140, 40 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	return pw, out, done
}

func TestSpaceOnAWideFloorShowsThePaneLiveInATile(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	pw, out, done := startWideFloor(t, s.Socket())
	select {
	case <-out.first:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame within 5 s")
	}
	if helloInATile(out.String()) {
		t.Fatal("the tile was filled before Space")
	}
	_, _ = io.WriteString(pw, " ")
	if !waitFor(t, 5*time.Second, func() bool { return helloInATile(out.String()) }) {
		t.Fatalf("hello never showed in a tile: %q", lastFrame(out.String()))
	}
	time.Sleep(pollEvery + 500*time.Millisecond)
	if !helloInATile(out.String()) {
		t.Fatalf("the tile changed on a poll: %q", lastFrame(out.String()))
	}
	_, _ = io.WriteString(pw, "q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after q")
	}
}

// recordingProxy is a unix socket in front of a server. It copies bytes
// both ways and keeps every whole line in order, tagged ">" when the
// client sent it and "<" when the server did.
type recordingProxy struct {
	sock  string
	mu    sync.Mutex
	lines []string
	// holdDetach delays every detach reply on its way to the client, the
	// way a slow server would.
	holdDetach time.Duration
}

func (p *recordingProxy) log(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, line)
}

// Lines returns a copy of every tagged line so far.
func (p *recordingProxy) Lines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.lines...)
}

// Sent returns every line the client sent, joined.
func (p *recordingProxy) Sent() string {
	var b strings.Builder
	for _, l := range p.Lines() {
		if strings.HasPrefix(l, ">") {
			b.WriteString(l[1:])
		}
	}
	return b.String()
}

// lineTee logs whole lines of one direction into the proxy.
type lineTee struct {
	p   *recordingProxy
	tag string
	buf []byte
}

func (w *lineTee) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.p.log(w.tag + string(w.buf[:i+1]))
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

func newRecordingProxy(t *testing.T, target string) *recordingProxy {
	t.Helper()
	p := &recordingProxy{sock: filepath.Join(t.TempDir(), "proxy.sock")}
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("unix", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			go func() {
				_, _ = io.Copy(io.MultiWriter(up, &lineTee{p: p, tag: ">"}), c)
				_ = up.Close()
			}()
			go func() {
				defer c.Close()
				r := bufio.NewReaderSize(up, 1<<20)
				for {
					line, err := r.ReadBytes('\n')
					if len(line) > 0 {
						if p.holdDetach > 0 && strings.Contains(string(line), `"id":"vd-`) {
							time.Sleep(p.holdDetach)
						}
						p.log("<" + string(line))
						if _, werr := c.Write(line); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return p
}

// reattachOrder checks every pane that was detached and attached again:
// the second attach request must come after the detach reply. It returns
// whether any such re-attach happened and the first violation found.
func reattachOrder(lines []string) (reattached bool, violation string) {
	detachSent := map[string]bool{}
	detachReplied := map[string]bool{}
	for _, l := range lines {
		body := l[1:]
		var msg struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(body), &msg) != nil || len(msg.ID) < 4 {
			continue
		}
		kind, pane := msg.ID[:3], msg.ID[3:]
		switch {
		case l[0] == '>' && kind == "vd-":
			detachSent[pane] = true
			detachReplied[pane] = false
		case l[0] == '<' && kind == "vd-":
			detachReplied[pane] = true
		case l[0] == '>' && kind == "va-" && detachSent[pane]:
			reattached = true
			if !detachReplied[pane] && violation == "" {
				violation = "attach of " + pane + " was sent before its detach reply"
			}
		}
	}
	return reattached, violation
}

func TestRunDetachesTheTileBeforeItReturns(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	p := newRecordingProxy(t, s.Socket())
	pw, out, done := startWideFloor(t, p.sock)
	select {
	case <-out.first:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame within 5 s")
	}
	_, _ = io.WriteString(pw, " ")
	if !waitFor(t, 5*time.Second, func() bool { return helloInATile(out.String()) }) {
		t.Fatalf("hello never showed in a tile: %q", lastFrame(out.String()))
	}
	_, _ = io.WriteString(pw, "q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after q")
	}
	// The proxy copies the last bytes a moment after Run returns.
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(p.Sent(), `"id":"vd-`) })
	sent := p.Sent()
	if !strings.Contains(sent, `"id":"va-`+id+`"`) {
		t.Fatalf("no view-only attach was sent: %q", sent)
	}
	if !strings.Contains(sent, `"id":"vd-`+id+`"`) {
		t.Fatalf("no detach was sent before Run returned: %q", sent)
	}
}

func TestAReattachWaitsForTheDetachReply(t *testing.T) {
	s := newTestServer(t)
	a := createShellPane(t, s.Socket(), "sh", "-c", "echo one; sleep 30")
	b := createShellPane(t, s.Socket(), "sh", "-c", "echo two; sleep 30")
	p := newRecordingProxy(t, s.Socket())
	p.holdDetach = 200 * time.Millisecond
	pw, out, done := startFloor(t, p.sock, 140)
	showsA := func() bool {
		for _, seg := range rightOfDivider(strings.Split(lastFrame(out.String()), "\r\n")) {
			if strings.Contains(seg, promptMark+a) {
				return true
			}
		}
		return false
	}
	// Two clicks in one write: b into the tile, then a into the tile. The
	// rows can swap places between one frame and the next, so the pair is
	// repeated until some pane was detached and attached again.
	for attempt := 0; attempt < 5; attempt++ {
		ya, yb := rowY(t, out.String(), a), rowY(t, out.String(), b)
		_, _ = io.WriteString(pw, "\x1b[<0;3;"+strconv.Itoa(yb)+"M\x1b[<0;3;"+strconv.Itoa(ya)+"M")
		waitFor(t, 3*time.Second, showsA)
		waitFor(t, time.Second, func() bool { r, _ := reattachOrder(p.Lines()); return r })
		if reattached, violation := reattachOrder(p.Lines()); reattached {
			if violation != "" {
				t.Fatal(violation)
			}
			quit(t, pw, done)
			return
		}
	}
	t.Fatal("no pane was detached and attached again in five tries")
}
