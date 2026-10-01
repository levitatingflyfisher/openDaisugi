package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// crashedWithDanglingStart builds a log in which turn 0 started a spawn and
// coppice stopped. mutate then changes the log dir, and the chat is opened
// again. It returns the notes of the new chat, the new server's chat and
// its log dir.
func crashedWithDanglingStart(t *testing.T, mutate func(dir string)) ([]string, *foremanChat, string, *Server) {
	t.Helper()
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
	mutate(logDir)
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
	return notes, fc2, logDir, s2
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("file modes do not bind root")
	}
}

func TestAnEndLineThatCannotBeWrittenStillSettlesTheTurnAsUnknown(t *testing.T) {
	skipIfRoot(t)
	notes, fc, _, _ := crashedWithDanglingStart(t, func(dir string) {
		if err := os.Chmod(filepath.Join(dir, foreman.CallsFile), 0o400); err != nil {
			t.Fatal(err)
		}
	})
	if len(notes) != 2 || !strings.Contains(notes[0], "outcome unknown") {
		t.Fatalf("notes = %q", notes)
	}
	if strings.Contains(notes[1], "Say it again") || !strings.Contains(notes[1], "outcome is unknown") {
		t.Fatalf("turn note = %q", notes[1])
	}
	if got, _ := fc.peekUnknown(); len(got) != 1 {
		t.Fatalf("state lines = %q", got)
	}
}

func TestACallLogThatCannotBeReadNeverInvitesARepeat(t *testing.T) {
	skipIfRoot(t)
	var path string
	notes, fc, _, _ := crashedWithDanglingStart(t, func(dir string) {
		path = filepath.Join(dir, foreman.CallsFile)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	})
	if len(notes) != 2 || !strings.Contains(notes[0], "could not be read") {
		t.Fatalf("notes = %q", notes)
	}
	if strings.Contains(notes[1], "Say it again") || !strings.Contains(notes[1], "Whether a tool call ran is unknown") {
		t.Fatalf("turn note = %q", notes[1])
	}
	if got, _ := fc.peekUnknown(); len(got) != 1 || !strings.Contains(got[0], "could not be read") {
		t.Fatalf("state lines = %q", got)
	}
}

func TestATornCallLogIsRepairedAtOpenAndTheDanglingStartStillCounts(t *testing.T) {
	notes, _, logDir, _ := crashedWithDanglingStart(t, func(dir string) {
		f, err := os.OpenFile(filepath.Join(dir, foreman.CallsFile), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(`{"call":"c9.1","phase":"sta`)
		f.Close()
	})
	if len(notes) != 2 || strings.Contains(notes[1], "Say it again") {
		t.Fatalf("notes = %q", notes)
	}
	b, _ := os.ReadFile(filepath.Join(logDir, foreman.CallsFile))
	if !strings.HasSuffix(string(b), "\n") || strings.Contains(string(b), "c9.1") {
		t.Fatalf("the torn tail is still there: %q", b)
	}
}

func TestAKnownActWithOtherWordsIsAnErrorToTheSender(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){"": {okStep("done")}})
	fs.sayAct(t, "build the thing", "act-1")
	waitUntil(t, "the turn", 30*time.Second, fs.turnsDone(1))
	b, _ := json.Marshal(map[string]any{"id": "2", "cmd": "floor.talk", "text": "something else", "act": "act-1"})
	r := roundTrip(t, fs.Server, string(b))[0]
	if r.OK || r.Error == nil || !strings.Contains(r.Error.Message, "act act-1 was used for other words") {
		t.Fatalf("reply = %+v", r)
	}
	if n := userMessages(fs.logMessages(t)); n != 1 {
		t.Fatalf("%d user messages", n)
	}
	// The same words are still a repeat.
	if rep := fs.sayAct(t, "build the thing", "act-1"); rep["already"] != true {
		t.Fatalf("repeat = %v", rep)
	}
}

func TestAnActThatIsNotAStringIsAnError(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){"": {okStep("done")}})
	for _, act := range []string{`5`, `{"a":1}`, `["x"]`, `true`} {
		r := roundTrip(t, fs.Server, `{"id":"1","cmd":"floor.talk","text":"hi","act":`+act+`}`)[0]
		if r.OK || r.Error == nil || !strings.Contains(r.Error.Message, "act must be") {
			t.Fatalf("act %s: reply = %+v", act, r)
		}
	}
	if n := userMessages(fs.logMessages(t)); n != 0 {
		t.Fatalf("%d user messages", n)
	}
	// null is no act.
	if r := roundTrip(t, fs.Server, `{"id":"1","cmd":"floor.talk","text":"hi","act":null}`)[0]; !r.OK {
		t.Fatalf("null act: %+v", r.Error)
	}
}

func TestConcurrentRepeatsOfOneActLogTheWordsOnce(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){"": {okStep("done")}})
	var wg sync.WaitGroup
	var mu sync.Mutex
	already := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := fs.sayAct(t, "build the thing", "act-race")
			if r["already"] == true {
				mu.Lock()
				already++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	waitUntil(t, "the turn", 30*time.Second, fs.turnsDone(1))
	if n := userMessages(fs.logMessages(t)); n != 1 || already != 7 {
		t.Fatalf("%d user messages, %d repeats", n, already)
	}
}

func TestASpawnsStartLineIsOnDiskBeforeTheEffectAndNoEndIsYet(t *testing.T) {
	_, fc, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	var atEffect []foreman.CallRecord
	ts.callFn = func(foreman.ToolRequest) (string, error) {
		atEffect, _ = foreman.ReadCalls(fc.dir)
		return "spawned", nil
	}
	r := toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "spawn", Args: map[string]any{"project": "p", "brief": "b"}})
	if !r.OK {
		t.Fatalf("spawn = %+v", r)
	}
	if len(atEffect) != 1 || atEffect[0].Phase != foreman.CallStart {
		t.Fatalf("at the effect the log held %+v", atEffect)
	}
	if after, _ := foreman.ReadCalls(fc.dir); len(after) != 2 || after[1].Phase != foreman.CallEnd || after[1].Outcome != foreman.CallOK {
		t.Fatalf("after the effect the log holds %+v", after)
	}
}

func TestTwoStandingSocketsInOneSecondMakeDifferentCallIds(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tag := standingTag()
		if seen[tag] {
			t.Fatalf("tag %q repeated", tag)
		}
		seen[tag] = true
	}
}

// spawnThenErrStep makes a spawn call (which fails before any effect, but
// writes its lines) and then fails the turn.
func spawnThenErrStep(msg string) func(TurnInput) (TurnResult, error) {
	return func(in TurnInput) (TurnResult, error) {
		in.Started(os.Getpid())
		conn, err := net.Dial("unix", in.Socket)
		if err != nil {
			return TurnResult{}, err
		}
		defer conn.Close()
		b, _ := json.Marshal(foreman.ToolRequest{Tool: "spawn", Args: map[string]any{"project": "p", "brief": "b"}})
		_, _ = conn.Write(append(b, '\n'))
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		return TurnResult{}, fmt.Errorf("%s", msg)
	}
}

func TestAFailureAfterASpawnRanNeverInvitesARepeat(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){
		"": {spawnThenErrStep("sprig ended with an error: boom")},
	})
	fs.turn(t, 1, "start it")
	var note string
	for _, l := range fs.tail(t, 0) {
		if strings.HasPrefix(l, "note: The foreman's turn failed: ") {
			note = l
		}
	}
	if note == "" || strings.Contains(note, "Say it again") || !strings.Contains(note, "A tool call ran in this turn") {
		t.Fatalf("log = %q", fs.tail(t, 0))
	}
}

func TestAFailureWithNoToolCallStillInvitesARepeat(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){
		"": {errStep("sprig ended with an error: boom")},
	})
	fs.turn(t, 1, "hi")
	found := false
	for _, l := range fs.tail(t, 0) {
		if strings.HasPrefix(l, "note: The foreman's turn failed: ") && strings.HasSuffix(l, "Say it again to try again.") {
			found = true
		}
	}
	if !found {
		t.Fatalf("log = %q", fs.tail(t, 0))
	}
}

func TestTheUnknownStateLinesStayUntilAModelHasSeenThem(t *testing.T) {
	_, fc, _, srv := crashedWithDanglingStart(t, func(string) {})
	fc.mu.Lock()
	m, err := fc.chat.Message(0)
	fc.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		_, stdin, n, err := srv.turnInput(fc, m, "")
		if err != nil || n != 1 || !strings.Contains(string(stdin), "tool outcome unknown: spawn(") {
			t.Fatalf("round %d: n=%d err=%v stdin=%q", round, n, err, stdin)
		}
	}
	fc.dropUnknown(1)
	if _, stdin, n, _ := srv.turnInput(fc, m, ""); n != 0 || strings.Contains(string(stdin), "tool outcome unknown") {
		t.Fatalf("after the drop: n=%d stdin=%q", n, stdin)
	}
}

func countNotes(fc *foremanChat, sub string) int {
	n := 0
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for i := int64(0); i < fc.chat.Count(); i++ {
		if m, _ := fc.chat.Message(i); m.Kind == foreman.KindNote && strings.Contains(m.Text, sub) {
			n++
		}
	}
	return n
}

func TestACarriedSpawnDoesNotHideALaterSpawnWithTheSameId(t *testing.T) {
	var logDir string
	notes, _, logDir, s2 := crashedWithDanglingStart(t, func(dir string) {
		// A broken history: the next open moves it aside and carries the
		// dangling c0.1 into a new chat.
		files, _ := filepath.Glob(filepath.Join(dir, "main", "*.jsonl"))
		f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("not json\n")
		f.Close()
	})
	if len(notes) != 2 || !strings.Contains(notes[1], "outcome unknown") {
		t.Fatalf("notes = %q", notes)
	}
	// The new chat's own turn makes the id the old chat had.
	if err := foreman.AppendCall(logDir, foreman.CallRecord{Call: "c0.1", Phase: foreman.CallStart, Turn: 0, Tool: "spawn", Args: `{"project":"new"}`, At: "2026-10-14T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, _ := chatServer(t, newFakeEnv(t), func(f *ForemanConfig) { f.LogDir = logDir })
	s3.StartForeman()
	fc3 := s3.ensureChat()
	if n := countNotes(fc3, `{"project":"new"}) started at 2026-10-14T10:00:00Z, outcome unknown`); n != 1 {
		t.Fatalf("the new spawn was reported %d times", n)
	}
}

func TestATurnWithAToolStartThatEndedNeverInvitesARepeatAfterARestart(t *testing.T) {
	notes, _, _, _ := crashedWithDanglingStart(t, func(dir string) {
		// The spawn did end (ok), and then coppice died before the turn did.
		if err := foreman.AppendCall(dir, foreman.CallRecord{Call: "c0.1", Phase: foreman.CallEnd, Outcome: foreman.CallOK, At: "2026-10-13T10:00:04Z"}); err != nil {
			t.Fatal(err)
		}
	})
	if len(notes) != 1 || strings.Contains(notes[0], "Say it again") || !strings.Contains(notes[0], "A tool call ran in this turn. Check the workers before you say it again.") {
		t.Fatalf("notes = %q", notes)
	}
}

func TestAnUnreadableCallLogAtTheTurnCheckNeverInvitesARepeat(t *testing.T) {
	skipIfRoot(t)
	_, fc, logDir, s := crashedWithDanglingStart(t, func(string) {})
	appendLog(t, s, foreman.KindUser, "again")
	// The newest line's spawn started and ended ok; the turn did not end.
	for _, r := range []foreman.CallRecord{
		{Call: "c1.1", Phase: foreman.CallStart, Turn: 1, Tool: "spawn", At: "2026-10-13T11:00:00Z"},
		{Call: "c1.1", Phase: foreman.CallEnd, Outcome: foreman.CallOK, At: "2026-10-13T11:00:01Z"},
	} {
		if err := foreman.AppendCall(logDir, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := foreman.AppendTurn(logDir, foreman.TurnRecord{Msg: 1, Started: "2026-10-13T11:00:00Z", Status: foreman.TurnRunning}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, foreman.CallsFile)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	unknown, started, unreadable := s.settleDanglingCalls(fc)
	s.closeDanglingTurns(fc, unknown, started, unreadable)
	if n := countNotes(fc, "Say it again"); n != 0 {
		t.Fatalf("%d notes say Say it again", n)
	}
	if n := countNotes(fc, "did not"); n < 2 {
		t.Fatalf("no note for the unanswered line (%d)", n)
	}
}

// A real 5xx on the sprig wire: the text is on stderr only. The Stderr field
// of the call error is what keeps it transient (FM-R-64).
func TestARealFiveHundredThroughCompactorCallNeverClips(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sprig")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\necho 'sprig: anthropic api 502: bad gateway' >&2\necho 'sprig: giving up' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := chatServer(t, newFakeEnv(t), nil)
	t.Setenv("COPPICE_SPRIG_BIN", script) // after the fake environment sets its own
	m := &foreman.ModelSummarizer{Call: s.compactorCall}
	for i := 0; i < 3; i++ {
		if got, err := m.Compress(context.Background(), foreman.Message{I: 3, Text: "source text"}, 64); err == nil || got != "" {
			t.Fatalf("ask %d: %q, %v", i, got, err)
		}
	}
}
