package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// sayAct is floor.talk with a client act id.
func (ts *turnServer) sayAct(t *testing.T, text, act string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "floor.talk", "text": text, "act": act})
	return result(t, roundTrip(t, ts.Server, string(b))[0])
}

func userMessages(msgs []foreman.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == foreman.KindUser {
			n++
		}
	}
	return n
}

func TestARepeatedActIdQueuesNoSecondTurnAndSurvivesARestart(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){"": {okStep("done")}})
	first := fs.sayAct(t, "build the thing", "act-1")
	if first["message"] != "log/0" || first["already"] != nil {
		t.Fatalf("first = %v", first)
	}
	waitUntil(t, "the turn", 30*time.Second, fs.turnsDone(1))
	again := fs.sayAct(t, "build the thing", "act-1")
	if again["message"] != "log/0" || again["already"] != true || again["queued"] != float64(0) {
		t.Fatalf("again = %v", again)
	}
	if n := userMessages(fs.logMessages(t)); n != 1 {
		t.Fatalf("%d user messages", n)
	}
	// Another id is another sentence.
	other := fs.sayAct(t, "and another", "act-2")
	if other["already"] != nil {
		t.Fatalf("other = %v", other)
	}
	waitUntil(t, "the second turn", 30*time.Second, fs.turnsDone(2))

	// After a restart the set is rebuilt from the log.
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := chatServer(t, fs.e, func(f *ForemanConfig) { f.LogDir = fs.logDir })
	s2.StartForeman()
	ts2 := &turnServer{Server: s2, e: fs.e, logDir: fs.logDir}
	rep := ts2.sayAct(t, "build the thing", "act-1")
	if rep["message"] != "log/0" || rep["already"] != true {
		t.Fatalf("after a restart: %v", rep)
	}
	if n := userMessages(ts2.logMessages(t)); n != 2 {
		t.Fatalf("%d user messages after the restart", n)
	}
	if recs, _ := foreman.ReadTurns(fs.logDir); len(recs) != 4 {
		t.Fatalf("turn records = %+v", recs)
	}
}

func TestATalkWithNoActBehavesAsBefore(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){"": {okStep("done")}})
	fs.say(t, "same words")
	fs.say(t, "same words")
	waitUntil(t, "two turns", 30*time.Second, fs.turnsDone(2))
	if n := userMessages(fs.logMessages(t)); n != 2 {
		t.Fatalf("%d user messages", n)
	}
}

func TestABadActIdIsRefused(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){"": {okStep("done")}})
	for _, act := range []string{strings.Repeat("a", 65), "has space", "new\nline", "q\"uote"} {
		b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "floor.talk", "text": "hi", "act": act})
		raw := roundTrip(t, fs.Server, string(b))[0]
		if raw.OK || raw.Error == nil || raw.Error.Code != "bad_request" || !strings.Contains(raw.Error.Message, "act") {
			t.Fatalf("act %q: %+v", act, raw)
		}
	}
	if n := userMessages(fs.logMessages(t)); n != 0 {
		t.Fatalf("%d user messages", n)
	}
}

func TestASpawnWritesItsStartLineBeforeTheEffectAndItsEndAfter(t *testing.T) {
	_, fc, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	r := toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "spawn", Args: map[string]any{"project": "p"}})
	if r.OK {
		t.Fatalf("spawn = %+v", r)
	}
	recs, err := foreman.ReadCalls(fc.dir)
	if err != nil || len(recs) != 2 {
		t.Fatalf("calls = %+v, %v", recs, err)
	}
	st, en := recs[0], recs[1]
	if st.Phase != foreman.CallStart || st.Tool != "spawn" || st.Turn != 1 || !strings.Contains(st.Args, `"project":"p"`) || st.Call == "" {
		t.Fatalf("start = %+v", st)
	}
	if en.Phase != foreman.CallEnd || en.Call != st.Call || en.Outcome != foreman.CallFailed {
		t.Fatalf("end = %+v", en)
	}
	// The read tools write no call line.
	toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "status"})
	toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"id": 0, "n": 1}})
	if recs, _ := foreman.ReadCalls(fc.dir); len(recs) != 2 {
		t.Fatalf("a read tool wrote a call line: %+v", recs)
	}
}

func TestASpawnWhoseStartLineCannotBeWrittenHasNoEffect(t *testing.T) {
	_, fc, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	if err := os.Mkdir(filepath.Join(fc.dir, foreman.CallsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	r := toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "spawn", Args: map[string]any{"project": "p", "brief": "b"}})
	if r.OK || !strings.Contains(r.Error, "not started") {
		t.Fatalf("spawn = %+v", r)
	}
}

func TestADanglingStartBecomesOneNoteAndNoInviteToRepeat(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	s.StartForeman()
	fc := s.ensureChat()
	appendLog(t, s, foreman.KindUser, "start the worker")
	if err := foreman.AppendTurn(fc.dir, foreman.TurnRecord{Msg: 0, Started: "2026-10-13T10:00:00Z", Status: foreman.TurnRunning}); err != nil {
		t.Fatal(err)
	}
	if err := foreman.AppendCall(fc.dir, foreman.CallRecord{Call: "c0.1", Phase: foreman.CallStart, Turn: 0, Tool: "spawn",
		Args: `{"project":"web","brief":"fix it"}`, At: "2026-10-13T10:00:03Z"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := chatServer(t, e, func(f *ForemanConfig) { f.LogDir = logDir })
	s2.StartForeman()
	fc2 := s2.ensureChat()
	var notes []string
	fc2.mu.Lock()
	for i := int64(0); i < fc2.chat.Count(); i++ {
		if m, _ := fc2.chat.Message(i); m.Kind == foreman.KindNote {
			notes = append(notes, m.Text)
		}
	}
	fc2.mu.Unlock()
	if len(notes) != 2 {
		t.Fatalf("notes = %q", notes)
	}
	if !strings.HasPrefix(notes[0], `spawn({"project":"web","brief":"fix it"}) started at 2026-10-13T10:00:03Z, outcome unknown.`) {
		t.Fatalf("tool note = %q", notes[0])
	}
	if !strings.Contains(notes[1], "log/0") || strings.Contains(notes[1], "Say it again") || !strings.Contains(notes[1], "outcome is unknown") {
		t.Fatalf("turn note = %q", notes[1])
	}
	// The call is settled: it is never reported or replayed again, and the
	// state of the next turn carries one line.
	recs, _ := foreman.ReadCalls(logDir)
	if d := foreman.DanglingCalls(recs); len(d) != 0 {
		t.Fatalf("still dangling: %+v", d)
	}
	got, nUnk := fc2.peekUnknown()
	if len(got) != 1 || nUnk != 1 || !strings.Contains(got[0], "spawn(") || !strings.Contains(got[0], "outcome unknown") {
		t.Fatalf("state lines = %q", got)
	}
	// They stay until a model has seen them.
	if again, _ := fc2.peekUnknown(); len(again) != 1 {
		t.Fatalf("a look used the state lines up: %q", again)
	}
	fc2.dropUnknown(nUnk)
	if again, _ := fc2.peekUnknown(); len(again) != 0 {
		t.Fatalf("the state lines came twice: %q", again)
	}
	// Nothing is started at open, and a second restart says nothing more.
	if after, _ := foreman.ReadTurns(logDir); len(after) != 2 {
		t.Fatalf("turn records = %+v", after)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, _ := chatServer(t, e, func(f *ForemanConfig) { f.LogDir = logDir })
	s3.StartForeman()
	fc3 := s3.ensureChat()
	fc3.mu.Lock()
	n := fc3.chat.Count()
	fc3.mu.Unlock()
	if int(n) != 1+len(notes) {
		t.Fatalf("a second restart added messages: %d", n)
	}
}

// This tests the guard on its own (defense in depth, FM-R-50): the flow does
// not reach a state with a start line and an empty count.
func TestTheLogIsReadAsASecondGuardAgainstAReplayOnAnotherAccount(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("sprig ended with an error: anthropic api 429: slow down")},
		"desk": {okStep("never")},
	})
	// A start line of this turn (message 0) is in the log: the memory of the
	// process holds no tool request, as after a crash.
	if err := foreman.AppendCall(fs.logDir, foreman.CallRecord{Call: "c0.1", Phase: foreman.CallStart, Turn: 0, Tool: "spawn", At: "2026-10-13T10:00:03Z"}); err != nil {
		t.Fatal(err)
	}
	fs.turn(t, 1, "hi")
	if got := fs.r.ran(); got != "" {
		t.Fatalf("ran on %q", got)
	}
	var note string
	for _, l := range fs.tail(t, 0) {
		if strings.HasPrefix(l, "note: The foreman's turn failed: ") {
			note = l
		}
	}
	if !strings.Contains(note, "after it had used tools") {
		t.Fatalf("log = %q", fs.tail(t, 0))
	}
}
