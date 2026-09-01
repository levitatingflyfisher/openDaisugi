package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func newPaneServer(t *testing.T) *Server {
	t.Helper()
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	return s
}

func result(t *testing.T, r proto.Response) map[string]any {
	t.Helper()
	if !r.OK {
		t.Fatalf("request failed: %+v", r.Error)
	}
	b, _ := json.Marshal(r.Result)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPaneCreateMakesAWorkspaceAndTabWhenNoneExist(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty","label":"first"}`)
	m := result(t, got[0])
	if m["pane"] != "w1:p1" || m["workspace"] != "w1" || m["tab"] != "w1:t1" {
		t.Fatalf("pane.create result = %v, want w1 / w1:t1 / w1:p1", m)
	}
}

// pane.create
// after Close used to answer pane_closed with server-scoped wording, an
// overload of a code that otherwise always names one particular pane. The
// wire now has a code of its own for this case.
//
// handlePaneCreate is called directly, not through roundTrip: ServeConn's
// own connection-level guard (c.Dead() || s.isClosed(), checked before a
// request is even dispatched) refuses a brand new connection to an
// already-closed server before handlePaneCreate's own check ever runs.
// That guard is the ordinary case; handlePaneCreate's is the narrow race
// window its own doc comment describes, a request already dispatched to
// its own goroutine when Close begins - which this reaches directly.
func TestPaneCreateAfterCloseAnswersServerClosed(t *testing.T) {
	s := newPaneServer(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var req proto.Request
	if err := json.Unmarshal([]byte(
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh"],"kind":"pty"}`), &req); err != nil {
		t.Fatal(err)
	}
	resp := s.handlePaneCreate(nil, &req)
	if resp.OK || resp.Error.Code != proto.ErrServerClosed {
		t.Fatalf("got %+v, want server_closed", resp)
	}
}

func TestPaneCreateWithoutCwdIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cmd_argv":["sh"]}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "cwd") {
		t.Fatalf("error %q does not name the missing field", got[0].Error.Message)
	}
}

func TestHeadlessPaneWithoutHarnessIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "harness") {
		t.Fatalf("error %q does not name the missing field", got[0].Error.Message)
	}
}

func TestSpawnFailureIsSpawnFailedNotInternal(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["coppice-no-such-binary"],"kind":"pty"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrSpawnFailed {
		t.Fatalf("got %+v, want spawn_failed", got[0])
	}
}

func TestUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.read","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

func TestClosedPaneRejectsInputWithPaneClosed(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.close","pane":"w1:p1"}`,
		`{"id":"3","cmd":"pane.send_text","pane":"w1:p1","text":"hi"}`)
	if !got[1].OK {
		t.Fatalf("pane.close failed: %+v", got[1].Error)
	}
	if got[2].OK || got[2].Error.Code != proto.ErrPaneClosed {
		t.Fatalf("got %+v, want pane_closed", got[2])
	}
}

func TestPaneRunSendsTheCommandAndAnEnter(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty","cols":60,"rows":10}`,
		`{"id":"2","cmd":"pane.run","pane":"w1:p1","line":"echo marker-9c1"}`,
		`{"id":"3","cmd":"pane.wait_output","pane":"w1:p1","contains":"marker-9c1","timeout_ms":5000}`,
		`{"id":"4","cmd":"pane.read","pane":"w1:p1","source":"visible"}`)
	for i, r := range got {
		if !r.OK {
			t.Fatalf("request %d failed: %+v", i+1, r.Error)
		}
	}
	m := result(t, got[3])
	text, _ := m["text"].(string)
	if !strings.Contains(text, "marker-9c1") {
		t.Fatalf("pane.read = %q, want it to contain marker-9c1", text)
	}
}

func TestWaitOutputTimesOutWithTheTimeoutCode(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"never-appears","timeout_ms":300}`)
	if got[1].OK || got[1].Error.Code != proto.ErrTimeout {
		t.Fatalf("got %+v, want timeout", got[1])
	}
}

func TestSendKeysTranslatesNamedKeys(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty","cols":60,"rows":10}`,
		`{"id":"2","cmd":"pane.send_text","pane":"w1:p1","text":"echo keyed-4f2","enter":false}`,
		`{"id":"3","cmd":"pane.send_keys","pane":"w1:p1","keys":["enter"]}`,
		`{"id":"4","cmd":"pane.wait_output","pane":"w1:p1","contains":"keyed-4f2","timeout_ms":5000}`)
	for i, r := range got {
		if !r.OK {
			t.Fatalf("request %d failed: %+v", i+1, r.Error)
		}
	}
}

func TestSendKeysRejectsAnUnknownKeyName(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.send_keys","pane":"w1:p1","keys":["hyperspace"]}`)
	if got[1].OK || got[1].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[1])
	}
	if !strings.Contains(got[1].Error.Message, "enter") {
		t.Fatalf("error %q does not list a key that works", got[1].Error.Message)
	}
}

// namedKeys gains
// f1 through f12. This writes testdata/keys.json as the sorted named-key
// vocabulary and fails if the file on disk already differs, so the file -
// not this test's own source - is the one list a client, or the parity
// Python suite, reads rather than re-deriving it by hand. Regenerate with
// COPPICE_UPDATE_GOLDEN=1.
func TestNamedKeysVocabularyMatchesTestdataKeysJSON(t *testing.T) {
	out := make([]string, 0, len(namedKeys))
	for k := range namedKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	path := filepath.Join("..", "..", "testdata", "keys.json")
	if os.Getenv("COPPICE_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v. Create it with COPPICE_UPDATE_GOLDEN=1 go test ./internal/server/ "+
			"-run TestNamedKeysVocabularyMatchesTestdataKeysJSON", err)
	}
	if string(got) != string(want) {
		t.Fatalf("namedKeys vocabulary differs from testdata/keys.json.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestPaneListReportsKindAndState(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty","label":"one"}`,
		`{"id":"2","cmd":"pane.list"}`)
	m := result(t, got[1])
	panes, _ := m["panes"].([]any)
	if len(panes) != 1 {
		t.Fatalf("pane.list returned %d panes, want 1", len(panes))
	}
	p, _ := panes[0].(map[string]any)
	if p["kind"] != "pty" || p["label"] != "one" {
		t.Fatalf("pane entry = %v, want kind pty and label one", p)
	}
	// A fresh pty pane has one fact from the start: its process has
	// written nothing yet, so the process source says idle. unknown is for
	// a pane with no source at all, and a live pane always has this one.
	if p["state"] != "idle" || p["source"] != "process" {
		t.Fatalf("a fresh pane reports state %v from %v, want idle from process", p["state"], p["source"])
	}
}

func TestProcessExitMarksThePaneDoneFromTheProcessSource(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 3"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.list"}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	m := result(t, got[2])
	panes, _ := m["panes"].([]any)
	p, _ := panes[0].(map[string]any)
	if p["state"] != "done" || p["source"] != "process" {
		t.Fatalf("after exit: state=%v source=%v, want done / process", p["state"], p["source"])
	}
	if p["exit_code"] != float64(3) {
		t.Fatalf("exit_code = %v, want 3", p["exit_code"])
	}
}

// PTY.Done() fires the instant the direct
// child exits, which can be before the reader goroutine has drained the
// child's last bytes into the grid. watchExit must wait for Drained() too
// (bounded) before posting the done state, or a fast-exiting child's own
// output can lose the race against the state event and never show up here.
// No sleep in this test: pane.wait_output's own bounded poll is what proves
// the ordering, not a fixed delay guessed to be long enough.
//
// pane.read itself is out of reach here: it goes through livePane, which
// refuses a closed pane by design (TestClosedPaneRejectsInputWithPaneClosed
// covers that refusal). This reads the grid directly instead, the same
// escape hatch pane.read --source visible (or the grid) uses: watchExit
// never closes the grid, only handleClose does, so it is
// still there to read once the pane is done.
func TestDoneWaitsForTheGridToDrainBeforeReporting(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","printf hello; exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	lp, ok := s.Live("w1:p1")
	if !ok {
		t.Fatal("the pane's grid is gone")
	}
	text, err := lp.Grid.Read(pane.ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "hello") {
		t.Fatalf("grid once done = %q, want it to contain hello", text)
	}
}

// New wires RegisterPaneCommands into itself (Handle
// refuses registration once Serve starts, so a caller cannot be trusted to
// remember a separate call). This uses newTestServer directly, not
// newPaneServer, precisely so it does NOT call RegisterPaneCommands itself -
// otherwise it would pass whether or not New does its own registration.
func TestNewRegistersThePaneVerbs(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.list"}`)
	if !got[0].OK {
		t.Fatalf("pane.list is not registered by New: %+v", got[0].Error)
	}
}

// A pane whose PROCESS
// exited keeps its grid, so its final screen stays readable; only an
// operator pane.close kills, frees the grid, and removes the live entry.
func TestReadWorksOnAPaneWhoseProcessExited(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","printf final; exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.read","pane":"w1:p1","source":"visible"}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	if !got[2].OK {
		t.Fatalf("pane.read on a process-exited pane failed: %+v, want it to still work", got[2].Error)
	}
	m := result(t, got[2])
	text, _ := m["text"].(string)
	if !strings.Contains(text, "final") {
		t.Fatalf("pane.read = %q, want it to contain final", text)
	}
}

// After an operator pane.close, pane.read and pane.wait_output must return
// pane_closed AT ONCE - no polling to a timeout, because there is nothing
// left to poll: close already freed the grid and removed the live entry.
func TestReadAfterCloseIsPaneClosedAtOnce(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.close","pane":"w1:p1"}`)
	if !got[1].OK {
		t.Fatalf("pane.close failed: %+v", got[1].Error)
	}

	start := time.Now()
	got2 := roundTrip(t, s,
		`{"id":"3","cmd":"pane.read","pane":"w1:p1"}`,
		`{"id":"4","cmd":"pane.wait_output","pane":"w1:p1","contains":"x","timeout_ms":2000}`)
	elapsed := time.Since(start)

	if got2[0].OK || got2[0].Error.Code != proto.ErrPaneClosed {
		t.Fatalf("pane.read after close = %+v, want pane_closed", got2[0])
	}
	if got2[1].OK || got2[1].Error.Code != proto.ErrPaneClosed {
		t.Fatalf("pane.wait_output after close = %+v, want pane_closed", got2[1])
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("pane.read + pane.wait_output after close took %v, want under 100ms (no polling to timeout)", elapsed)
	}
}

// Every pane record change routes through
// updatePane/closePane, so LivePane.Info never goes stale against the tree.
// After watchExit fires, s.Live(id) must report Closed and the exit code -
// proving refreshLiveInfo actually ran, not just that the tree write did.
func TestLiveInfoReflectsTheExitAfterWatchExitFires(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 5"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	if _, ok := s.Live("w1:p1"); !ok {
		t.Fatal("the pane's grid should still be live after a process exit; only pane.close removes it")
	}
	info, ok := s.paneInfo("w1:p1")
	if !ok {
		t.Fatal("paneInfo: pane not found")
	}
	if !info.Closed {
		t.Fatal("LivePane.Info.Closed = false after watchExit fired, want true")
	}
	if info.ExitCode == nil || *info.ExitCode != 5 {
		t.Fatalf("LivePane.Info.ExitCode = %v, want 5", info.ExitCode)
	}
}

// An expired ask must read as working everywhere Current is read,
// not just where EffectiveState happens to already be called.
func TestPaneListShowsAnExpiredAskAsWorking(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	paneID := "w1:p1"
	past := nowSeconds() - 10
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: "claude-code", Pane: &paneID,
		State: proto.StateBlocked, Source: proto.SrcHeadless,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm -rf x", Deadline: past},
	})

	got2 := roundTrip(t, s, `{"id":"2","cmd":"pane.list"}`)
	m := result(t, got2[0])
	panes, _ := m["panes"].([]any)
	if len(panes) != 1 {
		t.Fatalf("pane.list returned %d panes, want 1", len(panes))
	}
	p, _ := panes[0].(map[string]any)
	if p["state"] != "working" {
		t.Fatalf("pane.list state = %v, want working (the ask's deadline has passed)", p["state"])
	}
	if _, hasAsk := p["ask"]; hasAsk {
		t.Fatalf("pane.list still carries an ask %v, want it cleared once expired", p["ask"])
	}
}

// pane.list rows
// carry ts and session_id copied from the stored merged event - never a
// receive-time stamp. A live pty pane always has one stored event, the
// process fact it starts with, so both keys are present from the start.
func TestPaneListCarriesTsAndSessionIDFromTheStoredEvent(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	paneID := "w1:p1"

	got2 := roundTrip(t, s, `{"id":"2","cmd":"pane.list"}`)
	m := result(t, got2[0])
	panes, _ := m["panes"].([]any)
	p, _ := panes[0].(map[string]any)
	// A live pty pane's first stored event is the process source's own
	// idle fact, so ts is that event's stamp and session_id is the pane id.
	if p["source"] != "process" {
		t.Fatalf("pane.list source = %v, want the process fact a live pane starts with", p["source"])
	}
	if _, ok := p["ts"].(float64); !ok {
		t.Fatalf("pane.list carries no numeric ts for the process fact: %v", p)
	}
	if p["session_id"] != paneID {
		t.Fatalf("session_id = %v, want the pane id the process fact carries", p["session_id"])
	}

	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: 5678.25, SessionID: "the-session-id", Harness: "claude-code", Pane: &paneID,
		State: proto.StateWorking, Source: proto.SrcHeadless,
	})
	got3 := roundTrip(t, s, `{"id":"3","cmd":"pane.list"}`)
	m3 := result(t, got3[0])
	panes3, _ := m3["panes"].([]any)
	p3, _ := panes3[0].(map[string]any)
	if p3["ts"] != 5678.25 {
		t.Fatalf("ts = %v, want the stored event's own 5678.25", p3["ts"])
	}
	if p3["session_id"] != "the-session-id" {
		t.Fatalf("session_id = %v, want the stored event's own the-session-id", p3["session_id"])
	}
}

func TestWaitOutputForWorkingReturnsAtOnceWhenAnAskHasExpired(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	paneID := "w1:p1"
	past := nowSeconds() - 10
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: "claude-code", Pane: &paneID,
		State: proto.StateBlocked, Source: proto.SrcHeadless,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm -rf x", Deadline: past},
	})

	start := time.Now()
	got2 := roundTrip(t, s,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"working","timeout_ms":5000}`)
	elapsed := time.Since(start)
	if !got2[0].OK {
		t.Fatalf("wait_output for working failed: %+v", got2[0].Error)
	}
	if elapsed > time.Second {
		t.Fatalf("wait_output for working took %v, want it to return at once", elapsed)
	}
}

// watchExit must not write after Server.Close() has released the
// start lock. The child sleeps briefly so it is still running when Close()
// happens, and exits into watchExit's post-drain code shortly after.
func TestWatchExitDoesNotWriteAfterServerClose(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 0.3; exit 0"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	layoutFile := layoutPath(s.cfg.DataDir)
	fi, err := os.Stat(layoutFile)
	if err != nil {
		t.Fatal(err)
	}
	before := fi.ModTime()

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(layoutFile)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(before) {
			t.Fatalf("layout.json changed after Close(): mtime went from %v to %v", before, fi.ModTime())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A headless create no longer refuses
// outright. It resolves the adapter from the registry by harness instead -
// see the two tests below for what an unknown or unbuilt harness gets
// instead of the old blanket refusal.

// The old message pointed at "coppice agent list", which
// lists panes, not adapters. The real list is startPane's own
// adapters.Names(), so the message names it directly instead of a command
// that answers a different question.
func TestHeadlessPaneWithNoHarnessNamesTheRealAdapterList(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "claude") {
		t.Fatalf("error %q does not name a real adapter", got[0].Error.Message)
	}
	if strings.Contains(got[0].Error.Message, "coppice agent list") {
		t.Fatalf("error %q still points at agent list, which lists panes, not adapters",
			got[0].Error.Message)
	}
}

// A harness nothing has ever registered - not even as a design - is a plain
// client mistake: bad_request, naming the harness that was not found.
func TestHeadlessPaneWithUnknownHarnessIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"telepathy"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "telepathy") {
		t.Fatalf("error %q does not name the unknown harness", got[0].Error.Message)
	}
}

// This used to exercise "claude", then "codex", then "sprig" here in turn,
// as each one was built for real - see
// internal/adapters/claude/claude_test.go, internal/adapters/codex/
// codex_test.go and internal/adapters/sprig/sprig_test.go for their own
// coverage. sprig is the last of the four names this test used
// to borrow, so there is no longer a registered-but-unbuilt harness name
// left for this test binary to fail against: pi and opencode's own NotBuilt
// registrations live in their own packages (internal/adapters/pi,
// internal/adapters/opencode), which this test binary never blank-imports -
// see the comment at "headless pane plumbing" below - so a harness name
// nothing here has ever registered at all is now the only honest case left
// to check from a server test, and TestHeadlessPaneWithUnknownHarnessIsBadRequest
// above already covers exactly that path. This test is kept, renamed, as a
// second instance of the same check under a different name, so the intent
// ("an unbuilt/unknown harness never gets a pane that silently does
// nothing" - the dishonest control master spec section 3.5 bans) still has
// its own named test rather than quietly disappearing when sprig shipped.
func TestHeadlessPaneWithAStillUnregisteredHarnessIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"not-a-real-harness"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "not-a-real-harness") {
		t.Fatalf("error %q does not name the unregistered harness", got[0].Error.Message)
	}
}

// A client that disconnects while pane.wait_output is
// polling must not keep that handler polling all the way to its own
// timeout_ms.
func TestWaitOutputStopsPollingWhenTheClientDisconnects(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { s.ServeConn(inR, outW); _ = outW.Close(); close(done) }()

	d := proto.NewDecoder(outR)
	go func() {
		for {
			if _, err := d.Next(); err != nil {
				return
			}
		}
	}()

	if _, err := io.WriteString(inW,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"never-appears","timeout_ms":10000}`+"\n"); err != nil {
		t.Fatal(err)
	}

	// Give the handler a moment to actually start polling before disconnecting.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_ = inW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeConn never returned; wait_output kept polling past the disconnect")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("wait_output took %v to notice the disconnect, want well under its 10s timeout_ms", elapsed)
	}
}

// A bad pane.create must map layout's own error to the right
// closed-enum code by errors.Is, not by sniffing "workspace" out of the error
// string - a tab id that happens to contain that substring must not be
// misreported as no_such_workspace.
func TestPaneCreateNoSuchTabIsNotConfusedWithNoSuchWorkspace(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"workspace.create","cwd":"`+t.TempDir()+`"}`,
		`{"id":"2","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh"],"kind":"pty","workspace":"w1","tab":"w1:workspace-tab-9"}`)
	if !got[0].OK {
		t.Fatalf("workspace.create failed: %+v", got[0].Error)
	}
	if got[1].OK || got[1].Error.Code != proto.ErrNoSuchTab {
		t.Fatalf("got %+v, want no_such_tab", got[1])
	}
}

// No text at all is a mistake worth naming, not a silent
// bare Enter.
func TestSendTextWithoutTextIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.send_text","pane":"w1:p1"}`)
	if got[1].OK || got[1].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[1])
	}
}

// layoutPath must not depend on whether DataDir carries a
// trailing slash.
func TestLayoutPathJoinsCleanly(t *testing.T) {
	got := layoutPath("/tmp/coppice-data/")
	if strings.Contains(got, "//") {
		t.Fatalf("layoutPath(%q) = %q, want no doubled separator", "/tmp/coppice-data/", got)
	}
}

// --- headless pane plumbing -------------------------------------------------
//
// pi and opencode are still NotBuilt; claude, codex and
// sprig are real packages now, but this test binary never
// blank-imports any of the five, so none of them is registered here at all -
// registry.go's own init() no longer registers a placeholder either, now
// that sprig has landed and removed the last one. fakeProc/fakeAdapter
// stand in for a
// real headless harness purely for these tests: a channel the test can send
// pane.Events on, and a forwarding goroutine that owns closing Events() -
// obeying the adapter rule (c) that only the PRODUCER closes that
// channel, Stop() only signals it to.

type fakeProc struct {
	in, out chan pane.Event
	stop    chan struct{}
	// stopCalled closes the instant Stop() is first called, so a test can
	// observe THAT pumpAdapter called it, not just
	// infer it from the stream closing shortly after.
	stopCalled chan struct{}
	once       sync.Once

	mu     sync.Mutex
	sid    string
	hasSID bool
}

func newFakeProc() *fakeProc {
	p := &fakeProc{
		in: make(chan pane.Event), out: make(chan pane.Event),
		stop: make(chan struct{}), stopCalled: make(chan struct{}),
	}
	go func() {
		defer close(p.out)
		for {
			select {
			case ev, ok := <-p.in:
				if !ok {
					return
				}
				select {
				case p.out <- ev:
				case <-p.stop:
					return
				}
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *fakeProc) Prompt(string) error       { return nil }
func (p *fakeProc) Steer(string) error        { return pane.ErrUnsupported }
func (p *fakeProc) WriteStdin([]byte) error   { return nil }
func (p *fakeProc) Events() <-chan pane.Event { return p.out }

func (p *fakeProc) Stop() error {
	p.once.Do(func() {
		close(p.stopCalled)
		close(p.stop)
	})
	return nil
}

func (p *fakeProc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sid, p.hasSID
}

func (p *fakeProc) setSessionID(id string) {
	p.mu.Lock()
	p.sid, p.hasSID = id, true
	p.mu.Unlock()
}

// send blocks until pumpAdapter's range loop takes the event; the tests below
// only ever send from the test goroutine itself, so this never races closeIn.
func (p *fakeProc) send(ev pane.Event) { p.in <- ev }

// closeIn is what a real adapter's own producer goroutine does when its
// underlying process's stdout hits EOF with no explicit EvEnd: stop sending,
// let Events() close on its own.
func (p *fakeProc) closeIn() { close(p.in) }

type fakeAdapter struct {
	name string
	proc *fakeProc
}

func (a *fakeAdapter) Name() string { return a.name }

func (a *fakeAdapter) Start(_ context.Context, _ pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	return a.proc, nil
}

// stubbornProc ignores Stop() entirely and never closes its own Events()
// channel on its own. It exists for one thing: proving pumpAdapter's own
// drain bound is what ends the pump when an adapter
// does not cooperate - not dependent on Stop() actually working. Its
// Events() channel is fed directly by the test, with no forwarding
// goroutine behind it, so there is nothing here for -race to catch and
// nothing to leak: an unwatched, never-closed channel with no goroutine
// blocked on it is simply garbage once the test ends.
type stubbornProc struct {
	out chan pane.Event
}

func newStubbornProc() *stubbornProc { return &stubbornProc{out: make(chan pane.Event)} }

func (p *stubbornProc) Prompt(string) error       { return nil }
func (p *stubbornProc) Steer(string) error        { return pane.ErrUnsupported }
func (p *stubbornProc) WriteStdin([]byte) error   { return nil }
func (p *stubbornProc) Events() <-chan pane.Event { return p.out }
func (p *stubbornProc) SessionID() (string, bool) { return "", false }
func (p *stubbornProc) Stop() error               { return nil } // deliberately does nothing
func (p *stubbornProc) send(ev pane.Event)        { p.out <- ev }

type stubbornAdapter struct {
	name string
	proc *stubbornProc
}

func (a *stubbornAdapter) Name() string { return a.name }

func (a *stubbornAdapter) Start(_ context.Context, _ pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	return a.proc, nil
}

// chattyStubbornProc is stubbornProc's harder case: it ignores Stop() the
// same way, but instead of going quiet after EvEnd it keeps sending events of
// its own accord, faster than stopDrainWait, until the TEST tells it to stop
// - never because pumpAdapter asked. It exists for one thing: proving the
// drain has a single total deadline, not one that a busy adapter can keep
// pushing back forever by never going idle. stopSending is test-only
// cleanup machinery, deliberately not part of the pane.Proc contract Stop()
// belongs to - production code has no way to reach it, only this file does.
type chattyStubbornProc struct {
	out  chan pane.Event
	done chan struct{}
	once sync.Once
}

func newChattyStubbornProc() *chattyStubbornProc {
	p := &chattyStubbornProc{out: make(chan pane.Event), done: make(chan struct{})}
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-t.C:
				select {
				case p.out <- pane.Event{Kind: pane.EvText, Text: "still talking"}:
				case <-p.done:
					return
				}
			}
		}
	}()
	return p
}

func (p *chattyStubbornProc) Prompt(string) error       { return nil }
func (p *chattyStubbornProc) Steer(string) error        { return pane.ErrUnsupported }
func (p *chattyStubbornProc) WriteStdin([]byte) error   { return nil }
func (p *chattyStubbornProc) Events() <-chan pane.Event { return p.out }
func (p *chattyStubbornProc) SessionID() (string, bool) { return "", false }
func (p *chattyStubbornProc) Stop() error               { return nil } // deliberately does nothing
func (p *chattyStubbornProc) send(ev pane.Event)        { p.out <- ev }

// stopSending ends the background sender goroutine. Only the test calls
// this - it is not reachable through the pane.Proc interface, so it can
// never stand in for the adapter itself cooperating.
func (p *chattyStubbornProc) stopSending() {
	p.once.Do(func() { close(p.done) })
}

type chattyStubbornAdapter struct {
	name string
	proc *chattyStubbornProc
}

func (a *chattyStubbornAdapter) Name() string { return a.name }

func (a *chattyStubbornAdapter) Start(_ context.Context, _ pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	return a.proc, nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// Rule (b): the server renders a headless adapter's events into the SAME
// grid a pty pane uses, so pane.read cannot tell the two kinds of pane
// apart. Text and tool events also mean the pane is working, from source
// headless.
func TestHeadlessPaneRendersAdapterEventsIntoItsGrid(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-render", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-render"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)
	// The stream is never explicitly ended in this test; without this, the
	// pump goroutine and the fake's own forwarder goroutine would both leak
	// for the rest of the test binary. Wait for the pump
	// to actually finish (paneInfo(id).Closed) before the NEXT cleanup in
	// line - s.Close(), then the temp-dir removal - can run, or pumpAdapter's
	// own terminal closePane/saveLayout can race the directory disappearing
	// out from under it.
	t.Cleanup(func() {
		proc.closeIn()
		waitFor(t, 2*time.Second, func() bool {
			info, ok := s.paneInfo(id)
			return ok && info.Closed
		})
	})

	proc.send(pane.Event{Kind: pane.EvText, Text: "hello from the model"})
	proc.send(pane.Event{Kind: pane.EvTool, Tool: "Bash", Detail: "ls -la"})

	waitFor(t, 2*time.Second, func() bool {
		got := roundTrip(t, s, `{"id":"2","cmd":"pane.read","pane":"`+id+`"}`)
		if !got[0].OK {
			return false
		}
		text, _ := result(t, got[0])["text"].(string)
		return strings.Contains(text, "hello from the model") && strings.Contains(text, "Bash")
	})

	waitFor(t, 2*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateWorking && ev.Source == proto.SrcHeadless
	})
}

// An EvEnd becomes done from headless - the terminal state master spec 3.1
// reserves for process or headless - and the pane record itself closes the
// same way a pty's watchExit closes one: through closePane, so paneInfo (the
// LivePane's cached copy) never goes stale against the tree.
func TestHeadlessPaneEndEventBecomesDoneFromHeadless(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-end", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-end"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)

	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "turn complete"})
	proc.closeIn()

	waitFor(t, 2*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateDone
	})
	ev, _ := s.States().Current(id)
	if ev.Source != proto.SrcHeadless {
		t.Fatalf("source = %q, want headless", ev.Source)
	}
	if info, ok := s.paneInfo(id); !ok || !info.Closed {
		t.Fatalf("paneInfo(%s) = %+v, %v; want Closed true - closePane, not a raw tree.ClosePane, "+
			"is what keeps LivePane.Info from going stale", id, info, ok)
	}
	// WriteTranscript and ApplyState run synchronously, one after the other,
	// in pumpAdapter's own goroutine (a PTY.Drained()-style completion for
	// headless panes), so by the time done is OBSERVABLE the
	// EvEnd's own line has already landed in the grid. Assert the ordering
	// rather than just arguing it: pane.read must already show it.
	got2 := roundTrip(t, s, `{"id":"2","cmd":"pane.read","pane":"`+id+`"}`)
	if !got2[0].OK {
		t.Fatalf("pane.read failed: %+v", got2[0].Error)
	}
	text, _ := result(t, got2[0])["text"].(string)
	if !strings.Contains(text, "turn complete") {
		t.Fatalf("pane.read = %q, want the EvEnd's own transcript line already flushed by the time "+
			"done is observable", text)
	}
}

// Mirrored from watchExit: once Close() has
// released the start lock, this server no longer owns the data directory - a
// new server may already hold it - so pumpAdapter's terminal block must not
// write layout.json after that point. The fake stream stays open across
// Close() and only ends afterward, so this proves the guard rather than
// merely asserting it never fires in practice.
func TestPumpAdapterDoesNotWriteAfterServerClose(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-close", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-close"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	layoutFile := layoutPath(s.cfg.DataDir)
	fi, err := os.Stat(layoutFile)
	if err != nil {
		t.Fatal(err)
	}
	before := fi.ModTime()

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The stream ends only now, on the far side of Close() - pumpAdapter's
	// range loop was still blocked in Events() when the start lock was
	// released.
	proc.closeIn()

	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(layoutFile)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(before) {
			t.Fatalf("layout.json changed after Close(): mtime went from %v to %v", before, fi.ModTime())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A stream that just stops - the adapter's process died, or its stdout hit
// EOF - with no EvEnd at all must still report done, because pumpAdapter is
// the only place a headless pane's state comes from: nothing else is
// watching. The detail says plainly that nobody reported the end.
func TestHeadlessStreamEndingWithoutAnEndEventStillReportsDone(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-abrupt", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-abrupt"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)

	proc.closeIn()

	waitFor(t, 2*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateDone
	})
	ev, _ := s.States().Current(id)
	if ev.Source != proto.SrcHeadless {
		t.Fatalf("source = %q, want headless", ev.Source)
	}
	if !strings.Contains(ev.Detail, "adapter stream ended") {
		t.Fatalf("detail = %q, want it to say the stream just stopped", ev.Detail)
	}
}

// updatePane is the only mutator of a pane's record: once
// the adapter reports a harness session id, it lands on the tree record's
// HarnessSessionID, through updatePane, not by any other path.
func TestHeadlessPaneRecordsTheAdaptersReportedSessionID(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	proc.setSessionID("sess-123")
	adapters.Register(&fakeAdapter{name: "fake-headless-session", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-session"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)
	// Wait for the pump to actually finish before the
	// next cleanup (s.Close(), then the temp-dir removal) runs, or its
	// terminal closePane/saveLayout can race the directory disappearing.
	t.Cleanup(func() {
		proc.closeIn()
		waitFor(t, 2*time.Second, func() bool {
			info, ok := s.paneInfo(id)
			return ok && info.Closed
		})
	})

	proc.send(pane.Event{Kind: pane.EvText, Text: "hi"})

	waitFor(t, 2*time.Second, func() bool {
		info, ok := s.paneInfo(id)
		return ok && info.HarnessSessionID == "sess-123"
	})
}

// watchExit closes the pane's record BEFORE posting
// done; pumpAdapter must match that order, or a pane.wait_output --state
// done waiter could observe an OPEN record for a headless pane where it
// never could for a pty one. This asserts it with no polling after
// wait_output returns: pane.list must already show the record closed at
// that exact moment, on a separate round trip - a poll here would hide
// exactly the race this test exists to catch.
// pane.wait_output's own "state" match now carries the
// pane's closed flag in the same reply, read via s.paneInfo in the same
// branch that matches the state - never a second round trip like
// pane.list, which by the time a fresh request reaches the server has
// given closePane and ApplyState(done) plenty of real time to have both
// long since run regardless of their order. See
// TestHeadlessPaneRecordIsClosedBeforeApplyStatePostsDone below for the
// instrument that actually proves the ordering.
func TestWaitOutputStateReplyCarriesClosed(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-wait-output-closed", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-wait-output-closed"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)

	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "turn complete"})

	got2 := roundTrip(t, s, `{"id":"2","cmd":"pane.wait_output","pane":"`+id+`","state":"done","timeout_ms":5000}`)
	if !got2[0].OK {
		t.Fatalf("pane.wait_output failed: %+v", got2[0].Error)
	}
	m := result(t, got2[0])
	if m["closed"] != true {
		t.Fatalf(`wait_output's own reply = %v, want "closed": true alongside the matched state`, m)
	}
}

// The earlier version of this test
// proved vacuous under -race -count=10 - reversing pumpAdapter's closePane
// and ApplyState(done) order never turned it red, because the test's own
// second, separate pane.list round trip gave both statements, whichever
// order they ran in, plenty of real wall-clock time to have already
// finished by the time that second request was even dispatched.
//
// This version has no second round trip and no timing to get lucky on: the
// afterApplyState hook runs synchronously, on pumpAdapter's own goroutine,
// from inside the very ApplyState call that posts done - so the read of
// s.tree.Pane(id).Closed it does happens at a specific point in pumpAdapter's
// own program order, not at some later moment a second goroutine happens to
// get scheduled. If closePane has not yet run when that point is reached,
// this sees it deterministically, every time - reversing the two lines
// makes it red 100% of the time, not by luck.
func TestHeadlessPaneRecordIsClosedBeforeApplyStatePostsDone(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-apply-order", proc: proc})

	closedWhenDonePosted := make(chan bool, 1)
	s.afterApplyState = func(paneID string, ev proto.PaneStateEvent) {
		if ev.State != proto.StateDone {
			return
		}
		rec, ok := s.tree.Pane(paneID)
		select {
		case closedWhenDonePosted <- (ok && rec.Closed):
		default:
		}
	}

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-apply-order"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "turn complete"})

	select {
	case closed := <-closedWhenDonePosted:
		if !closed {
			t.Fatalf("the pane's record was not closed yet when ApplyState posted done - " +
				"closePane must run before ApplyState(done), never after")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the done event's own ApplyState call")
	}
}

// EvEnd is terminal for the pump. An adapter that sends one
// but - unlike a well-behaved one - keeps its own Events() channel open
// afterward must still be brought down: pumpAdapter calls Stop() itself,
// rather than depending on the adapter (or the test) to do its job for it.
// This fake never closes its stream on its own; only Stop() ends it.
func TestPumpAdapterStopsTheAdapterAfterEvEnd(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-laggard", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-laggard"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)

	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "turn complete"})
	// Deliberately no proc.closeIn(): the ONLY thing that can end this
	// stream is pumpAdapter itself calling Stop() - Stop() closes p.stop,
	// which the fake's own forwarder goroutine reacts to by closing
	// Events(), so nothing here leaks either.

	select {
	case <-proc.stopCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpAdapter never called Stop() after EvEnd")
	}

	waitFor(t, 2*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateDone && ev.Source == proto.SrcHeadless
	})
}

// The IDLE half of the bound: an adapter that ignores Stop()
// and never closes its stream, but also never sends anything ELSE after its
// own EvEnd, must not be able to hold a pane's record open forever.
// stubbornProc's Stop() is a deliberate no-op, so only pumpAdapter's own
// drain deadline can end this.
//
// This does NOT prove the deadline survives a stream that keeps talking -
// the actual bug (a fresh time.After() re-armed on every loop
// iteration, so an adapter faster than one event per stopDrainWait could
// hold the deadline off forever) is invisible here: with nothing arriving
// after EvEnd, the select's single timeout branch fires on schedule whether
// the timer was created inside or outside the loop, because there is only
// ever one iteration to begin with. That is exactly why an earlier
// version of this test passed against the still-broken code - see
// TestPumpAdapterHasOneTotalDrainDeadlineNotOnePerEvent for the test that
// actually exercises a chatty adapter and catches the per-iteration reset.
func TestPumpAdapterGivesUpDrainingAnIdleAdapterAfterItsBound(t *testing.T) {
	s := newPaneServer(t)
	proc := newStubbornProc()
	adapters.Register(&stubbornAdapter{name: "fake-headless-stubborn", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-stubborn"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)

	start := time.Now()
	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "turn complete"})

	waitFor(t, 3*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateDone
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("pumpAdapter took %v to give up on a stubborn adapter, want it bounded near its own drain wait", elapsed)
	}
}

// The drain must have ONE total deadline across the whole
// wait, not one that resets on every message it reads. The pre-fix code
// called time.After(stopDrainWait) fresh inside each select, so an adapter
// that ignores Stop() and keeps sending faster than once per stopDrainWait -
// exactly what chattyStubbornProc does, every 100ms, forever - could hold
// the drain, the terminal close, and this goroutine open indefinitely. Only
// hoisting the deadline above the loop (one timer, selected on every
// iteration) fixes it.
//
// waitFor's own bound below is deliberately short (stopDrainWait + 2s): the
// point is not to prove no upper bound exists, it is to prove THIS one does,
// and to fail fast (not hang the test run) if the per-iteration reset
// regresses.
func TestPumpAdapterHasOneTotalDrainDeadlineNotOnePerEvent(t *testing.T) {
	s := newPaneServer(t)
	proc := newChattyStubbornProc()
	t.Cleanup(proc.stopSending)
	adapters.Register(&chattyStubbornAdapter{name: "fake-headless-chatty", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-chatty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)

	start := time.Now()
	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "turn complete"})
	// From here on, proc's own background goroutine keeps sending every
	// 100ms - faster than stopDrainWait (1s) - and Stop() does nothing.
	// pumpAdapter's drain has to give up on its own schedule regardless.

	waitFor(t, stopDrainWait+2*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateDone
	})
	if elapsed := time.Since(start); elapsed > stopDrainWait+time.Second {
		t.Fatalf("pumpAdapter took %v to give up on a chatty adapter, want it bounded near stopDrainWait (%v)",
			elapsed, stopDrainWait)
	}

	ev, _ := s.States().Current(id)
	if ev.Source != proto.SrcHeadless {
		t.Fatalf("source = %q, want headless", ev.Source)
	}
	if ev.Detail != "turn complete" {
		t.Fatalf("detail = %q, want %q (the EvEnd's own detail, from the ONE terminal ApplyState call)",
			ev.Detail, "turn complete")
	}

	// Nothing posted twice: state.Merge's rule 2 (done is terminal) would
	// silently absorb a second ApplyState(done) at the state-store level, so
	// checking Current() again cannot tell "posted once" apart from "posted
	// twice, the second one absorbed." closePane's own saveLayout write is
	// the externally observable side effect that CAN tell them apart - if
	// the terminal close/done pair ran more than once, layout.json would be
	// written again, moving its mtime a second time.
	layoutFile := layoutPath(s.cfg.DataDir)
	fi, err := os.Stat(layoutFile)
	if err != nil {
		t.Fatal(err)
	}
	mtimeAfterDone := fi.ModTime()
	time.Sleep(500 * time.Millisecond)
	fi2, err := os.Stat(layoutFile)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(mtimeAfterDone) {
		t.Fatalf("layout.json changed again (mtime %v -> %v) after done was first observed - "+
			"the terminal close/done pair ran more than once", mtimeAfterDone, fi2.ModTime())
	}
}

// startPane's switch on rec.Kind must refuse anything it does not
// recognize outright - never register a live entry with nothing behind it.
// This bypasses the wire (handlePaneCreate already validates "kind" there
// today) to exercise startPane's own defense in depth directly.
func TestStartPaneRefusesAnUnknownKind(t *testing.T) {
	s := newPaneServer(t)
	err := s.startPane(layout.Pane{ID: "w1:p1", Kind: layout.PaneKind("teleport"), Cols: 80, Rows: 24})
	if err == nil {
		t.Fatal("startPane accepted an unknown kind")
	}
	if !errors.Is(err, errUnknownPaneKind) {
		t.Fatalf("startPane error = %v, want errUnknownPaneKind", err)
	}
	if !strings.Contains(err.Error(), "teleport") {
		t.Fatalf("error %q does not name the unknown kind", err.Error())
	}
	if _, ok := s.Live("w1:p1"); ok {
		t.Fatal("startPane registered a live entry for an unknown kind")
	}
}

// Once a headless pane's grid closes, WriteTranscript starts
// reporting an error and pumpAdapter must stop trying to render into it -
// but state must keep flowing regardless. Closing the grid is not the same
// as the pane dying, and a panic in pumpAdapter's own goroutine here would
// crash the whole test binary, which is itself part of what this proves.
func TestHeadlessPaneKeepsApplyingStateAfterItsGridCloses(t *testing.T) {
	s := newPaneServer(t)
	proc := newFakeProc()
	adapters.Register(&fakeAdapter{name: "fake-headless-gridclosed", proc: proc})

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless","harness":"fake-headless-gridclosed"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id, _ := result(t, got[0])["pane"].(string)

	lp, ok := s.Live(id)
	if !ok {
		t.Fatal("no live pane")
	}
	lp.Grid.Close()

	proc.send(pane.Event{Kind: pane.EvText, Text: "still talking"})
	waitFor(t, 2*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateWorking && ev.Source == proto.SrcHeadless
	})

	proc.send(pane.Event{Kind: pane.EvEnd, Detail: "done after grid close"})
	waitFor(t, 2*time.Second, func() bool {
		ev, ok := s.States().Current(id)
		return ok && ev.State == proto.StateDone
	})
}
