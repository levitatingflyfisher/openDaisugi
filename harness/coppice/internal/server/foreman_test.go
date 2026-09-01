package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
	"github.com/opendaisugi/coppice/skills"
)

// fakeForeman writes a config whose default harness, fakeh, is a shell
// script. It turns bracketed paste on, draws one line, and writes each
// line it reads to the returned file. A line that holds EXIT-NOW ends it.
// No real harness ever runs.
func fakeForeman(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := filepath.Join(t.TempDir(), "in")
	script := `printf '\033[?2004h'; echo ready; ` +
		`while IFS= read -r l; do printf '%s\n' "$l" >> '` + in + `'; ` +
		`case "$l" in *EXIT-NOW*) exit 0;; esac; done`
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", script}},
	}}); err != nil {
		t.Fatal(err)
	}
	return in
}

// talk sends floor.talk with text and returns the reply.
func talk(t *testing.T, s *Server, text string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "floor.talk", "text": text})
	return result(t, roundTrip(t, s, string(b))[0])
}

// waitFile waits until the file at path holds want, and returns its text.
func waitFile(t *testing.T, path, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), want) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never reached the foreman. It read:\n%s", want, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gateIdle marks pane id idle as a gate hook does at the end of a turn,
// once the pane drew its first line, so a test need not wait for the
// screen to go quiet.
func gateIdle(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if lp, ok := s.Live(id); ok {
			if _, drawn := lp.Grid.LastWrite(); drawn {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never drew", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcGate,
	})
}

// foremen lists the live top panes labelled foreman.
func foremen(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range listPanes(t, s) {
		if r["label"] == "foreman" {
			out = append(out, r)
		}
	}
	return out
}

// pageMark is a line of the foreman page that the tests look for.
const pageMark = "## Your commands"

func TestTheFirstTalkStartsTheForemanAndPagesItBeforeTheWords(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	res := talk(t, s, "hello foreman")
	id, _ := res["pane"].(string)
	if id == "" || res["started"] != true || res["label"] != "foreman" {
		t.Fatalf("floor.talk = %v, want a started foreman", res)
	}
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	dir := s.foremanDir()
	if row["label"] != "foreman" || row["harness"] != "fakeh" || row["cwd"] != dir {
		t.Fatalf("row = %v, want label foreman, harness fakeh, cwd %s", row, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the foreman dir was not made: %v", err)
	}
	// Real timing: the screen goes quiet, the page goes in, the page's
	// turn ends, then the words go in.
	got := waitFile(t, in, "hello foreman", 40*time.Second)
	page := strings.Index(got, pageMark)
	words := strings.Index(got, "hello foreman")
	if page < 0 || page > words {
		t.Fatalf("the page did not come first:\n%s", got)
	}
	if !strings.Contains(got, "\x1b[200~") || !strings.Contains(got, "\x1b[201~") {
		t.Fatalf("the page was not sent as one paste:\n%q", got)
	}
	for _, d := range loadRecentDirs(s.cfg.DataDir) {
		if d == dir {
			t.Fatalf("the foreman dir joined the recent projects: %v", loadRecentDirs(s.cfg.DataDir))
		}
	}
}

func TestTalksWhileTheForemanStartsQueueInOrderBehindOne(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	first := talk(t, s, "WORD-ONE")
	second := talk(t, s, "WORD-TWO")
	third := talk(t, s, "WORD-THREE")
	id, _ := first["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if second["pane"] != id || third["pane"] != id {
		t.Fatalf("the talks went to different panes: %v %v %v", first, second, third)
	}
	if second["started"] != false || third["started"] != false {
		t.Fatalf("a queued talk started a foreman: %v %v", second, third)
	}
	if n := len(foremen(t, s)); n != 1 {
		t.Fatalf("%d foremen run, want one", n)
	}
	gateIdle(t, s, id)
	got := waitFile(t, in, "WORD-THREE", 10*time.Second)
	at := func(w string) int { return strings.Index(got, w) }
	if !(at(pageMark) < at("WORD-ONE") && at("WORD-ONE") < at("WORD-TWO") && at("WORD-TWO") < at("WORD-THREE")) {
		t.Fatalf("the lines came out of order:\n%s", got)
	}
}

func TestADeadForemanIsReplacedOnTheNextTalkWithANote(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := talk(t, s, "EXIT-NOW")
	old, _ := res["pane"].(string)
	gateIdle(t, s, old)
	waitFile(t, in, "EXIT-NOW", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for len(foremen(t, s)) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the foreman never ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	res = talk(t, s, "are you back")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if id == "" || id == old || res["started"] != true {
		t.Fatalf("floor.talk = %v, want a new foreman", res)
	}
	note, _ := res["note"].(string)
	if !strings.Contains(note, "ended") || strings.Contains(note, "\n") {
		t.Fatalf("note = %q, want one line that says the old one ended", note)
	}
	notes := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.notes"}`)[0])
	if !strings.Contains(stringsOf(notes["notes"]), note) {
		t.Fatalf("the floor did not get the note: %v", notes)
	}
	gateIdle(t, s, id)
	waitFile(t, in, "are you back", 10*time.Second)
}

func TestAForemanThatDiesBeforeItIsReadyDropsTheWordsWithANote(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "echo broken; exit 1"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := talk(t, s, "hello")
	if res["started"] != true {
		t.Fatalf("floor.talk = %v", res)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		notes := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.notes"}`)[0])
		if strings.Contains(stringsOf(notes["notes"]), "ended before it was ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no note for a foreman that died: %v", notes)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.talkMu.Lock()
	left, running := len(s.talk.lines), s.talk.running
	s.talkMu.Unlock()
	if left != 0 || running {
		t.Fatalf("the queue kept %d lines, running=%v, want none and stopped", left, running)
	}
}

func TestWordsWaitWhileTheForemanIsBlocked(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := talk(t, s, "WORD-FIRST")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	gateIdle(t, s, id)
	waitFile(t, in, "WORD-FIRST", 10*time.Second)
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateBlocked, Source: proto.SrcGate,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "ls", Deadline: nowSeconds() + 9999},
	})
	talk(t, s, "WORD-SECOND")
	time.Sleep(600 * time.Millisecond)
	if b, _ := os.ReadFile(in); strings.Contains(string(b), "WORD-SECOND") {
		t.Fatal("words went into a foreman that waits on a question")
	}
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcGate,
	})
	waitFile(t, in, "WORD-SECOND", 10*time.Second)
}

func TestTalkNeedsText(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"floor.talk","text":"  "}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("want bad_request, got %+v", got[0])
	}
}

func TestTalkWithNoDefaultHarnessSaysHowToSetOne(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"floor.talk","text":"hello"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "no default harness") {
		t.Fatalf("want the no default harness refusal, got %+v", got[0])
	}
}

func TestThePastedPageIsTheSkillPage(t *testing.T) {
	text := foremanPage()
	if !strings.Contains(text, skills.Foreman) || !strings.HasPrefix(text, skills.FloorHeader) {
		t.Fatal("the pasted page is not the header and the skill page")
	}
}

// stringsOf flattens a JSON list of notes into one string.
func stringsOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestOnlyTheOperatorTalksToTheForeman(t *testing.T) {
	fakeForeman(t)
	s := newTestServer(t)
	// A connection that says hello as a pane is a pane too.
	got := roundTrip(t, s, `{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}`,
		`{"id":"1","cmd":"floor.talk","text":"hello"}`)
	if got[1].OK || got[1].Error == nil || got[1].Error.Code != proto.ErrUnauthorized {
		t.Fatalf("floor.talk after a pane hello = %+v, want unauthorized", got[1])
	}
	for _, f := range []*peerFacts{
		{checked: true, pane: true, paneID: "w1:p1"},
		{checked: true, unknown: true},
		{checked: true, plugin: "p"},
	} {
		got := roundTripFacts(t, s, f, `{"id":"1","cmd":"floor.talk","text":"hello"}`)
		if got[0].OK || got[0].Error == nil || got[0].Error.Message != TalkRefusal {
			t.Fatalf("floor.talk from %+v = %+v, want the refusal", f, got[0])
		}
	}
	if n := len(foremen(t, s)); n != 0 {
		t.Fatalf("a refused talk started %d foremen", n)
	}
}

// A harness that keeps drawing while it is idle still gets its page and
// the words.
func TestAForemanThatKeepsDrawingWhileIdleStillGetsThePage(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := filepath.Join(t.TempDir(), "in")
	script := `printf '\033[?2004h'; echo ready; (while :; do printf .; sleep 0.2; done) & ` +
		`while IFS= read -r l; do printf '%s\n' "$l" >> '` + in + `'; done`
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", script}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := talk(t, s, "WORD-TICK")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	gateIdle(t, s, id)
	time.Sleep(500 * time.Millisecond)
	got := waitFile(t, in, "WORD-TICK", 10*time.Second)
	if strings.Index(got, pageMark) > strings.Index(got, "WORD-TICK") {
		t.Fatalf("the page did not come first:\n%s", got)
	}
}

// Words with line breaks go in as one line, so one talk is one prompt.
func TestTalkSendsItsWordsAsOneLine(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := talk(t, s, "WORD-A\nWORD-B\r\n  WORD-C")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	gateIdle(t, s, id)
	got := waitFile(t, in, "WORD-C", 10*time.Second)
	if !strings.Contains(got, "WORD-A WORD-B WORD-C\n") {
		t.Fatalf("the words did not go as one line:\n%q", got)
	}
}

// The server finds the foreman by the id it started, never by the label.
// A pane labelled foreman that the server did not start gets no words.
func TestAPaneLabelledForemanIsNotTheForeman(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeForeman(t)
	s := newTestServer(t)
	fake := result(t, roundTrip(t, s, `{"id":"1","cmd":"pane.create","kind":"pty","cwd":"/","label":"foreman",`+
		`"cmd_argv":["sh","-c","exec cat"]}`)[0])
	fakeID, _ := fake["pane"].(string)
	t.Cleanup(func() { closePane(t, s, fakeID) })
	res := talk(t, s, "hello")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if id == fakeID || res["started"] != true {
		t.Fatalf("floor.talk = %v, want a foreman of its own, not %s", res, fakeID)
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, foremanFile))
	if err != nil || !strings.Contains(string(b), `"`+id+`"`) {
		t.Fatalf("the data dir does not name the foreman %s: %s %v", id, b, err)
	}
	if again := talk(t, s, "again"); again["pane"] != id {
		t.Fatalf("the second talk went to %v, want %s", again["pane"], id)
	}
}

// A new server reads the foreman's id from the data dir.
func TestTheForemanIdOutlivesTheServerObject(t *testing.T) {
	s := newTestServer(t)
	s.talkMu.Lock()
	s.setForeman("w1:p7")
	s.talk.loaded = false
	s.talk.foreman = ""
	got := s.trackedForeman()
	s.talkMu.Unlock()
	if got != "w1:p7" {
		t.Fatalf("tracked foreman = %q, want w1:p7", got)
	}
}

// A pane may not take the foreman's label by create, split, fork or
// rename. The operator may.
func TestOnlyTheOperatorLabelsAPaneForeman(t *testing.T) {
	s := newTestServer(t)
	pane := &peerFacts{checked: true, pane: true, paneID: "w1:p1"}
	for _, line := range []string{
		`{"id":"1","cmd":"pane.create","kind":"pty","cwd":"/","label":"foreman","cmd_argv":["true"]}`,
		`{"id":"1","cmd":"pane.split","kind":"pty","cwd":"/","label":" Foreman ","cmd_argv":["true"]}`,
		`{"id":"1","cmd":"pane.fork","pane":"w1:p1","label":"foreman"}`,
		`{"id":"1","cmd":"pane.rename","pane":"w1:p1","label":"foreman"}`,
	} {
		got := roundTripFacts(t, s, pane, line)
		if got[0].OK || got[0].Error == nil || got[0].Error.Message != ForemanLabelRefusal {
			t.Fatalf("%s from a pane = %+v, want the refusal", line, got[0])
		}
	}
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"w9:p9","label":"foreman"}`)
	if got[0].Error != nil && got[0].Error.Message == ForemanLabelRefusal {
		t.Fatal("the operator was refused the label")
	}
}

// floor.foreman starts the foreman with a named harness and pages it, and
// refuses a second while the first runs.
func TestFloorForemanStartsOneAndRefusesASecond(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.foreman","harness":"fakeh"}`)[0])
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if id == "" || res["label"] != "foreman" {
		t.Fatalf("floor.foreman = %v", res)
	}
	got := roundTrip(t, s, `{"id":"1","cmd":"floor.foreman","harness":"fakeh"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "runs already") {
		t.Fatalf("a second floor.foreman = %+v, want refused", got[0])
	}
	gateIdle(t, s, id)
	waitFile(t, in, pageMark, 10*time.Second)
	if talk(t, s, "hello")["pane"] != id {
		t.Fatal("talk did not go to the foreman floor.foreman started")
	}
}

// The queue holds at most maxTalkQueue sentences.
func TestTheTalkQueueIsBounded(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeForeman(t)
	s := newTestServer(t)
	var id string
	for i := 0; i < maxTalkQueue; i++ {
		res := talk(t, s, "WORD")
		id, _ = res["pane"].(string)
	}
	t.Cleanup(func() { closePane(t, s, id) })
	got := roundTrip(t, s, `{"id":"1","cmd":"floor.talk","text":"one too many"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "sentences waiting") {
		t.Fatalf("talk past the bound = %+v, want refused", got[0])
	}
}

func TestTalkTextDropsControlCharacters(t *testing.T) {
	if got := talkText("a\x1b[31mb\x07 c\td\x7f\ne"); got != "a[31mb c d e" {
		t.Fatalf("talkText = %q", got)
	}
}

func TestThePageHoldsNoEscape(t *testing.T) {
	if strings.ContainsRune(foremanPage(), 0x1b) {
		t.Fatal("the page holds an escape byte, which would end the paste early")
	}
}

// The foreman works outside the coppice data dir, which the gate guards.
func TestTheForemanDirIsUnderTheStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got := ForemanDir(); got != "/state/coppice/foreman" {
		t.Fatalf("ForemanDir = %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	home, _ := os.UserHomeDir()
	if got := ForemanDir(); got != filepath.Join(home, ".local", "state", "coppice", "foreman") {
		t.Fatalf("ForemanDir = %q", got)
	}
}

// Resuming the ended foreman's record makes the new pane the tracked
// foreman, so the next talk goes to it and no second foreman starts.
func TestResumingTheEndedForemanKeepsItTheForeman(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := talk(t, s, "EXIT-NOW")
	old, _ := res["pane"].(string)
	gateIdle(t, s, old)
	waitFile(t, in, "EXIT-NOW", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for len(foremen(t, s)) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the foreman never ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+old+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	s.talkMu.Lock()
	tracked := s.trackedForeman()
	s.talkMu.Unlock()
	if tracked != id {
		t.Fatalf("tracked foreman = %q after resume, want the resumed %q", tracked, id)
	}
	gateIdle(t, s, id)
	again := talk(t, s, "are you back")
	if again["pane"] != id || again["started"] != false {
		t.Fatalf("the talk after resume = %v, want it sent to %s with no new foreman", again, id)
	}
	if n := len(foremen(t, s)); n != 1 {
		t.Fatalf("%d panes labelled foreman, want 1", n)
	}
}

// A pane may not take the foreman's label by resuming a record that has
// it. The operator may.
func TestAPaneCannotResumeARecordLabelledForeman(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty","label":"foreman"}`)
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)
	pane := &peerFacts{checked: true, pane: true, paneID: "w1:p9"}
	got = roundTripFacts(t, s, pane, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Message != ForemanLabelRefusal {
		t.Fatalf("pane.resume of a foreman record from a pane = %+v, want the refusal", got[0])
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("the operator's resume failed: %+v", got[0].Error)
	}
	newID, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, newID)
}

// floor.facts names the tracked foreman while it runs, and nothing
// before one starts. No config key names it.
func TestFloorFactsNamesTheTrackedForeman(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	facts := func() map[string]any {
		return result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	}
	if f := facts(); f["foreman"] != nil {
		t.Fatalf("foreman before one starts = %v, want none", f["foreman"])
	}
	res := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.foreman","harness":"fakeh"}`)[0])
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	fm, _ := facts()["foreman"].(map[string]any)
	if fm == nil || fm["pane"] != id || fm["label"] != DefaultForemanLabel {
		t.Fatalf("foreman = %v, want pane %s labelled foreman", fm, id)
	}
	if b, _ := os.ReadFile(config.Path()); strings.Contains(string(b), "\nforeman") {
		t.Fatalf("the config file names a foreman: %s", b)
	}
}

// When a talk already started a new foreman, resuming the old foreman's
// record starts a pane that is not the foreman, and its label says so.
func TestResumingAnOldForemanAfterANewOneStartsIsNotTheForeman(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeForeman(t)
	s := newTestServer(t)
	s.foremanWait = 200 * time.Millisecond
	res := talk(t, s, "EXIT-NOW")
	old, _ := res["pane"].(string)
	gateIdle(t, s, old)
	waitFile(t, in, "EXIT-NOW", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for len(foremen(t, s)) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the foreman never ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	fresh, _ := talk(t, s, "are you back")["pane"].(string)
	t.Cleanup(func() { closePane(t, s, fresh) })
	got := roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+old+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if l := rowFor(t, listPanes(t, s), id)["label"]; l != "foreman-old" {
		t.Fatalf("the resumed old foreman is labelled %v, want foreman-old", l)
	}
	s.talkMu.Lock()
	tracked := s.trackedForeman()
	s.talkMu.Unlock()
	if tracked != fresh {
		t.Fatalf("tracked foreman = %q, want the new %q", tracked, fresh)
	}
}
