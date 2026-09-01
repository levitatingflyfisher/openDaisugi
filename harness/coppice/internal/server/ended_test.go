package server

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// newEndedServer is newPaneServer plus pane.forget and pane.resume, which
// New wires in for a real binary but a pane-only test server does not get
// unless it asks.
func newEndedServer(t *testing.T) *Server {
	t.Helper()
	s := newPaneServer(t)
	s.RegisterEndedCommands()
	return s
}

// waitDone waits for id to reach done through pane.wait_output, never
// waitState: a pane that reaches done is also Closed, so it leaves the
// live pane.list waitState polls the instant it gets there - polling the
// live list for "done" can never see it land.
func waitDone(t *testing.T, s *Server, id string) {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.wait_output","pane":"`+id+`","state":"done","timeout_ms":5000}`)
	if !got[0].OK {
		t.Fatalf("waiting for %s to reach done failed: %+v", id, got[0].Error)
	}
}

// An operator pane.close removes the record at once: it never shows up
// under `ended: true` either. That is the line between a pane the
// operator ends and one that ends on its own.
func TestOperatorClosedPaneNeverListsAsEnded(t *testing.T) {
	s := newEndedServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	roundTrip(t, s, `{"id":"1","cmd":"pane.close","pane":"`+id+`"}`)
	for _, row := range listEndedPanes(t, s) {
		if row["id"] == id {
			t.Fatalf("an operator-closed pane appeared under ended: true: %v", row)
		}
	}
}

// A pane whose process exits on its own leaves the live list and appears
// under `ended: true`, newest first, with ended_at and exit_code.
func TestSelfEndedPaneMovesToEnded(t *testing.T) {
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 4"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	for _, row := range listPanes(t, s) {
		if row["id"] == "w1:p1" {
			t.Fatalf("a self-ended pane is still in the live list: %v", row)
		}
	}
	row := rowFor(t, listEndedPanes(t, s), "w1:p1")
	if row["closed"] != true {
		t.Fatalf("ended row = %v, want closed true", row)
	}
	if row["exit_code"] != float64(4) {
		t.Fatalf("ended row exit_code = %v, want 4", row["exit_code"])
	}
	if row["ended_at"] == nil || row["ended_at"] == float64(0) {
		t.Fatalf("ended row has no ended_at: %v", row)
	}
}

// agent.list follows pane.list's own rule.
func TestAgentListFollowsTheSameEndedRule(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`,
		`{"id":"3","cmd":"agent.list"}`,
		`{"id":"4","cmd":"agent.list","ended":true}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	live, _ := result(t, got[2])["agents"].([]any)
	if len(live) != 0 {
		t.Fatalf("agent.list (live only) = %v, want empty", live)
	}
	ended, _ := result(t, got[3])["agents"].([]any)
	if len(ended) != 1 {
		t.Fatalf("agent.list ended = %v, want one row", ended)
	}
	row, _ := ended[0].(map[string]any)
	if row["pane"] != "w1:p1" || row["ended_at"] == nil {
		t.Fatalf("agent.list ended row = %v, want pane w1:p1 with ended_at", row)
	}
}

func TestPaneForgetOnALivePaneIsBadRequest(t *testing.T) {
	s := newEndedServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.forget","pane":"`+id+`"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

func TestPaneForgetOnAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newEndedServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.forget","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

func TestPaneForgetWithNeitherPaneNorEndedIsBadRequest(t *testing.T) {
	s := newEndedServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.forget"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

// pane and ended: true together is ambiguous - does the caller want one
// record or all of them - and answering it as "all of them" would wipe
// every other ended record the caller never named. Refused outright,
// rather than silently picking one meaning over the other.
func TestPaneForgetWithBothPaneAndEndedIsBadRequest(t *testing.T) {
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 1"],"kind":"pty"}`)
	if !got[0].OK || !got[1].OK {
		t.Fatalf("pane.create failed: %+v %+v", got[0].Error, got[1].Error)
	}
	waitDone(t, s, "w1:p1")
	waitDone(t, s, "w1:p2")

	res := roundTrip(t, s, `{"id":"3","cmd":"pane.forget","pane":"w1:p1","ended":true}`)
	if res[0].OK || res[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", res[0])
	}
	if len(listEndedPanes(t, s)) != 2 {
		t.Fatalf("a refused pane.forget removed a record: %v", listEndedPanes(t, s))
	}
}

func TestPaneForgetRemovesOneEndedRecord(t *testing.T) {
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.forget","pane":"w1:p1"}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	if !got[2].OK {
		t.Fatalf("pane.forget failed: %+v", got[2].Error)
	}
	m := result(t, got[2])
	if m["pane"] != "w1:p1" || m["forgot"] != true {
		t.Fatalf("pane.forget result = %v", m)
	}
	for _, row := range listEndedPanes(t, s) {
		if row["id"] == "w1:p1" {
			t.Fatalf("forgotten pane still listed as ended: %v", row)
		}
	}
	if _, ok := s.Live("w1:p1"); ok {
		t.Fatal("pane.forget left the grid live")
	}
}

func TestPaneForgetEndedRemovesEveryEndedRecordAndCountsThem(t *testing.T) {
	s := newEndedServer(t)
	cwd := t.TempDir()
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 1"],"kind":"pty"}`,
		`{"id":"3","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	waitDone(t, s, "w1:p1")
	waitDone(t, s, "w1:p2")
	got := roundTrip(t, s, `{"id":"4","cmd":"pane.forget","ended":true}`)
	if !got[0].OK {
		t.Fatalf("pane.forget --ended failed: %+v", got[0].Error)
	}
	m := result(t, got[0])
	if m["forgot"] != float64(2) {
		t.Fatalf("pane.forget --ended forgot = %v, want 2", m["forgot"])
	}
	if len(listEndedPanes(t, s)) != 0 {
		t.Fatalf("ended list not empty after forget --ended: %v", listEndedPanes(t, s))
	}
	live := listPanes(t, s)
	if len(live) != 1 || live[0]["id"] != "w1:p3" {
		t.Fatalf("live list after forget --ended = %v, want only w1:p3", live)
	}
}

// A headless pane with a recorded harness session id resumes that session,
// the same way a restart does: the new pane's record carries the same
// session id, and the ended record is gone.
func TestPaneResumeReusesAHeadlessSessionID(t *testing.T) {
	s := newEndedServer(t)
	proc := newFakeProc()
	proc.setSessionID("sess-resume-1")
	adapters.Register(&fakeAdapter{name: "fake-resume-headless", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-resume-headless","label":"h1"}`)
	id, _ := result(t, got[0])["pane"].(string)
	proc.send(pane.Event{Kind: pane.EvText, Text: "hi"})
	waitFor(t, 2*time.Second, func() bool {
		info, ok := s.paneInfo(id)
		return ok && info.HarnessSessionID == "sess-resume-1"
	})
	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "done"})
	waitFor(t, 2*time.Second, func() bool {
		p, ok := s.tree.Pane(id)
		return ok && p.Closed
	})

	got = roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	m := result(t, got[0])
	if m["resumed"] != true {
		t.Fatalf("pane.resume result = %v, want resumed true", m)
	}
	newID, _ := m["pane"].(string)
	if newID == "" || newID == id {
		t.Fatalf("pane.resume returned pane %q, want a fresh id", newID)
	}
	newRec, ok := s.tree.Pane(newID)
	if !ok {
		t.Fatal("the resumed pane has no record")
	}
	if newRec.HarnessSessionID != "sess-resume-1" || newRec.Label != "h1" {
		t.Fatalf("resumed record = %+v, want session sess-resume-1 and label h1", newRec)
	}
	for _, row := range listEndedPanes(t, s) {
		if row["id"] == id {
			t.Fatalf("the old ended record is still listed after a successful resume: %v", row)
		}
	}
}

// A pty pane resumes when its harness table has resume_args and the record
// carries a harness session id: the new pane's command line is built from
// resume_args, with {session} replaced. Nothing in today's code records a
// harness session id for a pty pane on its own - this test sets it by hand
// to exercise the mechanism a future harness adapter will feed.
func TestPaneResumeBuildsAPtyCommandFromResumeArgs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c"}, ResumeArgs: []string{"echo RESUMED-{session}; sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","harness":"fakeh","kind":"pty","args":["exit 0"]}`)
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)
	if err := s.updatePane(id, func(p *layout.Pane) { p.HarnessSessionID = "sess-pty-1" }); err != nil {
		t.Fatal(err)
	}

	got = roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	m := result(t, got[0])
	if m["resumed"] != true {
		t.Fatalf("pane.resume result = %v, want resumed true", m)
	}
	newID, _ := m["pane"].(string)
	waitState(t, s, newID, "working", 5*time.Second)
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.wait_output","pane":"`+newID+`","contains":"RESUMED-sess-pty-1","timeout_ms":5000}`)
	if !got[0].OK {
		t.Fatalf("the resumed pane never printed the resume command's own output: %+v", got[0].Error)
	}
}

// A resumed pty pane keeps whatever it was given past the harness's own
// fixed args at pane.create time (a task prompt, say): resume_args goes
// first, then that, never dropped.
func TestPaneResumeAppendsThePanesOwnExtraArgsAfterResumeArgs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c"}, ResumeArgs: []string{"echo resuming"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","harness":"fakeh","kind":"pty","args":["exit 0","--task","abc"]}`)
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)
	rec, _ := s.tree.Pane(id)
	if len(rec.ExtraArgs) != 3 || rec.ExtraArgs[0] != "exit 0" {
		t.Fatalf("ExtraArgs = %v, want the pane.create request's own args list", rec.ExtraArgs)
	}
	if err := s.updatePane(id, func(p *layout.Pane) { p.HarnessSessionID = "sess-extra-1" }); err != nil {
		t.Fatal(err)
	}

	got = roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	newID, _ := result(t, got[0])["pane"].(string)

	newRec, ok := s.tree.Pane(newID)
	if !ok {
		t.Fatal("the resumed pane has no record")
	}
	want := []string{"sh", "-c", "echo resuming", "exit 0", "--task", "abc"}
	if len(newRec.Argv) != len(want) {
		t.Fatalf("resumed argv = %v, want %v", newRec.Argv, want)
	}
	for i, w := range want {
		if newRec.Argv[i] != w {
			t.Fatalf("resumed argv = %v, want %v", newRec.Argv, want)
		}
	}
}

// A pty pane with no resume_args configured for its harness, or none at
// all, starts fresh from its own recorded argv: resumed: false, never a
// guess dressed up as a fact.
func TestPaneResumeStartsAPtyFreshWithNoResumeArgs(t *testing.T) {
	s := newEndedServer(t)
	id := createShellPane(t, s, "sh", "-c", "printf original; exit 0")
	waitDone(t, s, id)

	got := roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	m := result(t, got[0])
	if m["resumed"] != false {
		t.Fatalf("pane.resume result = %v, want resumed false", m)
	}
	newID, _ := m["pane"].(string)
	if newID == "" || newID == id {
		t.Fatalf("pane.resume returned pane %q, want a fresh id", newID)
	}
	waitDone(t, s, newID)
}

// pane.resume takes an optional size, so a client can start the new pane
// at the size of the window it will show in. With none, the ended
// record's own size is kept. A size that is not positive is refused.
func TestPaneResumeTakesAnOptionalSize(t *testing.T) {
	s := newEndedServer(t)
	id := createShellPane(t, s, "sh", "-c", "exit 0")
	waitDone(t, s, id)
	got := roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`","cols":0,"rows":10}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("a zero size got %+v, want bad_request", got[0])
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.resume","pane":"`+id+`","cols":81,"rows":33}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	newID, _ := result(t, got[0])["pane"].(string)
	if rec, ok := s.tree.Pane(newID); !ok || rec.Cols != 81 || rec.Rows != 33 {
		t.Fatalf("resumed record %+v, want 81x33", rec)
	}
	waitDone(t, s, newID)
	old, _ := s.tree.Pane(newID)
	got = roundTrip(t, s, `{"id":"4","cmd":"pane.resume","pane":"`+newID+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	third, _ := result(t, got[0])["pane"].(string)
	if rec, _ := s.tree.Pane(third); rec.Cols != old.Cols || rec.Rows != old.Rows {
		t.Fatalf("with no size the record is %dx%d, want the old %dx%d", rec.Cols, rec.Rows, old.Cols, old.Rows)
	}
}

func TestPaneResumeOnALivePaneIsBadRequest(t *testing.T) {
	s := newEndedServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.resume","pane":"`+id+`"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

func TestPaneResumeOnAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newEndedServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.resume","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

// server.status's resumable count is honest: it only counts an ended
// record pane.resume would actually resume, not merely one that ended.
func TestServerStatusResumableCountsOnlyWhatCanActuallyResume(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c"}, ResumeArgs: []string{"echo hi"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	cwd := t.TempDir()
	// w1:p1: pty, no session id recorded - not resumable.
	roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`)
	// w1:p2: pty with fakeh's resume_args AND a session id - resumable.
	roundTrip(t, s, `{"id":"2","cmd":"pane.create","cwd":"`+cwd+`","harness":"fakeh","kind":"pty","args":["exit 0"]}`)
	waitDone(t, s, "w1:p1")
	waitDone(t, s, "w1:p2")
	if err := s.updatePane("w1:p2", func(p *layout.Pane) { p.HarnessSessionID = "sess-1" }); err != nil {
		t.Fatal(err)
	}

	got := roundTrip(t, s, `{"id":"3","cmd":"server.status"}`)
	m := result(t, got[0])
	if m["resumable"] != float64(1) {
		t.Fatalf("resumable = %v, want 1 (only w1:p2)", m["resumable"])
	}
}

// The seven-day sweep removes an ended record once it is old enough, and
// leaves a fresh one alone.
func TestSweepEndedRemovesOnlyRecordsOlderThanSevenDays(t *testing.T) {
	s := newEndedServer(t)
	cwd := t.TempDir()
	roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`)
	waitDone(t, s, "w1:p1")

	if n := s.SweepEnded(); n != 0 {
		t.Fatalf("SweepEnded removed %d records before seven days passed, want 0", n)
	}
	if row := rowFor(t, listEndedPanes(t, s), "w1:p1"); row["id"] != "w1:p1" {
		t.Fatalf("a fresh ended record was swept: %v", row)
	}

	s.clockSkew.Store(int64(8 * 24 * time.Hour))
	if n := s.SweepEnded(); n != 1 {
		t.Fatalf("SweepEnded removed %d records after eight days, want 1", n)
	}
	if len(listEndedPanes(t, s)) != 0 {
		t.Fatalf("the swept record is still listed: %v", listEndedPanes(t, s))
	}
}

// Restore stamps EndedAt onto a record it finds already Closed with none -
// a layout saved before this feature existed - so it can still land in
// Recent and eventually age out, instead of sitting invisible forever.
func TestRestoreStampsEndedAtOnAnOldShapeClosedRecord(t *testing.T) {
	dir := t.TempDir()
	tr := layout.New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, err := tr.CreateTab(ws.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := tr.CreatePane(ws.ID, tab.ID, layout.Pane{Cwd: "/repo", Kind: layout.KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	// Old-shape close: Closed true, EndedAt still zero. layout.Tree has no
	// method that produces this any more, so this pokes the field
	// directly - the one legitimate way to build the fixture this test
	// needs.
	if err := tr.UpdatePane(rec.ID, func(p *layout.Pane) { p.Closed = true }); err != nil {
		t.Fatal(err)
	}
	if err := tr.Save(layoutPath(dir)); err != nil {
		t.Fatal(err)
	}

	s, err := New(Config{SocketPath: dir + "/server.sock", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s.RegisterPaneCommands()
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, listEndedPanes(t, s), rec.ID)
	if row["ended_at"] == nil || row["ended_at"] == float64(0) {
		t.Fatalf("Restore did not stamp ended_at on an old-shape closed record: %v", row)
	}
}

// Two concurrent pane.resume calls for the same ended id must not both
// spawn a live pane from it: exactly one succeeds, and the id's record is
// gone once, not twice over. handlePaneResume is called directly, twice,
// released from a shared start barrier so both calls actually overlap
// inside the handler rather than merely running one after the other.
func TestConcurrentPaneResumeOfTheSameIDStartsOnlyOnePane(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c"}, ResumeArgs: []string{"sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","harness":"fakeh","kind":"pty","args":["exit 0"]}`)
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)
	// A resumed record's new pane is given resume_args (a long sleep, so
	// it is still live to count once the race settles), never the
	// original argv (exit 0) again - or both concurrent calls' new panes
	// would end on their own before this test could tell one from two.
	if err := s.updatePane(id, func(p *layout.Pane) { p.HarnessSessionID = "sess-race-1" }); err != nil {
		t.Fatal(err)
	}

	req := func() *proto.Request {
		var r proto.Request
		if err := json.Unmarshal(
			[]byte(`{"id":"1","cmd":"pane.resume","pane":"`+id+`"}`), &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}

	start := make(chan struct{})
	results := make(chan proto.Response, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- s.handlePaneResume(nil, req())
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	// The loser's exact code depends on when it claims relative to the
	// winner's own release: bad_request "already resuming" if it arrives
	// while the winner still holds the claim, no_such_pane if it claims
	// only after the winner's own dropRecord already ran. Either is
	// correct; the load-bearing fact is that only one call ever spawns a
	// pane, checked below.
	var oks, refused int
	for resp := range results {
		if resp.OK {
			oks++
			continue
		}
		refused++
		if resp.Error.Code != proto.ErrBadRequest && resp.Error.Code != proto.ErrNoSuchPane {
			t.Fatalf("the refused call got %+v, want bad_request or no_such_pane", resp)
		}
	}
	if oks != 1 || refused != 1 {
		t.Fatalf("got %d ok and %d refused, want exactly one of each", oks, refused)
	}

	live := listPanes(t, s)
	newPanes := 0
	for _, row := range live {
		if row["id"] != id {
			newPanes++
		}
	}
	if newPanes != 1 {
		t.Fatalf("live panes after the race = %v, want exactly one new pane, not %d", live, newPanes)
	}
	if len(listEndedPanes(t, s)) != 0 {
		t.Fatalf("the ended record is still listed after a successful resume: %v", listEndedPanes(t, s))
	}
}

// A restart that finds a pty pane's process gone stamps EndedAt the same
// way any other ended pane does, so it lists under `ended: true` and ages
// out through the same seven-day sweep - not a separate, invisible case.
func TestRestoreStampsEndedAtOnAPtyPaneTheProcessDidNotSurvive(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(Config{SocketPath: dir + "/a.sock", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s1.RegisterPaneCommands()
	roundTrip(t, s1,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	s1.saveLayout()
	_ = s1.Close()

	s2, err := New(Config{SocketPath: dir + "/b.sock", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s2.RegisterPaneCommands()
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, listEndedPanes(t, s2), "w1:p1")
	if row["ended_at"] == nil || row["ended_at"] == float64(0) {
		t.Fatalf("a restart did not stamp ended_at on a pty pane the process did not survive: %v", row)
	}
	if row["closed"] != true {
		t.Fatalf("ended row = %v, want closed true", row)
	}
}

// A pane that already ended before a restart has no state event in the
// new server's fresh state store: Restore's AlreadyClosed branch only
// stamps ended_at, it never invents a receive-time event for a fact this
// server never itself observed. pane.list and agent.list must still
// report it honestly, from the record's own closed/exit_code/ended_at,
// rather than unknown with no ts - the shape find_ended (the Python
// floor backend) needs to reassemble anything at all.
func TestPaneListReportsDoneForAPaneThatEndedBeforeARestart(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(Config{SocketPath: dir + "/a.sock", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s1.RegisterPaneCommands()
	got := roundTrip(t, s1,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","exit 7"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	s1.saveLayout()
	_ = s1.Close()

	s2, err := New(Config{SocketPath: dir + "/b.sock", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s2.RegisterPaneCommands()
	s2.RegisterAgentCommands()
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}

	row := rowFor(t, listEndedPanes(t, s2), "w1:p1")
	if row["state"] != "done" {
		t.Fatalf("pane.list state = %v, want done", row["state"])
	}
	if row["source"] != "process" {
		t.Fatalf("pane.list source = %v, want process", row["source"])
	}
	if row["detail"] != "exit=7" {
		t.Fatalf("pane.list detail = %v, want exit=7", row["detail"])
	}
	endedAt, _ := row["ended_at"].(float64)
	ts, _ := row["ts"].(float64)
	if endedAt == 0 {
		t.Fatal("ended_at is 0")
	}
	if ts != endedAt {
		t.Fatalf("pane.list ts = %v, want it to equal ended_at %v (the real end time, not the moment this row is read)", ts, endedAt)
	}

	agentGot := roundTrip(t, s2, `{"id":"3","cmd":"agent.list","ended":true}`)
	agents, _ := result(t, agentGot[0])["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agent.list ended = %v, want one row", agents)
	}
	agentRow, _ := agents[0].(map[string]any)
	if agentRow["state"] != "done" || agentRow["source"] != "process" || agentRow["detail"] != "exit=7" {
		t.Fatalf("agent.list row = %v, want done/process/exit=7", agentRow)
	}
}

// A resumed pane keeps the env its ended record was started with, so a
// resumed agent still talks to the gateway it was pointed at.
func TestPaneResumeKeepsTheRecordsEnv(t *testing.T) {
	s := newEndedServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty",`+
			`"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9/gw"}}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)

	got = roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	newID, _ := result(t, got[0])["pane"].(string)
	rec, ok := s.tree.Pane(newID)
	if !ok || rec.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9/gw" {
		t.Fatalf("resumed record env = %v, want ANTHROPIC_BASE_URL kept", rec.Env)
	}
	waitDone(t, s, newID)
}
