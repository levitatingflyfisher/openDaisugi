package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The small parked items of the pane foreman (FM-R-103 to FM-R-106).

func countServerNotes(s *Server, prefix string) int {
	n := 0
	for _, t := range noteTexts(s) {
		if strings.HasPrefix(t, prefix) {
			n++
		}
	}
	return n
}

func TestOneFailureToOpenTheToolsIsNotedOnce(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	s := paneServer(t, e)
	// A closed socket cannot open: the same failure at every talk is one note.
	s.closeStanding()
	for i := 0; i < 3; i++ {
		s.openStandingNote("x")
	}
	const prefix = "The foreman's tools could not be opened"
	if n := countServerNotes(s, prefix); n != 1 {
		t.Fatalf("%d notes for one failure", n)
	}
	// The socket opens again; a later failure is a new one.
	s.standMu.Lock()
	s.standShut = false
	s.standMu.Unlock()
	s.openStandingNote("x")
	s.closeStanding()
	s.openStandingNote("x")
	if n := countServerNotes(s, prefix); n != 2 {
		t.Fatalf("%d notes after a second failure", n)
	}
}

func TestTheNoteForClearedReportsCountsThem(t *testing.T) {
	if got := lostReportsNote("foreman", 1); got != "foreman ended, so 1 worker report was not typed to it. The workers' logs are still there: status and zoom show them." {
		t.Fatalf("one: %q", got)
	}
	if got := lostReportsNote("foreman", 3); !strings.Contains(got, "3 worker reports were not typed") {
		t.Fatalf("three: %q", got)
	}
}

func TestAReplacedBinaryLosesItsDeletedSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/coppice (deleted)": "/opt/coppice",
		"/opt/coppice":           "/opt/coppice",
		"/opt/(deleted)/coppice": "/opt/(deleted)/coppice",
		" (deleted)":             "",
	} {
		if got := trimDeleted(in); got != want {
			t.Errorf("trimDeleted(%q) = %q, want %q", in, got, want)
		}
	}
}

// The standing socket reads its root at each call. A child of foreman A is
// refused while A still runs and the floor's foreman is B: with the root read
// once, when the socket opened, A's child would be served. (The earlier test
// ends A first, so its child is an orphan and is refused either way.)
func TestAChildOfAnEarlierForemanIsRefusedWhileItStillRuns(t *testing.T) {
	e := newFakeEnv(t)
	fakeHarnessLinks(t, e, "")
	late := filepath.Join(e.dir, "late")
	t.Setenv("FAKE_LATE", late)
	t.Setenv("FAKE_LATE_TWICE", "1")
	t.Setenv("FAKE_LATE_OUT", filepath.Join(e.dir, "late.out"))
	t.Cleanup(func() {
		_ = os.WriteFile(late, []byte("go"), 0o600)
		_ = os.WriteFile(late+".2", []byte("go"), 0o600)
	})
	s := paneServer(t, e)
	t.Setenv("FAKE_SOCK", s.standingSockPath())
	a := startForemanWith(t, s, "claude")
	waitHarnessDone(t, s, a, e)
	// The floor's foreman is now B, a pane of its own. A and its child run.
	b, _ := json.Marshal(map[string]any{"id": "1", "cmd": "pane.create", "cwd": "/", "cmd_argv": []string{"sleep", "60"}, "kind": "pty"})
	bid, _ := result(t, roundTrip(t, s, string(b))[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, bid) })
	s.talkMu.Lock()
	s.setForeman(bid)
	s.talkMu.Unlock()
	waitUntil(t, "B to run", 20*time.Second, func() bool { return s.paneForemanPID() > 1 })
	if err := os.WriteFile(late, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the first answer", 20*time.Second, func() bool {
		return strings.Contains(readFile(e.dir+"/late.out"), "answer:")
	})
	if got := readFile(e.dir + "/late.out"); !strings.Contains(got, "refused") {
		t.Fatalf("a child of A was served while B was the foreman: %s", got)
	}
	// A ends; its child lives on, a stranger to B's tree.
	closePane(t, s, a)
	if err := os.WriteFile(late+".2", []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the second answer", 20*time.Second, func() bool {
		return strings.Count(readFile(e.dir+"/late.out"), "answer:") >= 2
	})
	if got := readFile(e.dir + "/late.out"); strings.Count(got, "refused") != 2 {
		t.Fatalf("the child was served after A ended: %s", got)
	}
}

// FN-R-C4: a worker gets its tools with lowercase names, through spawn.
func TestAWorkerGetsItsToolsWithLowercaseNames(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if got, err := w.toolSpawn("trellis", "the brief", "", ""); err != nil || got != "started trellis-1" {
		t.Fatalf("spawn = %q, %v", got, err)
	}
	w.waitWork(t, 1)
	var stdin string
	for _, l := range strings.Split(read(w.e.log), "\n") {
		if p, ok := strings.CutPrefix(l, "stdin: "); ok {
			stdin = p
		}
	}
	var req struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(stdin), &req); err != nil {
		t.Fatalf("the worker's request %q: %v", stdin, err)
	}
	want := []string{"read", "write", "edit", "bash"}
	if strings.Join(req.Tools, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", req.Tools, want)
	}
	for _, name := range req.Tools {
		if name != strings.ToLower(name) {
			t.Fatalf("tool name %q is not lowercase", name)
		}
	}
}
