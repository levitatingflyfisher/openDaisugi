package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
