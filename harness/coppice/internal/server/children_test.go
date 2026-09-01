package server

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/pane"
)

// A child report lands on the parent's pane.list row and goes out as a
// child event.
func TestAReportedChildShowsOnItsParent(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"events.subscribe","kinds":["child"],"panes":"*"}`)
	next()
	got := roundTrip(t, s,
		`{"id":"2","cmd":"pane.report_child","pane":"w1:p1","child":"a1","state":"working","label":"Explore"}`)
	if !got[0].OK {
		t.Fatalf("pane.report_child: %+v", got[0].Error)
	}
	ev := next()
	if ev["event"] != "child" || ev["pane"] != "w1:p1" || ev["child"] != "a1" ||
		ev["state"] != "working" || ev["label"] != "Explore" {
		t.Fatalf("child event = %v", ev)
	}
	m := result(t, roundTrip(t, s, `{"id":"3","cmd":"pane.list"}`)[0])
	rows, _ := m["panes"].([]any)
	row, _ := rows[0].(map[string]any)
	kids, _ := row["children"].([]any)
	if len(kids) != 1 {
		t.Fatalf("children = %v, want one", row["children"])
	}
	kid, _ := kids[0].(map[string]any)
	if kid["id"] != "a1" || kid["state"] != "working" || kid["label"] != "Explore" {
		t.Fatalf("child row = %v", kid)
	}
	roundTrip(t, s, `{"id":"4","cmd":"pane.report_child","pane":"w1:p1","child":"a1","state":"done"}`)
	m = result(t, roundTrip(t, s, `{"id":"5","cmd":"pane.list"}`)[0])
	rows, _ = m["panes"].([]any)
	row, _ = rows[0].(map[string]any)
	kids, _ = row["children"].([]any)
	kid, _ = kids[0].(map[string]any)
	if kid["state"] != "done" || kid["label"] != "Explore" {
		t.Fatalf("after done, child row = %v", kid)
	}
}

func TestReportChildRefusals(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.report_child","pane":"w9:p9","child":"a1","state":"working"}`,
		`{"id":"2","cmd":"pane.report_child","pane":"w1:p1","state":"working"}`,
		`{"id":"3","cmd":"pane.report_child","pane":"w1:p1","child":"a1","state":"blocked"}`)
	if got[0].OK || got[0].Error.Code != "no_such_pane" {
		t.Fatalf("unknown pane: %+v", got[0])
	}
	if got[1].OK || got[1].Error.Code != "bad_request" {
		t.Fatalf("no child: %+v", got[1])
	}
	if got[2].OK || got[2].Error.Code != "bad_request" {
		t.Fatalf("bad state: %+v", got[2])
	}
}

// A pane connection reports its subagents with no note: the gate hook
// runs on every subagent start and stop.
func TestAPaneReportsChildrenWithNoNote(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p1"})
	defer stop()
	send(`{"id":"1","cmd":"pane.report_child","pane":"w1:p1","child":"a1","state":"working"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("report_child from a pane: %v", r)
	}
	list := result(t, roundTrip(t, s, `{"id":"2","cmd":"floor.notes"}`)[0])
	if notes, _ := list["notes"].([]any); len(notes) != 0 {
		t.Fatalf("a child report left notes: %v", notes)
	}
}

// An adapter's child event reaches the same store as a report.
func TestAnAdapterChildEventIsRecorded(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	s.applyChildEvent("w1:p1", pane.Event{Kind: pane.EvChild, Child: "a2", State: "working", Text: "Plan"})
	kids := s.childrenOf("w1:p1")
	if len(kids) != 1 || kids[0]["id"] != "a2" || kids[0]["label"] != "Plan" {
		t.Fatalf("children = %v", kids)
	}
}
