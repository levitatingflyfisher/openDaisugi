package server

import (
	"fmt"
	"strings"
	"testing"
)

// A note reaches a client that subscribed to notes, and floor.notes keeps it.
func TestANoteRoundTripsToASubscriberAndIsKept(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"events.subscribe","kinds":["note"],"panes":"*"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("subscribe: %v", r)
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"floor.note","text":"docs pane spawned"}`)
	if !got[0].OK {
		t.Fatalf("floor.note: %+v", got[0].Error)
	}
	ev := next()
	if ev["event"] != "note" || ev["text"] != "docs pane spawned" {
		t.Fatalf("note event = %v", ev)
	}
	if _, ok := ev["ts"].(float64); !ok {
		t.Fatalf("note event has no ts: %v", ev)
	}
	list := result(t, roundTrip(t, s, `{"id":"3","cmd":"floor.notes"}`)[0])
	notes, _ := list["notes"].([]any)
	if len(notes) != 1 {
		t.Fatalf("floor.notes = %v, want one note", list)
	}
	if n, _ := notes[0].(map[string]any); n["text"] != "docs pane spawned" {
		t.Fatalf("kept note = %v", notes[0])
	}
}

func TestFloorNoteNeedsText(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"floor.note","text":"  "}`)
	if got[0].OK || got[0].Error.Code != "bad_request" {
		t.Fatalf("an empty note answered %+v, want bad_request", got[0])
	}
}

// floor.notes keeps the last 200 notes, oldest first.
func TestFloorNotesKeepsTheLast200(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 205; i++ {
		s.Note(fmt.Sprintf("n%d", i), "")
	}
	list := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.notes"}`)[0])
	notes, _ := list["notes"].([]any)
	if len(notes) != 200 {
		t.Fatalf("%d notes kept, want 200", len(notes))
	}
	first, _ := notes[0].(map[string]any)
	last, _ := notes[199].(map[string]any)
	if first["text"] != "n5" || last["text"] != "n204" {
		t.Fatalf("kept %v to %v, want n5 to n204", first["text"], last["text"])
	}
}

// The note text for a verb is the verb, then its label, pane, harness and
// text values, two spaces apart, with the text cut at 60 runes.
func TestVerbNoteText(t *testing.T) {
	long := strings.Repeat("x", 70)
	got := verbNote("floor", "agent.prompt", map[string]string{"pane": "w1:p2", "text": long})
	want := "floor › agent.prompt  w1:p2  " + strings.Repeat("x", 60)
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	got = verbNote("floor", "pane.create", map[string]string{"label": "docs", "harness": "pi"})
	if got != "floor › pane.create  docs  pi" {
		t.Fatalf("got %q", got)
	}
}

// A note is cut at 200 runes and loses every control rune, so a pane can
// neither fill the server nor write escape codes to the operator's screen.
func TestANoteIsCutAndCleaned(t *testing.T) {
	s := newTestServer(t)
	s.Note("a\x1b]52;c;aGk=\x07b\x9bc\u009b\x7fd\tе"+strings.Repeat("x", 300), "")
	list := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.notes"}`)[0])
	notes, _ := list["notes"].([]any)
	n, _ := notes[0].(map[string]any)
	text, _ := n["text"].(string)
	if strings.ContainsAny(text, "\x1b\x07\x7f\t\u009b") {
		t.Fatalf("control runes kept: %q", text)
	}
	if !strings.HasPrefix(text, "a]52;c;aGk=bcdе") {
		t.Fatalf("text = %q", text[:20])
	}
	if got := len([]rune(text)); got != 200 {
		t.Fatalf("%d runes kept, want 200", got)
	}
}

func TestAChildLabelIsCutAndCleaned(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	s.ReportChild("w1:p1", "a1", "working", "Ex\x1b[2Jplore"+strings.Repeat("y", 300))
	kids := s.childrenOf("w1:p1")
	label, _ := kids[0]["label"].(string)
	if strings.Contains(label, "\x1b") || !strings.HasPrefix(label, "Ex[2Jplore") || len([]rune(label)) != 200 {
		t.Fatalf("label = %q", label)
	}
}
