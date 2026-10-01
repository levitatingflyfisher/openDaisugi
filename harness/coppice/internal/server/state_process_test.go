package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// createShellPane starts one pty pane running argv in a temp directory and
// returns its id. It fails the test on any error, so a caller reads the id
// as a fact.
func createShellPane(t *testing.T, s *Server, argv ...string) string {
	t.Helper()
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, fmt.Sprintf("%q", a))
	}
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":[`+
			strings.Join(quoted, ",")+`],"kind":"pty","label":"shell"}`)
	id, _ := result(t, got[0])["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create returned no pane id: %+v", got[0])
	}
	return id
}

// listPanes returns every pane.list row as a map.
func listPanes(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.list"}`)
	raw, _ := result(t, got[0])["panes"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		rows = append(rows, m)
	}
	return rows
}

// listEndedPanes returns every `pane.list --ended` row as a map.
func listEndedPanes(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.list","ended":true}`)
	raw, _ := result(t, got[0])["panes"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		rows = append(rows, m)
	}
	return rows
}

// rowFor finds the pane.list row with this id, or fails the test.
func rowFor(t *testing.T, rows []map[string]any, id string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r["id"] == id {
			return r
		}
	}
	t.Fatalf("no pane.list row for %s in %v", id, rows)
	return nil
}

// waitState polls pane.list until the pane's state is want, or fails once
// timeout has passed, naming the last state it saw.
func waitState(t *testing.T, s *Server, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := any(nil)
	for time.Now().Before(deadline) {
		row := rowFor(t, listPanes(t, s), id)
		last = row["state"]
		if last == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pane %s never reached state %q within %s. Last state: %v", id, want, timeout, last)
}

func TestALivePtyPaneNeverListsAsUnknown(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	row := rowFor(t, listPanes(t, s), id)
	if row["state"] == "unknown" || row["state"] == nil {
		t.Fatalf("live pane listed as unknown: %v", row)
	}
	if row["source"] != "process" {
		t.Fatalf("expected process source, got %v", row["source"])
	}
	if _, ok := row["quiet_for"].(float64); !ok {
		t.Fatalf("quiet_for missing: %v", row)
	}
}

func TestOutputFlipsIdleToWorkingAndBack(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 1; echo hi; sleep 30")
	waitState(t, s, id, "working", 5*time.Second)
	// The quiet window is five seconds.
	waitState(t, s, id, "idle", 15*time.Second)
}

// A process fact never outranks a gate hold: a blocked pane stays blocked
// while output keeps arriving.
func TestAGateBlockStaysBlockedWhileTheProcessSpeaks(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "while true; do echo tick; sleep 1; done")
	waitState(t, s, id, "working", 5*time.Second)
	deadline := nowSeconds() + 60
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.report_state","pane":"`+id+`","event":{`+
		`"state":"blocked","source":"gate","harness":"shell","session_id":"`+id+`",`+
		`"ts":1,"v":1,"detail":"","ask":{"id":"a1","tool":"Bash","summary":"rm","deadline":`+
		fmt.Sprintf("%f", deadline)+`}}}`)
	if !got[0].OK {
		t.Fatalf("report_state failed: %+v", got[0].Error)
	}
	time.Sleep(2500 * time.Millisecond)
	row := rowFor(t, listPanes(t, s), id)
	if row["state"] != "blocked" || row["source"] != "gate" {
		t.Fatalf("gate hold lost to the process source: %v", row)
	}
}

// A read computes the process fact from the last write and the read's own
// clock, not from the last tick (CP-R-50). A pane that printed and was seen
// to print lists as working at once; the 1 s tick used to leave "no output
// yet" for up to a second after the output was on the screen.
func TestAReadAfterOutputSeesTheOutputAtOnce(t *testing.T) {
	s := newPaneServer(t)
	for i := 0; i < 5; i++ {
		id := createShellPane(t, s, "sh", "-c", "echo ready; exec sleep 30")
		got := roundTrip(t, s, `{"id":"1","cmd":"pane.wait_output","pane":"`+id+`","contains":"ready","timeout_ms":10000}`)
		if !got[0].OK {
			t.Fatalf("wait_output: %+v", got[0].Error)
		}
		row := rowFor(t, listPanes(t, s), id)
		if row["state"] != "working" || row["detail"] != "output in the last 5 s" {
			t.Fatalf("run %d: a read after the output listed %v / %v", i, row["state"], row["detail"])
		}
	}
}

// quietThenRedraw leaves pane id with a standing gate idle, moves the
// clock 6 s past its last output, and then writes a few bytes of redraw.
// The redraw is stamped by the real clock, so the skew goes back to zero
// to make it a write "just now".
func quietThenRedraw(t *testing.T, s *Server, id string) *LivePane {
	t.Helper()
	lp, ok := s.Live(id)
	if !ok {
		t.Fatalf("no live pane %s", id)
	}
	// A turn drew the screen, the gate said idle, and 6 s later the process
	// source called the quiet screen idle too. That idle replaces the
	// gate's in the merged state, so the pane must remember the gate's.
	if _, err := lp.Grid.Write([]byte("turn output")); err != nil {
		t.Fatal(err)
	}
	s.processTick(id, lp)
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcGate,
	})
	s.clockSkew.Store(int64(6 * time.Second))
	s.processTick(id, lp)
	if st, _ := s.effectiveState(id); st.State != "idle" || st.Source != "process" {
		t.Fatalf("6 s after the turn the pane is %s from %s, want idle from process", st.State, st.Source)
	}
	// The grid marks a burst by its writes: the turn was 6 s ago.
	lp.Grid.StampWriteForTest(time.Now().Add(-6 * time.Second))
	if _, err := lp.Grid.Write([]byte("12:01")); err != nil {
		t.Fatal(err)
	}
	s.clockSkew.Store(0)
	s.processTick(id, lp)
	return lp
}

// A gate idle stands against a short redraw of a quiet pane (CP-R-56): a
// clock or a status line ticking on an idle harness is no work. Output
// that goes on for a second more is work, and the process fact applies.
func TestAShortRedrawDoesNotOutrankAStandingGateIdle(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp := quietThenRedraw(t, s, id)
	if st, _ := s.effectiveState(id); st.State != "idle" {
		t.Fatalf("after a short redraw the pane is %s from %s, want idle", st.State, st.Source)
	}
	time.Sleep(processSustain + 100*time.Millisecond)
	if _, err := lp.Grid.Write([]byte("12:02")); err != nil {
		t.Fatal(err)
	}
	s.processTick(id, lp)
	if st, _ := s.effectiveState(id); st.State != "working" || st.Source != "process" {
		t.Fatalf("after output for a second more the pane is %s from %s, want working from process", st.State, st.Source)
	}
}

// A burst starts at a write after a quiet gap, not at a look that saw the
// gap (CP-R-56). On a loaded box no look may fall inside the 7 s between
// two draws, so the old burst start would make the second draw a burst of
// 7 s and let the process working outrank the standing gate idle. The Go
// case is cases/facts/redraw.jsonl.
func TestABurstAfterAGapIsNewEvenIfNoLookSawTheGap(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: 1, SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcGate, Detail: "Stop",
	})
	if _, err := lp.Grid.Write([]byte("turn")); err != nil {
		t.Fatal(err)
	}
	s.processTick(id, lp) // the look that sees the first burst
	// No look during the next 7 s. The second draw lands at +7 s.
	first, _ := lp.Grid.LastWrite()
	lp.Grid.StampWriteForTest(first.Add(7 * time.Second))
	s.clockSkew.Store(int64(7500 * time.Millisecond))
	s.processTick(id, lp)
	if st, _ := s.effectiveState(id); st.State != "idle" || st.Source != "gate" {
		t.Fatalf("after a draw 7 s after the last the pane is %s from %s, want idle from gate", st.State, st.Source)
	}
}

// A hook idle that stands is not replaced by the first look's "no output
// yet" (CP-R-56): whether the hook or the first look came first must not
// decide the source. On a loaded box the tick starts late and the look came
// second (cases/facts/redraw.jsonl:4 read process on Go).
func TestTheFirstLookDoesNotReplaceAStandingHookIdle(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	lp.procMu.Lock()
	lp.procState = "" // no look has run yet
	lp.procMu.Unlock()
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: 1, SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcGate, Detail: "Stop",
	})
	s.processTick(id, lp)
	if st, _ := s.effectiveState(id); st.State != "idle" || st.Source != "gate" {
		t.Fatalf("after the first look the pane is %s from %s, want idle from gate", st.State, st.Source)
	}
}

// The same holds when a draw was held first: the process source published
// nothing, so its later quiet idle replaces no fact and the hook idle stays.
func TestAQuietIdleAfterAHeldDrawDoesNotReplaceAStandingHookIdle(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	lp.procMu.Lock()
	lp.procState = ""
	lp.procMu.Unlock()
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: 1, SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcGate, Detail: "Stop",
	})
	if _, err := lp.Grid.Write([]byte("turn")); err != nil {
		t.Fatal(err)
	}
	s.processTick(id, lp) // the first look sees the draw and holds it
	s.clockSkew.Store(int64(6 * time.Second))
	s.processTick(id, lp) // quiet now: the idle must not replace the hook
	if st, _ := s.effectiveState(id); st.State != "idle" || st.Source != "gate" {
		t.Fatalf("after a held draw and 6 s of quiet the pane is %s from %s, want idle from gate", st.State, st.Source)
	}
}
