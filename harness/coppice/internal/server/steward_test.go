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

// fakeSteward writes a config whose default harness, fakeh, is a shell
// script. It turns bracketed paste on, draws one line, and writes each
// line it reads to the returned file. A line that holds EXIT-NOW ends it.
// No real harness ever runs.
func fakeSteward(t *testing.T) string {
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
			t.Fatalf("%q never reached the steward. It read:\n%s", want, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// wardenIdle marks pane id idle as a warden hook does at the end of a turn,
// once the pane drew its first line, so a test need not wait for the
// screen to go quiet.
func wardenIdle(t *testing.T, s *Server, id string) {
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
		State: proto.StateIdle, Source: proto.SrcWarden,
	})
}

// stewards lists the live top panes labelled steward.
func stewards(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range listPanes(t, s) {
		if r["label"] == "steward" {
			out = append(out, r)
		}
	}
	return out
}

// pageMark is a line of the steward page that the tests look for.
const pageMark = "## Your commands"

func TestTheFirstTalkStartsTheStewardAndPagesItBeforeTheWords(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	res := talk(t, s, "hello steward")
	id, _ := res["pane"].(string)
	if id == "" || res["started"] != true || res["label"] != "steward" {
		t.Fatalf("floor.talk = %v, want a started steward", res)
	}
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	dir := s.stewardDir()
	if row["label"] != "steward" || row["harness"] != "fakeh" || row["cwd"] != dir {
		t.Fatalf("row = %v, want label steward, harness fakeh, cwd %s", row, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the steward dir was not made: %v", err)
	}
	// Real timing: the screen goes quiet, the page goes in, the page's
	// turn ends, then the words go in.
	got := waitFile(t, in, "hello steward", 40*time.Second)
	page := strings.Index(got, pageMark)
	words := strings.Index(got, "hello steward")
	if page < 0 || page > words {
		t.Fatalf("the page did not come first:\n%s", got)
	}
	if !strings.Contains(got, "\x1b[200~") || !strings.Contains(got, "\x1b[201~") {
		t.Fatalf("the page was not sent as one paste:\n%q", got)
	}
	for _, d := range loadRecentDirs(s.cfg.DataDir) {
		if d == dir {
			t.Fatalf("the steward dir joined the recent projects: %v", loadRecentDirs(s.cfg.DataDir))
		}
	}
}

func TestTalksWhileTheStewardStartsQueueInOrderBehindOne(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	first := talk(t, s, "WORD-ONE")
	second := talk(t, s, "WORD-TWO")
	third := talk(t, s, "WORD-THREE")
	id, _ := first["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if second["pane"] != id || third["pane"] != id {
		t.Fatalf("the talks went to different panes: %v %v %v", first, second, third)
	}
	if second["started"] != false || third["started"] != false {
		t.Fatalf("a queued talk started a steward: %v %v", second, third)
	}
	if n := len(stewards(t, s)); n != 1 {
		t.Fatalf("%d stewards run, want one", n)
	}
	wardenIdle(t, s, id)
	got := waitFile(t, in, "WORD-THREE", 10*time.Second)
	at := func(w string) int { return strings.Index(got, w) }
	if !(at(pageMark) < at("WORD-ONE") && at("WORD-ONE") < at("WORD-TWO") && at("WORD-TWO") < at("WORD-THREE")) {
		t.Fatalf("the lines came out of order:\n%s", got)
	}
}

func TestADeadStewardIsReplacedOnTheNextTalkWithANote(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	res := talk(t, s, "EXIT-NOW")
	old, _ := res["pane"].(string)
	wardenIdle(t, s, old)
	waitFile(t, in, "EXIT-NOW", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for len(stewards(t, s)) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the steward never ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	res = talk(t, s, "are you back")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if id == "" || id == old || res["started"] != true {
		t.Fatalf("floor.talk = %v, want a new steward", res)
	}
	note, _ := res["note"].(string)
	if !strings.Contains(note, "ended") || strings.Contains(note, "\n") {
		t.Fatalf("note = %q, want one line that says the old one ended", note)
	}
	notes := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.notes"}`)[0])
	if !strings.Contains(stringsOf(notes["notes"]), note) {
		t.Fatalf("the floor did not get the note: %v", notes)
	}
	wardenIdle(t, s, id)
	waitFile(t, in, "are you back", 10*time.Second)
}

func TestAStewardThatDiesBeforeItIsReadyDropsTheWordsWithANote(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "echo broken; exit 1"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
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
			t.Fatalf("no note for a steward that died: %v", notes)
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

func TestWordsWaitWhileTheStewardIsBlocked(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	res := talk(t, s, "WORD-FIRST")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	wardenIdle(t, s, id)
	waitFile(t, in, "WORD-FIRST", 10*time.Second)
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateBlocked, Source: proto.SrcWarden,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "ls", Deadline: nowSeconds() + 9999},
	})
	talk(t, s, "WORD-SECOND")
	time.Sleep(600 * time.Millisecond)
	if b, _ := os.ReadFile(in); strings.Contains(string(b), "WORD-SECOND") {
		t.Fatal("words went into a steward that waits on a question")
	}
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcWarden,
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
	text := stewardPage()
	if !strings.Contains(text, skills.Steward) || !strings.HasPrefix(text, skills.FloorHeader) {
		t.Fatal("the pasted page is not the header and the skill page")
	}
}

// stringsOf flattens a JSON list of notes into one string.
func stringsOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestOnlyTheOperatorTalksToTheSteward(t *testing.T) {
	fakeSteward(t)
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
	if n := len(stewards(t, s)); n != 0 {
		t.Fatalf("a refused talk started %d stewards", n)
	}
}

// A harness that keeps drawing while it is idle still gets its page and
// the words.
func TestAStewardThatKeepsDrawingWhileIdleStillGetsThePage(t *testing.T) {
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
	s.stewardWait = 200 * time.Millisecond
	res := talk(t, s, "WORD-TICK")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	// The hook's idle comes after the process fact says working, as it
	// does for a real harness. A warden idle that came first would be
	// replaced by the tick's working a moment later, and that pane never
	// reads idle again while it draws.
	deadline := time.Now().Add(10 * time.Second)
	for {
		lp, ok := s.Live(id)
		if ok {
			lp.procMu.Lock()
			st := lp.procState
			lp.procMu.Unlock()
			if st == proto.StateWorking {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the process fact never said working")
		}
		time.Sleep(20 * time.Millisecond)
	}
	wardenIdle(t, s, id)
	time.Sleep(500 * time.Millisecond)
	got := waitFile(t, in, "WORD-TICK", 10*time.Second)
	if strings.Index(got, pageMark) > strings.Index(got, "WORD-TICK") {
		t.Fatalf("the page did not come first:\n%s", got)
	}
}

// Words with line breaks go in as one line, so one talk is one prompt.
func TestTalkSendsItsWordsAsOneLine(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	res := talk(t, s, "WORD-A\nWORD-B\r\n  WORD-C")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	wardenIdle(t, s, id)
	got := waitFile(t, in, "WORD-C", 10*time.Second)
	if !strings.Contains(got, "WORD-A WORD-B WORD-C\n") {
		t.Fatalf("the words did not go as one line:\n%q", got)
	}
}

// The server finds the steward by the id it started, never by the label.
// A pane labelled steward that the server did not start gets no words.
func TestAPaneLabelledStewardIsNotTheSteward(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeSteward(t)
	s := newTestServer(t)
	fake := result(t, roundTrip(t, s, `{"id":"1","cmd":"pane.create","kind":"pty","cwd":"/","label":"steward",`+
		`"cmd_argv":["sh","-c","exec cat"]}`)[0])
	fakeID, _ := fake["pane"].(string)
	t.Cleanup(func() { closePane(t, s, fakeID) })
	res := talk(t, s, "hello")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if id == fakeID || res["started"] != true {
		t.Fatalf("floor.talk = %v, want a steward of its own, not %s", res, fakeID)
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, stewardFile))
	if err != nil || !strings.Contains(string(b), `"`+id+`"`) {
		t.Fatalf("the data dir does not name the steward %s: %s %v", id, b, err)
	}
	if again := talk(t, s, "again"); again["pane"] != id {
		t.Fatalf("the second talk went to %v, want %s", again["pane"], id)
	}
}

// A new server reads the steward's id from the data dir.
func TestTheStewardIdOutlivesTheServerObject(t *testing.T) {
	s := newTestServer(t)
	s.talkMu.Lock()
	s.setSteward("w1:p7")
	s.talk.loaded = false
	s.talk.steward = ""
	got := s.trackedSteward()
	s.talkMu.Unlock()
	if got != "w1:p7" {
		t.Fatalf("tracked steward = %q, want w1:p7", got)
	}
}

// A pane may not take the steward's label by create, split, fork or
// rename. The operator may.
func TestOnlyTheOperatorLabelsAPaneSteward(t *testing.T) {
	s := newTestServer(t)
	pane := &peerFacts{checked: true, pane: true, paneID: "w1:p1"}
	for _, line := range []string{
		`{"id":"1","cmd":"pane.create","kind":"pty","cwd":"/","label":"steward","cmd_argv":["true"]}`,
		`{"id":"1","cmd":"pane.split","kind":"pty","cwd":"/","label":" Steward ","cmd_argv":["true"]}`,
		`{"id":"1","cmd":"pane.fork","pane":"w1:p1","label":"steward"}`,
		`{"id":"1","cmd":"pane.rename","pane":"w1:p1","label":"steward"}`,
	} {
		got := roundTripFacts(t, s, pane, line)
		if got[0].OK || got[0].Error == nil || got[0].Error.Message != StewardLabelRefusal {
			t.Fatalf("%s from a pane = %+v, want the refusal", line, got[0])
		}
	}
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.rename","pane":"w9:p9","label":"steward"}`)
	if got[0].Error != nil && got[0].Error.Message == StewardLabelRefusal {
		t.Fatal("the operator was refused the label")
	}
}

// floor.steward starts the steward with a named harness and pages it, and
// refuses a second while the first runs.
func TestFloorStewardStartsOneAndRefusesASecond(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	res := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.steward","harness":"fakeh"}`)[0])
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	if id == "" || res["label"] != "steward" {
		t.Fatalf("floor.steward = %v", res)
	}
	got := roundTrip(t, s, `{"id":"1","cmd":"floor.steward","harness":"fakeh"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "runs already") {
		t.Fatalf("a second floor.steward = %+v, want refused", got[0])
	}
	wardenIdle(t, s, id)
	waitFile(t, in, pageMark, 10*time.Second)
	if talk(t, s, "hello")["pane"] != id {
		t.Fatal("talk did not go to the steward floor.steward started")
	}
}

// The queue holds at most maxTalkQueue sentences.
func TestTheTalkQueueIsBounded(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeSteward(t)
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
	if strings.ContainsRune(stewardPage(), 0x1b) {
		t.Fatal("the page holds an escape byte, which would end the paste early")
	}
}

// The steward works outside the coppice dirs, which the warden and the
// fence guard.
func TestTheStewardDirIsBesideTheCoppiceStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got := StewardDir(); got != "/state/coppice-steward" {
		t.Fatalf("StewardDir = %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	home, _ := os.UserHomeDir()
	if got := StewardDir(); got != filepath.Join(home, ".local", "state", "coppice-steward") {
		t.Fatalf("StewardDir = %q", got)
	}
}

// `daisugi fence` shuts these as secrets (clients/go/internal/fence
// CoppiceDirs and DataDirs, golden here): a workspace that is, holds or sits
// under one is refused. The default steward dir must be none of them, in
// the default layout and under XDG overrides.
func TestTheDefaultStewardDirReachesNoFenceSecret(t *testing.T) {
	reaches := func(a, b string) bool {
		if a == b {
			return true
		}
		return strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
	}
	home := "/home/u"
	for _, env := range []map[string]string{
		{}, {"XDG_STATE_HOME": "/xs", "XDG_CONFIG_HOME": "/xc", "XDG_DATA_HOME": "/xd"},
	} {
		def := func(k, d string) string {
			if v := env[k]; v != "" {
				return v
			}
			return home + d
		}
		secrets := []string{
			def("XDG_CONFIG_HOME", "/.config") + "/coppice", def("XDG_STATE_HOME", "/.local/state") + "/coppice",
			def("XDG_DATA_HOME", "/.local/share") + "/coppice", home + "/.opendaisugi",
			def("XDG_DATA_HOME", "/.local/share") + "/opendaisugi",
		}
		for k, v := range env {
			t.Setenv(k, v)
		}
		if _, ok := env["XDG_STATE_HOME"]; !ok {
			t.Setenv("XDG_STATE_HOME", "")
			t.Setenv("HOME", home)
		}
		got := StewardDir()
		for _, sec := range secrets {
			if reaches(got, sec) || reaches(sec, got) {
				t.Errorf("env %v: steward dir %s reaches the fence secret %s", env, got, sec)
			}
		}
	}
}

// Resuming the ended steward's record makes the new pane the tracked
// steward, so the next talk goes to it and no second steward starts.
func TestResumingTheEndedStewardKeepsItTheSteward(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	res := talk(t, s, "EXIT-NOW")
	old, _ := res["pane"].(string)
	wardenIdle(t, s, old)
	waitFile(t, in, "EXIT-NOW", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for len(stewards(t, s)) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the steward never ended")
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
	tracked := s.trackedSteward()
	s.talkMu.Unlock()
	if tracked != id {
		t.Fatalf("tracked steward = %q after resume, want the resumed %q", tracked, id)
	}
	wardenIdle(t, s, id)
	again := talk(t, s, "are you back")
	if again["pane"] != id || again["started"] != false {
		t.Fatalf("the talk after resume = %v, want it sent to %s with no new steward", again, id)
	}
	if n := len(stewards(t, s)); n != 1 {
		t.Fatalf("%d panes labelled steward, want 1", n)
	}
}

// A pane may not take the steward's label by resuming a record that has
// it. The operator may.
func TestAPaneCannotResumeARecordLabelledSteward(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty","label":"steward"}`)
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)
	pane := &peerFacts{checked: true, pane: true, paneID: "w1:p9"}
	got = roundTripFacts(t, s, pane, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Message != StewardLabelRefusal {
		t.Fatalf("pane.resume of a steward record from a pane = %+v, want the refusal", got[0])
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("the operator's resume failed: %+v", got[0].Error)
	}
	newID, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, newID)
}

// floor.facts names the tracked steward while it runs, and nothing
// before one starts. No config key names it.
func TestFloorFactsNamesTheTrackedSteward(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	facts := func() map[string]any {
		return result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	}
	if f := facts(); f["steward"] != nil {
		t.Fatalf("steward before one starts = %v, want none", f["steward"])
	}
	res := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.steward","harness":"fakeh"}`)[0])
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	fm, _ := facts()["steward"].(map[string]any)
	if fm == nil || fm["pane"] != id || fm["label"] != DefaultStewardLabel {
		t.Fatalf("steward = %v, want pane %s labelled steward", fm, id)
	}
	if b, _ := os.ReadFile(config.Path()); strings.Contains(string(b), "\nsteward") {
		t.Fatalf("the config file names a steward: %s", b)
	}
}

// When a talk already started a new steward, resuming the old steward's
// record starts a pane that is not the steward, and its label says so.
func TestResumingAnOldStewardAfterANewOneStartsIsNotTheSteward(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	s.stewardWait = 200 * time.Millisecond
	res := talk(t, s, "EXIT-NOW")
	old, _ := res["pane"].(string)
	wardenIdle(t, s, old)
	waitFile(t, in, "EXIT-NOW", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for len(stewards(t, s)) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the steward never ended")
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
	if l := rowFor(t, listPanes(t, s), id)["label"]; l != "steward-old" {
		t.Fatalf("the resumed old steward is labelled %v, want steward-old", l)
	}
	s.talkMu.Lock()
	tracked := s.trackedSteward()
	s.talkMu.Unlock()
	if tracked != fresh {
		t.Fatalf("tracked steward = %q, want the new %q", tracked, fresh)
	}
}

// A page's turn that starts and ends between two polls of the talk queue
// still counts as the turn: the words go after it at once, not after
// stewardWait (30 s). A fake steward whose hook says working and then idle
// within a millisecond did this in chat/read.jsonl, and the queue waited
// the whole 30 s for a turn it had missed.
func TestAPageTurnShorterThanAPollStillLetsTheWordsGo(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := filepath.Join(t.TempDir(), "in")
	script := `stty -echo; printf '\033[?2004h'; echo ready; ` +
		`while IFS= read -r l; do printf '%s\n' "$l" >> '` + in + `'; done`
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", script}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	// The process source reads the pane as quiet from the start, so only
	// the hook's events below make a turn.
	s.clockSkew.Store(int64(2 * processQuiet))
	res := talk(t, s, "WORD-QUICK")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	wardenIdle(t, s, id)
	// The end of the paste comes with the page's Enter.
	waitFile(t, in, "\x1b[201~", 10*time.Second)
	for _, st := range []string{proto.StateWorking, proto.StateIdle} {
		s.ApplyState(id, proto.PaneStateEvent{
			V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
			State: st, Source: proto.SrcWarden,
		})
	}
	waitFile(t, in, "WORD-QUICK", 10*time.Second)
}

// A steward with a warden idle, quiet for 6 s, that redraws a few bytes
// still gets its words (CP-R-56). The redraw used to read as working from
// the process source, and the queue waited for an idle that only a hook or
// 5 more quiet seconds would bring.
func TestAShortRedrawOfAnIdleStewardDoesNotHoldTheWords(t *testing.T) {
	toolchain.RequireOrSkip(t)
	in := fakeSteward(t)
	s := newTestServer(t)
	// The process source reads the pane as quiet from the start.
	s.clockSkew.Store(int64(10 * time.Second))
	res := talk(t, s, "WORD-REDRAW")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	wardenIdle(t, s, id)
	waitFile(t, in, "\x1b[201~", 10*time.Second)
	for _, st := range []string{proto.StateWorking, proto.StateIdle} {
		s.ApplyState(id, proto.PaneStateEvent{
			V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
			State: st, Source: proto.SrcWarden,
		})
	}
	// The page's turn ended. The redraw lands before the queue's next poll.
	lp, _ := s.Live(id)
	// The grid marks a burst by its writes: the steward's output was 6 s ago.
	lp.Grid.StampWriteForTest(time.Now().Add(-6 * time.Second))
	if _, err := lp.Grid.Write([]byte("12:01")); err != nil {
		t.Fatal(err)
	}
	s.clockSkew.Store(0)
	s.processTick(id, lp)
	waitFile(t, in, "WORD-REDRAW", 3*time.Second)
}

// A steward whose page landed, whose Stop hook then said idle, and whose
// screen redraws every 500 ms for 30 s, still gets its words within 12 s of
// that idle (CP-R-57). The process source reads such a screen as working
// the whole time, and the queue used to wait for it to stop.
func TestAStatusLineThatNeverRestsDoesNotHoldTheWords(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := filepath.Join(t.TempDir(), "in")
	script := `stty -echo; printf '\033[?2004h'; echo ready; ` +
		`(sleep 12; i=0; while [ $i -lt 60 ]; do sleep 0.5; printf t; i=$((i+1)); done) & ` +
		`while IFS= read -r l; do printf '%s\n' "$l" >> '` + in + `'; done`
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", script}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	res := talk(t, s, "WORD-STATUS")
	id, _ := res["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	// The page goes in once the screen is quiet (about 5 s). Its turn ends:
	// the Stop hook says idle the way the real hook does, as source headless
	// (hook.py). The screen then goes quiet for good measure and, from 12 s,
	// redraws every 500 ms for 30 s. The process source flips to working,
	// the hook idle stands, and the words must not wait for the redraw to
	// end (CP-R-57).
	waitFile(t, in, "\x1b[201~", 15*time.Second)
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "fakeh", Pane: &id,
		State: proto.StateIdle, Source: proto.SrcHeadless, Detail: "session stop",
	})
	waitFile(t, in, "WORD-STATUS", 20*time.Second)
}
