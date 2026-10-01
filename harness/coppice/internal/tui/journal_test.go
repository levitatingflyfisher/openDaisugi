package tui

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/tiles"
)

func journalFrame(m *Model, cols, rows int) string {
	var b strings.Builder
	for _, l := range Render(m, cols, rows) {
		b.WriteString(stripSGR(l))
		b.WriteString("\n")
	}
	return b.String()
}

// j on the rail opens the journal, and the rows are the server's, newest
// first, with the agent's label. Esc closes it.
func TestJOnTheRailOpensTheJournalFromTheServer(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo up; sleep 30")
	for i, d := range []string{"allow", "deny"} {
		rawCall(t, s.Socket(), "pane.report_state", map[string]any{"pane": id, "event": map[string]any{
			"v": 1, "ts": nowSeconds(), "session_id": "s", "harness": "claude-code", "pane": id, "state": "working",
			"source": "gate", "detail": "verdict=" + d, "mode": "enforcing",
			"verdict": map[string]any{"decision": d, "tool": []string{"Read", "Bash"}[i], "clause": []string{"", "net"}[i]},
		}})
	}
	m := &Model{Rows: []Row{{ID: id, Label: "worker", State: "working"}}}
	f := &floor{o: Options{Socket: s.Socket(), Size: func() (int, int) { return 100, 20 }}, m: m}
	if _, err := f.handle(key{kind: keyByte, b: 'j'}); err != nil {
		t.Fatal(err)
	}
	if m.Journal == nil || len(m.Journal.Rows) != 2 {
		t.Fatalf("journal %+v", m.Journal)
	}
	if m.Journal.Rows[0].Decision != "deny" || m.Journal.Rows[0].Clause != "net" {
		t.Fatalf("newest first: %+v", m.Journal.Rows)
	}
	frame := journalFrame(m, 100, 20)
	for _, want := range []string{"Journal · the verdicts the gate and each agent's loop report", "worker", "deny · Bash · clause net", "allow · Read", "Up Down scroll  Esc or j closes"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("frame lacks %q:\n%s", want, frame)
		}
	}
	if strings.Index(frame, "deny · Bash") > strings.Index(frame, "allow · Read") {
		t.Fatalf("not newest first:\n%s", frame)
	}
	if _, err := f.handle(key{kind: keyEsc}); err != nil {
		t.Fatal(err)
	}
	if m.Journal != nil {
		t.Fatal("Esc left the journal open")
	}
	if _, err := f.handle(key{kind: keyByte, b: 'j'}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handle(key{kind: keyByte, b: 'j'}); err != nil {
		t.Fatal(err)
	}
	if m.Journal != nil {
		t.Fatal("j did not close the open journal")
	}
}

// The journal covers the windows and the rail stays in sight.
func TestTheJournalCoversTheWindowsAndLeavesTheRail(t *testing.T) {
	m := &Model{
		Rows:  []Row{{ID: "a", Label: "alpha", State: "working"}},
		Tiles: tiles.New(tiles.Focus, 1),
		Journal: &JournalView{Rows: []JournalRow{
			{At: 1757300000, Pane: "a", Decision: "ask", Tool: "Bash"},
		}},
	}
	m.Tiles.Fill("a")
	frame := journalFrame(m, 140, 20)
	if !strings.Contains(frame, "alpha") || !strings.Contains(frame, "ask · Bash") {
		t.Fatalf("frame:\n%s", frame)
	}
	if strings.Contains(frame, "Empty. Select a row") {
		t.Fatalf("a window shows under the journal:\n%s", frame)
	}
	for x, v := range m.TileAt {
		if v >= 0 {
			t.Fatalf("a click at column %d would reach a window under the journal", x)
		}
	}
	empty := &Model{Journal: &JournalView{}}
	if !strings.Contains(journalFrame(empty, 100, 20), "No gate verdicts yet.") {
		t.Fatal("an empty journal says nothing")
	}
	failed := &Model{Journal: &JournalView{Err: "the server is away"}}
	if !strings.Contains(journalFrame(failed, 100, 20), "The journal did not load: the server is away") {
		t.Fatal("a failed read is not named")
	}
}

// Down scrolls toward older verdicts and stops at the last one.
func TestTheJournalScrollsAndStopsAtTheEnds(t *testing.T) {
	m := &Model{Journal: &JournalView{Rows: []JournalRow{{At: 3, Pane: "a", Decision: "allow"}, {At: 2, Pane: "a", Decision: "deny"}}}}
	f := &floor{o: Options{Size: func() (int, int) { return 100, 20 }}, m: m}
	for i := 0; i < 5; i++ {
		f.journalKey(key{kind: keyDown})
	}
	if m.Journal.Top != 1 {
		t.Fatalf("top %d, want 1", m.Journal.Top)
	}
	for i := 0; i < 5; i++ {
		f.journalKey(key{kind: keyUp})
	}
	if m.Journal.Top != 0 {
		t.Fatalf("top %d, want 0", m.Journal.Top)
	}
}

// No gateway figure, no number: the header names a saving only when the
// server sent one.
func TestTheHeaderNamesTokensSavedOnlyFromAGatewayFigure(t *testing.T) {
	line := func(res map[string]any) string {
		m := &Model{Facts: decodeFacts(res)}
		return stripSGR(Render(m, 140, 20)[1])
	}
	base := func() map[string]any { return map[string]any{"daisugi": map[string]any{"armed": true}} }
	if got := line(base()); strings.Contains(got, "saved") {
		t.Fatalf("header %q names a saving with no figure", got)
	}
	for _, bad := range []any{nil, "x", map[string]any{}, map[string]any{"tokens": -3.0}, map[string]any{"tokens": "many"}} {
		res := base()
		res["saved_today"] = bad
		if got := line(res); strings.Contains(got, "saved") {
			t.Fatalf("header %q names a saving for %v", got, bad)
		}
	}
	res := base()
	res["saved_today"] = map[string]any{"tokens": 12500.0}
	if got := line(res); !strings.Contains(got, "tokens today 0   saved 12k") {
		t.Fatalf("header %q lacks the saving", got)
	}
}
