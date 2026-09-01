package tui

import (
	"strings"
	"testing"
)

// trustDetail is the detail the server puts on a pane that shows Claude's
// folder trust screen.
const trustDetail = "asks to trust this folder (rule=coppice_first_run_trust region=whole_recent)"

// A row on the trust screen says so in words, not the generic question
// line.
func TestATrustRowSaysItAsksToTrustTheFolder(t *testing.T) {
	rows := RowsFrom([]map[string]any{{
		"id": "w1:p1", "label": "claude-new", "state": "blocked", "source": "manifest", "detail": trustDetail,
	}}, 0)
	if !rows[0].Trust || rows[0].Line != TrustLine {
		t.Fatalf("row = %+v, want Trust and the trust line", rows[0])
	}
}

// trustFloor is a floor with one pane on the trust screen and a peek open
// on it.
func trustFloor(t *testing.T, sock *answerSocket) *floor {
	t.Helper()
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "claude-new", State: "blocked", Trust: true}}}
	m.OpenPeek("screen text", nil)
	f := testFloor(m, 100)
	f.o.Socket = sock.path
	return f
}

// The peek on the trust screen offers both answers, and y sends
// pane.trust.
func TestThePeekTrustsTheFolderWithY(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := trustFloor(t, sock)
	lines := strings.Join(peekLines(f.m.Peek, 100, 10), "\n")
	if !strings.Contains(lines, TrustKeys) {
		t.Fatalf("the peek does not offer the answers:\n%s", lines)
	}
	press(t, f, "y")
	reqs := sock.requests()
	if len(reqs) != 1 || reqs[0]["cmd"] != "pane.trust" || reqs[0]["pane"] != "w1:p1" || reqs[0]["trust"] != true {
		t.Fatalf("requests = %v, want one pane.trust with trust true", reqs)
	}
	if f.m.Peek != nil {
		t.Fatal("the peek stayed open after the answer")
	}
}

// n is not now: pane.trust with trust false.
func TestThePeekSaysNotNowWithN(t *testing.T) {
	sock := newAnswerSocket(t, never)
	f := trustFloor(t, sock)
	press(t, f, "n")
	reqs := sock.requests()
	if len(reqs) != 1 || reqs[0]["cmd"] != "pane.trust" || reqs[0]["trust"] != false {
		t.Fatalf("requests = %v, want one pane.trust with trust false", reqs)
	}
}
