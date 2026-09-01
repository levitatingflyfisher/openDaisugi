package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The header row says the web header's facts in the web header's words,
// and takes working and needs you from the rows under it.
func TestTheHeaderRowShowsTheFloorFacts(t *testing.T) {
	f := decodeFacts(map[string]any{
		"daisugi":      map[string]any{"mode": "watching", "armed": true, "enforcing": 1.0, "watching": 2.0, "off": 0.0},
		"working":      9.0,
		"needing_you":  9.0,
		"tokens_today": map[string]any{"fresh": 1000.0, "cache_read": 1200000.0, "cache_write": 3000.0, "out": 5000.0},
		"gateway":      map[string]any{"url": "http://127.0.0.1:8787", "answers": true},
	})
	m := &Model{
		Rows: []Row{
			{ID: "a", State: "blocked", Line: "x"},
			{ID: "b", State: "working"},
			{ID: "c", State: "blocked", Held: &Held{TaskLabel: "t"}},
			{ID: "c/k", Parent: "c", State: "working"},
		},
		NeedYou: 1, Facts: f,
	}
	lines := Render(m, 120, 20)
	got := stripSGR(lines[1])
	for _, want := range []string{"daisugi · 1 enforcing · 2 watching", "gateway answers", "2 working", "1 needs you", "tokens today 1.2M"} {
		if !strings.Contains(got, want) {
			t.Fatalf("header row %q lacks %q", got, want)
		}
	}
	if narrow := Render(m, 99, 20); strings.Contains(stripSGR(narrow[1]), "daisugi") {
		t.Fatalf("a narrow screen drew the header row: %q", narrow[1])
	}
	m.Facts = decodeFacts(map[string]any{"daisugi": map[string]any{"armed": false, "off": 1.0}, "gateway": map[string]any{"url": "http://box:1"}})
	got = stripSGR(Render(m, 120, 20)[1])
	if !strings.Contains(got, "daisugi · 1 off · disarmed") || !strings.Contains(got, "gateway ?") {
		t.Fatalf("header row %q", got)
	}
}

// With no gate hook installed and no agent guarded, the floor says how to
// turn the gate on, on any screen wide enough for the line, and says
// nothing once a hook is installed or an agent is guarded.
func TestTheFloorNamesTheGateInstallUntilAHookIsInstalled(t *testing.T) {
	has := func(m *Model, cols int) bool {
		for _, l := range Render(m, cols, 20) {
			if strings.Contains(stripSGR(l), gateHint) {
				return true
			}
		}
		return false
	}
	m := &Model{Facts: decodeFacts(map[string]any{"daisugi": map[string]any{"armed": true, "installed": false}})}
	for _, cols := range []int{hintMinCols, 80, 120} {
		if !has(m, cols) {
			t.Fatalf("an empty floor with no hook at %d columns shows no hint", cols)
		}
	}
	if has(m, hintMinCols-1) {
		t.Fatal("the hint drawn on a screen too narrow for it")
	}
	for _, d := range []map[string]any{
		{"armed": true, "installed": true},
		{"armed": true, "installed": false, "watching": 1.0},
		{"armed": true},
	} {
		if has(&Model{Facts: decodeFacts(map[string]any{"daisugi": d})}, 120) {
			t.Fatalf("hint shown for %v", d)
		}
	}
}

// A quiet floor reads the ended list once in a while, not on every poll.
// An agent that left the live list, an action on Recent, or an open
// Recent reads it at once.
func TestTheEndedListIsReadOnlyWhenItCanHaveChanged(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	f := testFloor(m, 80)
	now := time.Now()
	if !f.endedWanted(map[string]bool{"a": true}, now) {
		t.Fatal("the first refresh did not read the ended list")
	}
	if f.endedWanted(map[string]bool{"a": true}, now.Add(2*time.Second)) {
		t.Fatal("a quiet poll read the ended list")
	}
	if !f.endedWanted(map[string]bool{"a": true, "gone": true}, now.Add(3*time.Second)) {
		t.Fatal("an agent that left the live list did not read it")
	}
	f.endedDue = true
	if !f.endedWanted(map[string]bool{"a": true}, now.Add(4*time.Second)) {
		t.Fatal("a Recent action did not read it")
	}
	m.RecentOpen = true
	if !f.endedWanted(map[string]bool{"a": true}, now.Add(5*time.Second)) {
		t.Fatal("an open Recent did not read it")
	}
	m.RecentOpen = false
	if f.endedWanted(map[string]bool{"a": true}, now.Add(6*time.Second)) {
		t.Fatal("a quiet poll read it")
	}
	if !f.endedWanted(map[string]bool{"a": true}, now.Add(5*time.Second+endedEvery)) {
		t.Fatal("the slow beat never read it")
	}
}

// The facts are read only on a screen that shows them (the header row, or
// at least the install hint), and a failed read
// keeps the last ones, so the row never blinks out and resizes windows.
func TestFactsAreReadOnlyWhereTheyShowAndOutliveAFailedRead(t *testing.T) {
	s := newTestServer(t)
	for _, cols := range []int{hintMinCols - 1, hintMinCols, 100, 200} {
		m := &Model{}
		f := &floor{o: Options{Socket: s.Socket(), Size: func() (int, int) { return cols, 30 }}, m: m}
		f.readFacts()
		if cols < hintMinCols {
			if m.Facts != nil {
				t.Fatalf("a %d-column floor read the facts", cols)
			}
			continue
		}
		if m.Facts == nil {
			t.Fatalf("a %d-column floor has no facts", cols)
		}
		if cols >= factsMinCols && !strings.Contains(stripSGR(Render(m, cols, 30)[1]), "daisugi · ") {
			t.Fatalf("a %d-column floor drew no facts row", cols)
		}
		last := m.Facts
		f.o.Socket = filepath.Join(t.TempDir(), "gone.sock")
		f.readFacts()
		if m.Facts != last {
			t.Fatal("a failed read dropped the facts")
		}
	}
}
