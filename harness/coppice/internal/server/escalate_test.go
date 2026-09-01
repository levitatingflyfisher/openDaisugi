package server

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// team is a team task with a foreman pane, and a child task under it with
// one worker pane. The team names the foreman.
type team struct {
	s                          *Server
	team, child, foreman, work string
}

// newTeam builds the team on an agent server whose holds last holdFor.
func newTeam(t *testing.T, holdFor time.Duration) team {
	t.Helper()
	s := newAgentServer(t)
	s.holdFor = holdFor
	tm := team{s: s}
	tm.team, _ = createTask(t, s, `"label":"review-team","cwd":"/"`)
	tm.child, _ = createTask(t, s, `"label":"docs","parent":"`+tm.team+`","cwd":"/"`)
	tm.foreman = paneIn(t, s, tm.team, "lead")
	tm.work = paneIn(t, s, tm.child, "writer")
	got := roundTrip(t, s, `{"id":"1","cmd":"task.set_foreman","task":"`+tm.team+`","pane":"`+tm.foreman+`"}`)
	if !got[0].OK {
		t.Fatalf("task.set_foreman: %+v", got[0].Error)
	}
	return tm
}

// paneIn opens one sleeping pane in task and returns its id.
func paneIn(t *testing.T, s *Server, task, label string) string {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","task":"`+task+`","label":"`+label+`",`+
		`"cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	id, _ := result(t, got[0])["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create: %+v", got[0])
	}
	return id
}

// row is the pane.list row of pane id.
func row(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.list"}`)
	rows, _ := result(t, got[0])["panes"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("no row for %s", id)
	return nil
}

// notesFor is the text of every kept note addressed to pane id.
func notesFor(t *testing.T, s *Server, id string) []string {
	t.Helper()
	list := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.notes"}`)[0])
	raw, _ := list["notes"].([]any)
	var out []string
	for _, n := range raw {
		m, _ := n.(map[string]any)
		if m["to"] == id {
			text, _ := m["text"].(string)
			out = append(out, text)
		}
	}
	return out
}

// report sends a gate block on pane with ask id and tier.
func (tm team) report(t *testing.T, pane, ask, tier string) {
	t.Helper()
	plantTieredAsk(t, tm.s.cfg.GateRoot, ask, tier)
	got := roundTrip(t, tm.s, tieredBlockedLine(pane, ask, tier))
	if !got[0].OK {
		t.Fatalf("report_state: %+v", got[0].Error)
	}
}

// waitUnheld waits until the row of pane has no hold.
func waitUnheld(t *testing.T, s *Server, pane string, within time.Duration) map[string]any {
	t.Helper()
	end := time.Now().Add(within)
	for {
		r := row(t, s, pane)
		if _, held := r["held"]; !held {
			return r
		}
		if time.Now().After(end) {
			t.Fatalf("the hold on %s did not end in %v: %v", pane, within, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAnUndoableAskGoesToTheForemanFirstAndSurfacesAtTheDeadline(t *testing.T) {
	tm := newTeam(t, 400*time.Millisecond)
	send, next, stop := stream(t, tm.s)
	defer stop()
	send(`{"id":"1","cmd":"events.subscribe","kinds":["state"],"panes":["` + tm.work + `"]}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("subscribe: %v", r)
	}
	tm.report(t, tm.work, "ask-1", "undoable")

	ev := next()
	held, _ := ev["held"].(map[string]any)
	if ev["state"] != "blocked" || held == nil || held["by"] != tm.foreman || held["task"] != tm.team {
		t.Fatalf("the blocked event = %v, want it held by %s", ev, tm.foreman)
	}
	r := row(t, tm.s, tm.work)
	h, _ := r["held"].(map[string]any)
	if r["state"] != "blocked" || h == nil || h["by"] != tm.foreman || h["task_label"] != "review-team" {
		t.Fatalf("the held row = %v", r)
	}
	notes := notesFor(t, tm.s, tm.foreman)
	if len(notes) != 1 || !strings.Contains(notes[0], "ask-1") || !strings.Contains(notes[0], "rm -rf build/") ||
		!strings.Contains(notes[0], "coppice agent deny "+tm.work+" ask-1") {
		t.Fatalf("foreman notes = %q", notes)
	}

	// At the deadline the hold ends, and the same ask goes out again with
	// no hold, so every floor moves it to NEEDS YOU.
	ev = next()
	if _, still := ev["held"]; still || ev["state"] != "blocked" {
		t.Fatalf("the surfacing event = %v", ev)
	}
	r = waitUnheld(t, tm.s, tm.work, time.Second)
	if r["state"] != "blocked" {
		t.Fatalf("after the deadline the row = %v", r)
	}

	// The same ask reported again stays with the operator.
	tm.report(t, tm.work, "ask-1", "undoable")
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("an ask that surfaced was held a second time")
	}
}

func TestAPermanentAskSurfacesAtOnceAndTheForemanStillHears(t *testing.T) {
	tm := newTeam(t, time.Minute)
	tm.report(t, tm.work, "ask-2", "permanent")
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("a permanent ask was held")
	}
	if notes := notesFor(t, tm.s, tm.foreman); len(notes) != 1 || !strings.Contains(notes[0], "ask-2") {
		t.Fatalf("foreman notes = %q", notes)
	}
	// An ask with no tier is permanent.
	tm.report(t, tm.work, "ask-3", "")
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("an ask with no tier was held")
	}
}

func TestNoHoldWithoutAForemanThatCanAnswer(t *testing.T) {
	// No foreman on the chain.
	s := newAgentServer(t)
	s.holdFor = time.Minute
	task, _ := createTask(t, s, `"label":"solo","cwd":"/"`)
	lone := paneIn(t, s, task, "solo")
	plantTieredAsk(t, s.cfg.GateRoot, "ask-1", "undoable")
	roundTrip(t, s, tieredBlockedLine(lone, "ask-1", "undoable"))
	if _, held := row(t, s, lone)["held"]; held {
		t.Fatal("an ask with no foreman was held")
	}

	// A blocked foreman.
	tm := newTeam(t, time.Minute)
	tm.report(t, tm.foreman, "ask-f", "undoable")
	tm.report(t, tm.work, "ask-1", "undoable")
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("an ask was held by a blocked foreman")
	}

	// A closed foreman.
	tm = newTeam(t, time.Minute)
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.close","pane":"`+tm.foreman+`"}`)
	tm.report(t, tm.work, "ask-1", "undoable")
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("an ask was held by a closed foreman")
	}

	// The foreman's own ask.
	tm = newTeam(t, time.Minute)
	tm.report(t, tm.foreman, "ask-1", "undoable")
	if _, held := row(t, tm.s, tm.foreman)["held"]; held {
		t.Fatal("the foreman held its own ask")
	}
}

// The foreman is the nearest one up the chain: a child task's own foreman
// wins over its parent's.
func TestTheNearestForemanHolds(t *testing.T) {
	tm := newTeam(t, time.Minute)
	near := paneIn(t, tm.s, tm.child, "near")
	got := roundTrip(t, tm.s, `{"id":"1","cmd":"task.set_foreman","task":"`+tm.child+`","pane":"`+near+`"}`)
	if !got[0].OK {
		t.Fatalf("task.set_foreman: %+v", got[0].Error)
	}
	tm.report(t, tm.work, "ask-1", "undoable")
	h, _ := row(t, tm.s, tm.work)["held"].(map[string]any)
	if h == nil || h["by"] != near || h["task"] != tm.child {
		t.Fatalf("held = %v, want it held by %s", h, near)
	}
}

// The foreman may deny the ask it holds, and nothing else. It never
// allows.
func TestTheForemanMayDenyOnlyTheAskItHolds(t *testing.T) {
	tm := newTeam(t, time.Minute)
	other := paneIn(t, tm.s, tm.child, "other")
	tm.report(t, tm.work, "ask-1", "undoable")
	tm.report(t, other, "ask-9", "permanent")
	asForeman := &peerFacts{checked: true, pane: true, paneID: tm.foreman}
	asWorker := &peerFacts{checked: true, pane: true, paneID: tm.work}

	got := roundTripFacts(t, tm.s, asForeman,
		`{"id":"1","cmd":"agent.allow","pane":"`+tm.work+`","ask":"ask-1"}`,
		`{"id":"2","cmd":"agent.deny","pane":"`+other+`","ask":"ask-9"}`)
	for i, r := range got {
		if r.OK || r.Error == nil || r.Error.Code != "unauthorized" {
			t.Fatalf("call %d from the foreman = %+v, want unauthorized", i, r)
		}
	}
	got = roundTripFacts(t, tm.s, asWorker,
		`{"id":"1","cmd":"agent.deny","pane":"`+tm.work+`","ask":"ask-1"}`)
	if got[0].OK {
		t.Fatal("the asking pane denied its own ask")
	}
	if _, ok := answerFile(t, tm.s.cfg.GateRoot, "ask-1"); ok {
		t.Fatal("a refused call wrote an answer")
	}

	got = roundTripFacts(t, tm.s, asForeman,
		`{"id":"1","cmd":"agent.deny","pane":"`+tm.work+`","ask":"ask-1"}`)
	if !got[0].OK {
		t.Fatalf("the foreman's deny = %+v", got[0].Error)
	}
	ans, ok := answerFile(t, tm.s.cfg.GateRoot, "ask-1")
	if !ok || ans["decision"] != "deny" || !strings.Contains(ans["reason"].(string), "foreman") {
		t.Fatalf("answer = %v", ans)
	}
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("the hold stayed after the foreman denied")
	}
}

// Only the operator names a foreman. A pane that could name itself would
// keep other panes' asks from the operator.
func TestOnlyTheOperatorNamesAForeman(t *testing.T) {
	tm := newTeam(t, time.Minute)
	for _, f := range []*peerFacts{
		{checked: true, pane: true, paneID: tm.work},
		{checked: true, unknown: true},
		{checked: true, plugin: "p"},
	} {
		got := roundTripFacts(t, tm.s, f,
			`{"id":"1","cmd":"task.set_foreman","task":"`+tm.child+`","pane":"`+tm.work+`"}`)
		if got[0].OK || got[0].Error == nil || got[0].Error.Code != "unauthorized" {
			t.Fatalf("set_foreman from %+v = %+v, want unauthorized", f, got[0])
		}
	}
	rows := listTasks(t, tm.s)
	if rows[tm.child]["foreman"] != "" || rows[tm.team]["foreman"] != tm.foreman {
		t.Fatalf("task rows = %v", rows)
	}
}

func TestSetForemanRefusesAPaneOrTaskThatIsNotThere(t *testing.T) {
	tm := newTeam(t, time.Minute)
	for _, line := range []string{
		`{"id":"1","cmd":"task.set_foreman","task":"t99","pane":"` + tm.foreman + `"}`,
		`{"id":"1","cmd":"task.set_foreman","task":"` + tm.team + `","pane":"w9:p9"}`,
		`{"id":"1","cmd":"task.set_foreman","pane":"` + tm.foreman + `"}`,
	} {
		if got := roundTrip(t, tm.s, line); got[0].OK {
			t.Fatalf("%s answered ok", line)
		}
	}
	got := roundTrip(t, tm.s, `{"id":"1","cmd":"task.set_foreman","task":"`+tm.team+`","pane":""}`)
	if !got[0].OK || listTasks(t, tm.s)[tm.team]["foreman"] != "" {
		t.Fatalf("clearing the foreman = %+v", got[0])
	}
}

// A hold ends when the pane closes, so a closed pane's ask never comes
// back later.
func TestAHoldEndsWhenThePaneCloses(t *testing.T) {
	tm := newTeam(t, time.Minute)
	tm.report(t, tm.work, "ask-1", "undoable")
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.close","pane":"`+tm.work+`"}`)
	tm.s.holdsMu.Lock()
	n := len(tm.s.holds)
	tm.s.holdsMu.Unlock()
	if n != 0 {
		t.Fatalf("%d holds after the pane closed", n)
	}
}

// waitStateEvent reads events until one for pane arrives, and returns it.
func waitStateEvent(t *testing.T, next func() map[string]any, pane string) map[string]any {
	t.Helper()
	for i := 0; i < 20; i++ {
		ev := next()
		if ev["event"] == "state" && ev["pane"] == pane {
			return ev
		}
	}
	t.Fatalf("no state event for %s", pane)
	return nil
}

// A foreman that closes or blocks while it holds an ask cannot answer it,
// so the ask goes to the operator at once, with a state event that has no
// hold.
func TestAForemanThatClosesOrBlocksReleasesWhatItHolds(t *testing.T) {
	for _, how := range []string{"close", "block"} {
		t.Run(how, func(t *testing.T) {
			tm := newTeam(t, time.Minute)
			send, next, stop := stream(t, tm.s)
			defer stop()
			send(`{"id":"1","cmd":"events.subscribe","kinds":["state"],"panes":["` + tm.work + `"]}`)
			if r := next(); r["ok"] != true {
				t.Fatalf("subscribe: %v", r)
			}
			tm.report(t, tm.work, "ask-1", "undoable")
			if ev := waitStateEvent(t, next, tm.work); ev["held"] == nil {
				t.Fatalf("the ask was not held: %v", ev)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if how == "close" {
					roundTrip(t, tm.s, `{"id":"1","cmd":"pane.close","pane":"`+tm.foreman+`"}`)
				} else {
					plantTieredAsk(t, tm.s.cfg.GateRoot, "ask-f", "undoable")
					roundTrip(t, tm.s, tieredBlockedLine(tm.foreman, "ask-f", "undoable"))
				}
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the foreman's " + how + " did not return")
			}
			ev := waitStateEvent(t, next, tm.work)
			if _, still := ev["held"]; still || ev["state"] != "blocked" {
				t.Fatalf("the release event = %v", ev)
			}
			if _, held := row(t, tm.s, tm.work)["held"]; held {
				t.Fatal("the row kept its hold")
			}
		})
	}
}

// A team whose only blocked pane is held works, and does not need the
// operator, in task.list.
func TestAHeldAskFoldsIntoTheTaskStateAsWorking(t *testing.T) {
	tm := newTeam(t, time.Minute)
	tm.report(t, tm.work, "ask-1", "undoable")
	rows := listTasks(t, tm.s)
	if rows[tm.child]["state"] != "working" || rows[tm.team]["state"] != "working" {
		t.Fatalf("task states = %v, %v", rows[tm.child]["state"], rows[tm.team]["state"])
	}
}

// blockedLineAt reports a gate block on pane with ask id, tier undoable,
// and the given deadline.
func blockedLineAt(pane, ask string, deadline float64) string {
	return strings.Replace(tieredBlockedLine(pane, ask, "undoable"),
		`"deadline":9999999999.0`, fmt.Sprintf(`"deadline":%.1f`, deadline), 1)
}

// A hold note names the asking pane as its author and the foreman as the
// pane it is for, so a floor never files the child's ask as the foreman's
// own work.
func TestAHoldNoteComesFromTheChildAndGoesToTheForeman(t *testing.T) {
	tm := newTeam(t, time.Minute)
	tm.report(t, tm.work, "ask-1", "undoable")
	list := result(t, roundTrip(t, tm.s, `{"id":"1","cmd":"floor.notes"}`)[0])
	raw, _ := list["notes"].([]any)
	found := false
	for _, n := range raw {
		m, _ := n.(map[string]any)
		if m["to"] == tm.foreman {
			found = true
			if m["pane"] != tm.work {
				t.Fatalf("hold note = %v, want pane %s", m, tm.work)
			}
		}
	}
	if !found {
		t.Fatalf("no note for the foreman: %v", raw)
	}
}

// The hold ends 30 s before the gate stops waiting. A gate that waits
// too little for that gets no hold, and the foreman's note says why.
func TestAHoldEndsBeforeTheGateStopsWaiting(t *testing.T) {
	tm := newTeam(t, time.Minute)
	now := nowSeconds()
	plantTieredAsk(t, tm.s.cfg.GateRoot, "ask-1", "undoable")
	roundTrip(t, tm.s, blockedLineAt(tm.work, "ask-1", now+40))
	h, _ := row(t, tm.s, tm.work)["held"].(map[string]any)
	until, _ := h["until"].(float64)
	if h == nil || until < now+8 || until > now+12 {
		t.Fatalf("held = %v, want until near now plus 10 s", h)
	}

	tm = newTeam(t, time.Minute)
	now = nowSeconds()
	plantTieredAsk(t, tm.s.cfg.GateRoot, "ask-2", "undoable")
	roundTrip(t, tm.s, blockedLineAt(tm.work, "ask-2", now+20))
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("an ask the gate waits 20 s on was held")
	}
	notes := notesFor(t, tm.s, tm.foreman)
	if len(notes) != 1 || !strings.Contains(notes[0], "waits too little") {
		t.Fatalf("foreman notes = %q", notes)
	}
}

// A Foreman that names a pane the tree does not have holds nothing.
func TestNoHoldWhenTheForemanPaneIsGone(t *testing.T) {
	tm := newTeam(t, time.Minute)
	if err := tm.s.tree.SetForeman(tm.team, "w9:p9"); err != nil {
		t.Fatal(err)
	}
	tm.report(t, tm.work, "ask-1", "undoable")
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("a missing foreman held an ask")
	}
}

// Clearing or replacing a task's foreman sends every ask the old one
// held to the operator at once.
func TestChangingTheForemanReleasesTheOldHolds(t *testing.T) {
	for _, next := range []string{"clear", "replace"} {
		t.Run(next, func(t *testing.T) {
			tm := newTeam(t, time.Minute)
			tm.report(t, tm.work, "ask-1", "undoable")
			pane := ""
			if next == "replace" {
				pane = paneIn(t, tm.s, tm.team, "lead2")
			}
			got := roundTrip(t, tm.s, `{"id":"1","cmd":"task.set_foreman","task":"`+tm.team+`","pane":"`+pane+`"}`)
			if !got[0].OK {
				t.Fatalf("task.set_foreman: %+v", got[0].Error)
			}
			if _, held := row(t, tm.s, tm.work)["held"]; held {
				t.Fatal("the old foreman kept its hold")
			}
		})
	}
}

// A move that changes a task's nearest foreman releases its holds.
func TestAMoveThatChangesTheForemanReleasesTheHold(t *testing.T) {
	tm := newTeam(t, time.Minute)
	tm.report(t, tm.work, "ask-1", "undoable")
	got := roundTrip(t, tm.s, `{"id":"1","cmd":"task.move","task":"`+tm.child+`","parent":""}`)
	if !got[0].OK {
		t.Fatalf("task.move: %+v", got[0].Error)
	}
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("the hold stayed after the move")
	}
}

// A pane may not move a task: a move can put a task under a foreman the
// operator did not choose.
func TestTaskMoveFromAPaneIsRefused(t *testing.T) {
	tm := newTeam(t, time.Minute)
	for _, f := range []*peerFacts{
		{checked: true, pane: true, paneID: tm.work},
		{checked: true, unknown: true},
	} {
		got := roundTripFacts(t, tm.s, f, `{"id":"1","cmd":"task.move","task":"`+tm.child+`","parent":""}`)
		if got[0].OK || got[0].Error == nil || got[0].Error.Code != "unauthorized" || got[0].Error.Message != PaneRefusal {
			t.Fatalf("task.move from %+v = %+v", f, got[0])
		}
	}
	if listTasks(t, tm.s)[tm.child]["parent"] != tm.team {
		t.Fatal("a refused move moved the task")
	}
}

// A foreman pane that closes leaves its task, so the task never names a
// closed pane.
func TestAClosedForemanLeavesItsTask(t *testing.T) {
	tm := newTeam(t, time.Minute)
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.close","pane":"`+tm.foreman+`"}`)
	if f := listTasks(t, tm.s)[tm.team]["foreman"]; f != "" {
		t.Fatalf("task foreman = %v after it closed", f)
	}
}

// catTeam is a team whose foreman runs cat, so each byte typed into it
// shows on its screen.
func catTeam(t *testing.T) team {
	t.Helper()
	s := newAgentServer(t)
	s.holdFor = time.Minute
	tm := team{s: s}
	tm.team, _ = createTask(t, s, `"label":"review-team","cwd":"/"`)
	tm.child, _ = createTask(t, s, `"label":"docs","parent":"`+tm.team+`","cwd":"/"`)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","task":"`+tm.team+`","label":"lead",`+
		`"cmd_argv":["cat"],"kind":"pty","cols":400,"rows":20}`)
	tm.foreman, _ = result(t, got[0])["pane"].(string)
	tm.work = paneIn(t, s, tm.child, "writer")
	roundTrip(t, s, `{"id":"1","cmd":"task.set_foreman","task":"`+tm.team+`","pane":"`+tm.foreman+`"}`)
	return tm
}

// setState reports a gate state with no ask for pane.
func setState(t *testing.T, s *Server, pane, state string) {
	t.Helper()
	got := roundTrip(t, s, fmt.Sprintf(`{"id":"1","cmd":"pane.report_state","pane":"%s","event":`+
		`{"v":1,"ts":%.3f,"session_id":"x","harness":"shell","pane":"%s","state":"%s","source":"gate"}}`,
		pane, nowSeconds(), pane, state))
	if !got[0].OK {
		t.Fatalf("report %s: %+v", state, got[0].Error)
	}
}

// screen is the visible text of pane, read as one line.
func screen(t *testing.T, s *Server, pane string) string {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.read","pane":"`+pane+`"}`)
	text, _ := result(t, got[0])["text"].(string)
	return strings.ReplaceAll(text, "\n", "")
}

// waitScreen waits until pane's screen holds want.
func waitScreen(t *testing.T, s *Server, pane, want string) string {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for {
		text := screen(t, s, pane)
		if strings.Contains(text, want) {
			return text
		}
		if time.Now().After(end) {
			t.Fatalf("%s never showed %q: %q", pane, want, text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An idle foreman gets one line when a hold starts: the pane, the ask and
// the two commands, never the ask's summary, which the child wrote.
func TestAnIdleForemanIsToldOnceAndNeverGetsTheSummary(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "idle")
	tm.report(t, tm.work, "ask-1", "undoable")
	text := waitScreen(t, tm.s, tm.foreman, "coppice agent deny "+tm.work+" h1")
	if !strings.Contains(text, "coppice agent get "+tm.work) {
		t.Fatalf("the line has no get command: %q", text)
	}
	if strings.Contains(text, "rm -rf") {
		t.Fatalf("the ask summary reached the foreman: %q", text)
	}
	// Idle again later: no second line. The pty echoes the line and cat
	// prints it again, so the count is taken once the screen settles.
	time.Sleep(200 * time.Millisecond)
	before := strings.Count(screen(t, tm.s, tm.foreman), "coppice agent deny")
	setState(t, tm.s, tm.foreman, "working")
	setState(t, tm.s, tm.foreman, "idle")
	time.Sleep(200 * time.Millisecond)
	if after := strings.Count(screen(t, tm.s, tm.foreman), "coppice agent deny"); after != before {
		t.Fatalf("the foreman got a second line: %d copies, then %d", before, after)
	}
}

// A working foreman gets no bytes. It gets the line when it goes idle
// while it still holds the ask.
func TestAWorkingForemanIsToldOnlyWhenItGoesIdle(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "working")
	tm.report(t, tm.work, "ask-1", "undoable")
	time.Sleep(200 * time.Millisecond)
	if text := screen(t, tm.s, tm.foreman); text != "" {
		t.Fatalf("a working foreman got bytes: %q", text)
	}
	setState(t, tm.s, tm.foreman, "idle")
	waitScreen(t, tm.s, tm.foreman, "coppice agent deny "+tm.work+" h1")
}

// A foreman that blocks while it holds an ask gets no bytes: an Enter
// there would answer its own question. Its holds go to the operator.
func TestABlockedForemanGetsNoBytes(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "working")
	tm.report(t, tm.work, "ask-1", "undoable")
	tm.report(t, tm.foreman, "ask-f", "undoable")
	time.Sleep(200 * time.Millisecond)
	if text := screen(t, tm.s, tm.foreman); text != "" {
		t.Fatalf("a blocked foreman got bytes: %q", text)
	}
	if _, held := row(t, tm.s, tm.work)["held"]; held {
		t.Fatal("a blocked foreman kept its hold")
	}
}

// An ask id the child chose in an odd shape never reaches the foreman:
// the line names the hold by the server's own id instead.
func TestAnOddAskIDIsNeverTyped(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "idle")
	tm.report(t, tm.work, "ask 1; rm", "undoable")
	text := waitScreen(t, tm.s, tm.foreman, "coppice agent deny "+tm.work+" h")
	if strings.Contains(text, "ask 1; rm") || strings.Contains(text, "; rm") {
		t.Fatalf("an odd ask id reached the foreman: %q", text)
	}
}

// The line names the hold by a short id the server makes, and deny takes
// that id in place of the ask id.
func TestTheForemanDeniesByTheHoldID(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "idle")
	tm.report(t, tm.work, "ask-1", "undoable")
	h, _ := row(t, tm.s, tm.work)["held"].(map[string]any)
	id, _ := h["hold"].(string)
	if !strings.HasPrefix(id, "h") {
		t.Fatalf("held = %v, want a hold id", h)
	}
	waitScreen(t, tm.s, tm.foreman, "coppice agent deny "+tm.work+" "+id)
	got := roundTripFacts(t, tm.s, &peerFacts{checked: true, pane: true, paneID: tm.foreman},
		`{"id":"1","cmd":"agent.deny","pane":"`+tm.work+`","ask":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("deny by hold id: %+v", got[0].Error)
	}
	if ans, ok := answerFile(t, tm.s.cfg.GateRoot, "ask-1"); !ok || ans["decision"] != "deny" {
		t.Fatalf("answer = %v", ans)
	}
}

// A foreman the operator typed into may hold a half-typed line. A turn
// that ends does not clear it. The server's line waits until the operator
// submits the line and a turn ends after it, and the server never clears
// the operator's text.
func TestAForemanWithOperatorInputWaitsForASubmittedTurn(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "working")
	setState(t, tm.s, tm.foreman, "idle")
	got := roundTrip(t, tm.s, `{"id":"1","cmd":"pane.send_text","pane":"`+tm.foreman+`","text":"half","enter":false}`)
	if !got[0].OK {
		t.Fatalf("send_text: %+v", got[0].Error)
	}
	tm.report(t, tm.work, "ask-1", "undoable")
	time.Sleep(200 * time.Millisecond)
	if text := screen(t, tm.s, tm.foreman); strings.Contains(text, "coppice agent") {
		t.Fatalf("the line joined the operator's text: %q", text)
	}
	setState(t, tm.s, tm.foreman, "working")
	setState(t, tm.s, tm.foreman, "idle")
	assertNoLine(t, tm)
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.send_keys","pane":"`+tm.foreman+`","keys":["enter"]}`)
	setState(t, tm.s, tm.foreman, "working")
	setState(t, tm.s, tm.foreman, "idle")
	waitScreen(t, tm.s, tm.foreman, "coppice agent deny "+tm.work)
}

// A restart closes every pane, so no task names a foreman after it.
func TestARestartClearsEveryForeman(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	task, _ := createTask(t, s1, `"label":"review-team","cwd":"/"`)
	lead := paneIn(t, s1, task, "lead")
	roundTrip(t, s1, `{"id":"1","cmd":"task.set_foreman","task":"`+task+`","pane":"`+lead+`"}`)
	_ = s1.Close()
	s2, err := New(Config{SocketPath: filepath.Join(dir, "b.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	if f := listTasks(t, s2)[task]["foreman"]; f != "" {
		t.Fatalf("task foreman = %v after a restart", f)
	}
}

// A pty foreman's own echo makes it working, and the process source calls
// it idle once the grid is quiet past processQuiet. That idle is no turn
// end: the operator may have stopped in the middle of a sentence. The
// clock is moved past the window, not slept through.
func TestAProcessIdleNeverClearsTheInputMarkOrTypes(t *testing.T) {
	tm := catTeam(t)
	lp, ok := tm.s.Live(tm.foreman)
	if !ok {
		t.Fatal("no live foreman")
	}
	got := roundTrip(t, tm.s, `{"id":"1","cmd":"pane.send_text","pane":"`+tm.foreman+`","text":"half","enter":false}`)
	if !got[0].OK {
		t.Fatalf("send_text: %+v", got[0].Error)
	}
	waitScreen(t, tm.s, tm.foreman, "half")
	tm.report(t, tm.work, "ask-1", "undoable")
	tm.s.processTick(tm.foreman, lp)
	if st, _ := tm.s.effectiveState(tm.foreman); st.State != "working" || st.Source != "process" {
		t.Fatalf("after the echo the foreman is %s from %s, want working from process", st.State, st.Source)
	}
	tm.s.clockSkew.Store(int64(2 * processQuiet))
	tm.s.processTick(tm.foreman, lp)
	if st, _ := tm.s.effectiveState(tm.foreman); st.State != "idle" || st.Source != "process" {
		t.Fatalf("past the window the foreman is %s from %s, want idle from process", st.State, st.Source)
	}
	time.Sleep(200 * time.Millisecond)
	if text := screen(t, tm.s, tm.foreman); strings.Contains(text, "coppice agent") {
		t.Fatalf("a process idle let the line in after the operator's text: %q", text)
	}
	tm.s.holdsMu.Lock()
	marked := tm.s.typed[tm.foreman]
	tm.s.holdsMu.Unlock()
	if !marked {
		t.Fatal("a process idle cleared the input mark")
	}
}

// The server marks its own line as input, so a foreman gets one line per
// turn: a second hold waits for the next turn that ends.
func TestTwoHoldsOnOneForemanGetOneLinePerTurn(t *testing.T) {
	tm := catTeam(t)
	second := paneIn(t, tm.s, tm.child, "second")
	setState(t, tm.s, tm.foreman, "working")
	setState(t, tm.s, tm.foreman, "idle")
	tm.report(t, tm.work, "ask-1", "undoable")
	tm.report(t, second, "ask-2", "undoable")
	text := waitScreen(t, tm.s, tm.foreman, "coppice: hold h")
	time.Sleep(200 * time.Millisecond)
	text = screen(t, tm.s, tm.foreman)
	if strings.Contains(text, "hold h1") == strings.Contains(text, "hold h2") {
		t.Fatalf("want exactly one hold line after one idle: %q", text)
	}
	setState(t, tm.s, tm.foreman, "working")
	setState(t, tm.s, tm.foreman, "idle")
	text = waitScreen(t, tm.s, tm.foreman, "hold h1")
	waitScreen(t, tm.s, tm.foreman, "hold h2")
	_ = text
}

// setManifestIdle reports an idle from a manifest read for pane.
func setManifestIdle(t *testing.T, s *Server, pane string) {
	t.Helper()
	id := pane
	s.ApplyState(pane, proto.PaneStateEvent{V: 1, TS: nowSeconds(), SessionID: pane, Harness: "claude-code",
		Pane: &id, State: proto.StateIdle, Source: proto.SrcManifest, Detail: "live_prompt_box"})
}

// assertNoLine waits a moment for any hold line to land, then proves none
// reached the foreman's screen.
func assertNoLine(t *testing.T, tm team) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if text := screen(t, tm.s, tm.foreman); strings.Contains(text, "coppice") {
		t.Fatalf("a hold line reached the foreman after the operator's text: %q", text)
	}
}

// The operator types half a sentence and pauses. The echo gives a process
// working, and a manifest read of the prompt box, which holds the half
// line, gives an idle. That idle is no turn end, and no bytes go.
//
// The foreman has no gate state here, the way it has none once the gate's
// short hold on its own report lapses, so the process and manifest
// sources decide its state.
func TestAPauseAfterTypingSendsNoBytes(t *testing.T) {
	tm := catTeam(t)
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.send_text","pane":"`+tm.foreman+`","text":"half","enter":false}`)
	waitScreen(t, tm.s, tm.foreman, "half")
	tm.report(t, tm.work, "ask-1", "undoable")
	lp, _ := tm.s.Live(tm.foreman)
	tm.s.processTick(tm.foreman, lp)
	if st, _ := tm.s.effectiveState(tm.foreman); st.State != "working" {
		t.Fatalf("after the echo the foreman is %s from %s, want working", st.State, st.Source)
	}
	// Past the merge's hold window, which keeps a manifest read back from
	// a fresh process fact, and still inside the process quiet window.
	tm.s.clockSkew.Store(int64(3 * time.Second))
	setManifestIdle(t, tm.s, tm.foreman)
	if st, _ := tm.s.effectiveState(tm.foreman); st.State != "idle" || st.Source != "manifest" {
		t.Fatalf("after the read the foreman is %s from %s, want idle from manifest", st.State, st.Source)
	}
	assertNoLine(t, tm)
}

// The operator types ahead while the foreman works. The hook's idle ends
// the turn, but the typed-ahead text is still in the input line, so no
// bytes go.
func TestTypingAheadDuringATurnSendsNoBytes(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "working")
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.send_text","pane":"`+tm.foreman+`","text":"ahead","enter":false}`)
	waitScreen(t, tm.s, tm.foreman, "ahead")
	tm.report(t, tm.work, "ask-1", "undoable")
	setState(t, tm.s, tm.foreman, "idle")
	assertNoLine(t, tm)
}

// Input that ends with Enter, then a turn that the hook ends, clears the
// mark, and the line goes then.
func TestASubmittedLineThenATurnEndLetsTheLineGo(t *testing.T) {
	tm := catTeam(t)
	setState(t, tm.s, tm.foreman, "working")
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.send_text","pane":"`+tm.foreman+`","text":"half","enter":false}`)
	roundTrip(t, tm.s, `{"id":"1","cmd":"pane.send_keys","pane":"`+tm.foreman+`","keys":["enter"]}`)
	tm.report(t, tm.work, "ask-1", "undoable")
	setState(t, tm.s, tm.foreman, "idle")
	waitScreen(t, tm.s, tm.foreman, "coppice: hold h1")
}
