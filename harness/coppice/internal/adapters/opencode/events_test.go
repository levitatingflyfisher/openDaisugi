package opencode

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
)

const ourSession = "ses_f3107821dffeLaGMxGh2BNticC"

func fixedNow() time.Time { return time.Unix(1000, 0) }

func newTestTranslator() *translator { return newTranslator(ourSession, fixedNow) }

func mustParse(t *testing.T, line string) busEvent {
	t.Helper()
	ev, ok := parseSSELine(line)
	if !ok {
		t.Fatalf("parseSSELine(%q) = not ok", line)
	}
	return ev
}

func one(t *testing.T, tr *translator, line string) pane.Event {
	t.Helper()
	evs := tr.translate(mustParse(t, line))
	if len(evs) != 1 {
		t.Fatalf("translate(%s) = %+v, want one event", line, evs)
	}
	return evs[0]
}

func none(t *testing.T, tr *translator, line string) {
	t.Helper()
	if evs := tr.translate(mustParse(t, line)); len(evs) != 0 {
		t.Fatalf("translate(%s) = %+v, want nothing", line, evs)
	}
}

func TestParseSSELineDecodesADataLine(t *testing.T) {
	ev := mustParse(t, `data: {"id":"evt_1","type":"session.idle","properties":{"sessionID":"s1"}}`)
	if ev.Type != "session.idle" {
		t.Fatalf("got type %q", ev.Type)
	}
}

func TestParseSSELineSkipsBlankCommentAndMalformedLines(t *testing.T) {
	for _, line := range []string{"", ": a comment", "event: x", "data: not json", "data:"} {
		if _, ok := parseSSELine(line); ok {
			t.Fatalf("parseSSELine(%q) = ok, want skipped", line)
		}
	}
}

func TestSessionStatusBusyAndRetryAreWorkingIdleIsIdle(t *testing.T) {
	tr := newTestTranslator()
	for status, want := range map[string]string{
		"busy": pane.StateWorkingStr, "retry": pane.StateWorkingStr, "idle": pane.StateIdleStr,
	} {
		ev := one(t, tr, `data: {"type":"session.status","properties":{"sessionID":"`+ourSession+`","status":{"type":"`+status+`"}}}`)
		if ev.Kind != pane.EvState || ev.State != want {
			t.Fatalf("status %s: got %+v, want state %s", status, ev, want)
		}
	}
	none(t, tr, `data: {"type":"session.status","properties":{"sessionID":"`+ourSession+`","status":{"type":"sleeping"}}}`)
}

func TestSessionErrorIsUnknownWithTheMessage(t *testing.T) {
	ev := one(t, newTestTranslator(), `data: {"type":"session.error","properties":{"sessionID":"`+ourSession+`","error":{"name":"UnknownError","data":{"message":"Model not found"}}}}`)
	if ev.State != pane.StateUnknownStr || !strings.Contains(ev.Detail, "Model not found") {
		t.Fatalf("got %+v", ev)
	}
}

func TestPermissionAskedIsBlockedWithAnAskAndTheReplyClearsIt(t *testing.T) {
	tr := newTestTranslator()
	ev := one(t, tr, `data: {"type":"permission.asked","properties":{"id":"per_1","sessionID":"`+ourSession+`","permission":"bash","patterns":["rm -rf *"],"metadata":{},"always":[]}}`)
	if ev.State != pane.StateBlockedStr || ev.Ask == nil {
		t.Fatalf("got %+v", ev)
	}
	if ev.Ask.ID != "per_1" || ev.Ask.Tool != "bash" || ev.Ask.Summary != "rm -rf *" {
		t.Fatalf("got ask %+v", ev.Ask)
	}
	if ev.Ask.Deadline != 1090 {
		t.Fatalf("deadline %v, want now plus 90 s", ev.Ask.Deadline)
	}
	if perms, _ := tr.open(); strings.Join(perms, ",") != "per_1" {
		t.Fatalf("open permissions %v, want per_1", perms)
	}
	back := one(t, tr, `data: {"type":"permission.replied","properties":{"sessionID":"`+ourSession+`","requestID":"per_1","reply":"once"}}`)
	if back.State != pane.StateWorkingStr {
		t.Fatalf("after the reply got %+v, want working", back)
	}
	if perms, _ := tr.open(); len(perms) != 0 {
		t.Fatalf("open permissions %v after the reply", perms)
	}
}

func TestQuestionAskedIsBlockedAndRejectedClearsIt(t *testing.T) {
	tr := newTestTranslator()
	ev := one(t, tr, `data: {"type":"question.asked","properties":{"id":"que_1","sessionID":"`+ourSession+`","questions":[{"question":"Which branch?","header":"Branch","options":[]}]}}`)
	if ev.State != pane.StateBlockedStr || ev.Ask == nil || ev.Ask.ID != "que_1" || ev.Ask.Tool != "question" || ev.Ask.Summary != "Which branch?" {
		t.Fatalf("got %+v ask %+v", ev, ev.Ask)
	}
	if _, qs := tr.open(); strings.Join(qs, ",") != "que_1" {
		t.Fatalf("open questions %v", qs)
	}
	one(t, tr, `data: {"type":"question.rejected","properties":{"sessionID":"`+ourSession+`","requestID":"que_1"}}`)
	if _, qs := tr.open(); len(qs) != 0 {
		t.Fatalf("open questions %v after the reject", qs)
	}
}

func TestIdleClearsAnOpenAsk(t *testing.T) {
	tr := newTestTranslator()
	one(t, tr, `data: {"type":"permission.asked","properties":{"id":"per_1","sessionID":"`+ourSession+`","permission":"bash","patterns":[],"metadata":{},"always":[]}}`)
	one(t, tr, `data: {"type":"session.idle","properties":{"sessionID":"`+ourSession+`"}}`)
	if perms, qs := tr.open(); len(perms)+len(qs) != 0 {
		t.Fatalf("open (%v, %v) after idle", perms, qs)
	}
}

func TestTextIsEmittedOnceWhenThePartEnds(t *testing.T) {
	tr := newTestTranslator()
	none(t, tr, `data: {"type":"message.part.updated","properties":{"sessionID":"`+ourSession+`","part":{"type":"text","text":"","time":{"start":1}}}}`)
	none(t, tr, `data: {"type":"message.part.delta","properties":{"sessionID":"`+ourSession+`","field":"text","delta":"Hi"}}`)
	ev := one(t, tr, `data: {"type":"message.part.updated","properties":{"sessionID":"`+ourSession+`","part":{"type":"text","text":"Hi!","time":{"start":1,"end":2}}}}`)
	if ev.Kind != pane.EvText || ev.Text != "Hi!" {
		t.Fatalf("got %+v", ev)
	}
}

func TestTheUserEchoAndReasoningAreNotText(t *testing.T) {
	tr := newTestTranslator()
	none(t, tr, `data: {"type":"message.part.updated","properties":{"sessionID":"`+ourSession+`","part":{"type":"text","text":"say hi"}}}`)
	none(t, tr, `data: {"type":"message.part.updated","properties":{"sessionID":"`+ourSession+`","part":{"type":"reasoning","text":"hmm","time":{"start":1,"end":2}}}}`)
}

func TestAToolPartIsOneToolEventWhenItStartsRunning(t *testing.T) {
	tr := newTestTranslator()
	pending := `data: {"type":"message.part.updated","properties":{"sessionID":"` + ourSession + `","part":{"type":"tool","callID":"call_1","tool":"bash","state":{"status":"pending","input":{}}}}}`
	running := `data: {"type":"message.part.updated","properties":{"sessionID":"` + ourSession + `","part":{"type":"tool","callID":"call_1","tool":"bash","state":{"status":"running","input":{"command":"ls"}}}}}`
	none(t, tr, pending)
	ev := one(t, tr, running)
	if ev.Kind != pane.EvTool || ev.Tool != "bash" || !strings.Contains(ev.Detail, "call_1") || !strings.Contains(ev.Detail, `"ls"`) {
		t.Fatalf("got %+v", ev)
	}
	none(t, tr, running)
}

func TestAnotherSessionsEventsAreSkipped(t *testing.T) {
	tr := newTestTranslator()
	none(t, tr, `data: {"type":"session.idle","properties":{"sessionID":"ses_OTHER"}}`)
	none(t, tr, `data: {"type":"permission.asked","properties":{"id":"per_x","sessionID":"ses_OTHER","permission":"bash","patterns":[],"metadata":{},"always":[]}}`)
	none(t, tr, `data: {"type":"session.error","properties":{"error":{"name":"X"}}}`)
	if perms, _ := tr.open(); len(perms) != 0 {
		t.Fatalf("another session's ask became open: %v", perms)
	}
}

func TestServerEventsAreSkipped(t *testing.T) {
	tr := newTestTranslator()
	none(t, tr, `data: {"type":"server.connected","properties":{}}`)
	none(t, tr, `data: {"type":"plugin.added","properties":{"id":"agent"}}`)
}

// The fixture holds a real capture and a constructed tail. PINS.md names
// which lines are which.
func TestFixtureProducesTheExpectedSequence(t *testing.T) {
	f, err := os.Open("testdata/session_lifecycle.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := newTestTranslator()
	var got []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		ev, ok := parseSSELine(sc.Text())
		if !ok {
			continue
		}
		for _, pe := range tr.translate(ev) {
			switch pe.Kind {
			case pane.EvState:
				got = append(got, pe.State)
			case pane.EvText:
				got = append(got, "text:"+pe.Text)
			case pane.EvTool:
				got = append(got, "tool:"+pe.Tool)
			}
		}
	}
	want := []string{
		// the real capture
		"working", "working", "text:Hi!", "working", "idle", "idle",
		// the constructed tail
		"working", "tool:bash", "blocked", "working", "blocked", "working", "unknown", "idle",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

const childSession = "ses_child"

func childCreated(parent string) string {
	return `data: {"type":"session.created","properties":{"sessionID":"` + childSession + `","info":{"id":"` + childSession + `","parentID":"` + parent + `"}}}`
}

func permLine(id, session string) string {
	return `data: {"type":"permission.asked","properties":{"id":"` + id + `","sessionID":"` + session + `","permission":"bash","patterns":["x ` + id + `"],"metadata":{},"always":[]}}`
}

func TestTwoOpenAsksAreKeptByIDAndTheOtherStaysBlocked(t *testing.T) {
	tr := newTestTranslator()
	one(t, tr, permLine("per_1", ourSession))
	one(t, tr, permLine("per_2", ourSession))
	if perms, _ := tr.open(); strings.Join(perms, ",") != "per_1,per_2" {
		t.Fatalf("open = %v", perms)
	}
	ev := one(t, tr, `data: {"type":"permission.replied","properties":{"sessionID":"`+ourSession+`","requestID":"per_2","reply":"reject"}}`)
	if ev.State != pane.StateBlockedStr || ev.Ask == nil || ev.Ask.ID != "per_1" {
		t.Fatalf("after one of two replies got %+v, want blocked on per_1", ev)
	}
	ev = one(t, tr, `data: {"type":"permission.replied","properties":{"sessionID":"`+ourSession+`","requestID":"per_1","reply":"once"}}`)
	if ev.State != pane.StateWorkingStr {
		t.Fatalf("after both replies got %+v, want working", ev)
	}
}

func TestAChildSessionsPermissionIsThePanesAsk(t *testing.T) {
	tr := newTestTranslator()
	none(t, tr, childCreated(ourSession))
	ev := one(t, tr, permLine("per_c", childSession))
	if ev.State != pane.StateBlockedStr || ev.Ask == nil || ev.Ask.ID != "per_c" {
		t.Fatalf("got %+v", ev)
	}
	if !tr.owns("per_c") {
		t.Fatal("the pane must own its child session's ask")
	}
	// A child's idle and status are not the pane's.
	none(t, tr, `data: {"type":"session.idle","properties":{"sessionID":"`+childSession+`"}}`)
	none(t, tr, `data: {"type":"session.status","properties":{"sessionID":"`+childSession+`","status":{"type":"idle"}}}`)
}

func TestAnUnrelatedSessionsChildIsNotOurs(t *testing.T) {
	tr := newTestTranslator()
	none(t, tr, childCreated("ses_OTHER"))
	none(t, tr, permLine("per_x", childSession))
}

func TestAnAskExpiresAtItsDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := newTranslator(ourSession, func() time.Time { return now })
	one(t, tr, permLine("per_1", ourSession))
	if got := tr.expire(); len(got) != 0 {
		t.Fatalf("expired early: %v", got)
	}
	now = now.Add(askDeadline + time.Second)
	got := tr.expire()
	if len(got) != 1 || got[0].id != "per_1" || got[0].kind != "permission" {
		t.Fatalf("expire() = %+v", got)
	}
	if tr.owns("per_1") {
		t.Fatal("an expired ask is still owned")
	}
}
