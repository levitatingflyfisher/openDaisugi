package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/pando"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/steward"
)

// workServer is a chat-mode server with one pinned project, trellis, and
// an envelope file for it.
type workServer struct {
	*turnServer
	project string
}

func newWorkServer(t *testing.T) *workServer {
	t.Helper()
	ts := newTurnServer(t, nil)
	project := filepath.Join(ts.e.dir, "projects", "trellis")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(config.Config{Projects: []string{project}}); err != nil {
		t.Fatal(err)
	}
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":50000,"turns":30}`)
	return &workServer{turnServer: ts, project: project}
}

// rootSession is the root session the project's envelope file names now.
func (w *workServer) rootSession(t *testing.T) string {
	t.Helper()
	pe, err := readProjectEnvelope(filepath.Join(envelopeDir(), "trellis.json"), "trellis")
	if err != nil {
		t.Fatal(err)
	}
	return rootSession("trellis", pe)
}

func writeEnvelope(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(envelopeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envelopeDir(), name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// spawn calls the spawn tool as the turn socket would.
func (w *workServer) spawn(brief string, extra ...string) (string, error) {
	profile, model := "", ""
	if len(extra) > 0 {
		profile = extra[0]
	}
	if len(extra) > 1 {
		model = extra[1]
	}
	return w.toolSpawn("trellis", brief, profile, model)
}

func (w *workServer) workMessages(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range w.logMessages(t) {
		if m.Kind == pando.KindWork {
			out = append(out, m.Text)
		}
	}
	return out
}

func (w *workServer) waitWork(t *testing.T, n int) []string {
	t.Helper()
	var got []string
	waitUntil(t, "the work message", 90*time.Second, func() bool {
		got = w.workMessages(t)
		return len(got) >= n
	})
	return got
}

func TestEachEndOfAWorkerIsOneWorkMessage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		exit   string
		errTxt string
		want   string
	}{
		{"ran", `{"status":"ran","session":"s1","answer":"All done.\nThree files changed.","tokens_used":10,"turns_used":2,"reasons":[],"ask_id":null}`, "0", "",
			"[trellis-1] report\nAll done.\nThree files changed."},
		{"refused", `{"status":"refused","session":null,"answer":null,"reasons":["the edge is wider than the root","second reason"],"ask_id":null}`, "1", "",
			"[trellis-1] refused: the edge is wider than the root; second reason"},
		{"asked", `{"status":"asked","session":null,"answer":null,"reasons":["needs the operator"],"ask_id":"ask_0123456789ab"}`, "3", "",
			"[trellis-1] sent to the operator as ask ask_0123456789ab. needs the operator"},
		{"failed with reasons", `{"status":"failed","session":"s2","answer":null,"reasons":["sprig ended with 1"],"ask_id":null}`, "1", "",
			"[trellis-1] failed: sprig ended with 1"},
		{"bad input", ``, "2", "Tried to read the tree run request on stdin.\nenvelope is not valid\nFix the request and run again.",
			"[trellis-1] failed: Fix the request and run again."},
		{"not json", `plain text`, "0", "", "[trellis-1] failed: tree run gave an answer that is not JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkServer(t)
			t.Setenv("FAKE_TREE_STATUS", tc.status)
			t.Setenv("FAKE_TREE_EXIT", tc.exit)
			t.Setenv("FAKE_TREE_ERR", tc.errTxt)
			got, err := w.spawn("fix the thing")
			if err != nil || got != "started trellis-1" {
				t.Fatalf("spawn = %q, %v", got, err)
			}
			msgs := w.waitWork(t, 1)
			if len(msgs) != 1 || msgs[0] != tc.want {
				t.Fatalf("work messages = %q, want %q", msgs, tc.want)
			}
			// A work message starts no turn.
			time.Sleep(200 * time.Millisecond)
			if recs, _ := steward.ReadTurns(w.logDir); len(recs) != 0 {
				t.Fatalf("a work message started a turn: %+v", recs)
			}
		})
	}
}

func TestSpawnReturnsBeforeTheWorkerEnds(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_MS", "4000")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"done","reasons":[]}`)
	start := time.Now()
	if _, err := w.spawn("a long job"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("spawn took %s", took)
	}
	if got := w.workMessages(t); len(got) != 0 {
		t.Fatalf("a report came at once: %q", got)
	}
	st := w.toolStatus()
	if !strings.Contains(st, "trellis-1 | trellis | default | - | 20") || !strings.Contains(st, "| running | -") {
		t.Fatalf("status = %q", st)
	}
	if !strings.Contains(w.workersState(), "- trellis-1 on trellis since 20") {
		t.Fatalf("state block = %q", w.workersState())
	}
	w.waitWork(t, 1)
	st = w.toolStatus()
	want := filepath.Join(w.logDir, "work", "warden", "sessions", "s1")
	if !strings.Contains(st, "| ran | "+want) || w.workersState() != "workers: none\n" {
		t.Fatalf("status = %q, state = %q", st, w.workersState())
	}
}

func TestTheWorkerRunsInTheProjectWithTheRequestOnStdin(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("the brief", "default", "haiku"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	root := w.rootSession(t)
	for _, want := range []string{
		"tree root pwd: " + filepath.Join(w.logDir, "work"),
		"[tree] [root] [" + filepath.Join(w.logDir, "work", "roots", root+".envelope.json") + "] [--session] [" + root + "] [--data-dir]",
		"[--tokens] [50000] [--turns] [30]",
		"tree run pwd: " + w.project,
		"[tree] [run] [--parent] [" + root + "] [--profile=default] [--model=haiku] [--tokens] [50000] [--turns] [30] [--max-run-s] [1800] [--data-dir] [" + filepath.Join(w.logDir, "work") + "] [--root] [" + filepath.Join(w.logDir, "work", "warden") + "] [-]",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("the fake's log lacks %q:\n%s", want, log)
		}
	}
	var req struct {
		Task     string   `json:"task"`
		Tools    []string `json:"tools"`
		Envelope struct {
			ID string `json:"id"`
		} `json:"envelope"`
	}
	for _, l := range strings.Split(log, "\n") {
		if p, ok := strings.CutPrefix(l, "stdin: "); ok {
			if err := json.Unmarshal([]byte(p), &req); err != nil {
				t.Fatal(err)
			}
		}
	}
	if req.Task != "the brief" || req.Envelope.ID != "env_1" || strings.Join(req.Tools, ",") != "read,write,edit,bash,help" {
		t.Fatalf("request = %+v", req)
	}
	if strings.Contains(log, "fd3") {
		t.Fatalf("the default profile got a key descriptor:\n%s", log)
	}
}

// FN-R-28: a fenced worker's tree run gets --sprig-fence with the fence in
// DAISUGI_SPRIG_FENCE, so a daisugi that cannot apply it starts nothing.
func TestAFencedWorkerRequiresTheSprigFence(t *testing.T) {
	w := newWorkServer(t)
	armWarden(t, w.Server)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("the brief", "default", "haiku"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	if !strings.Contains(log, "[--root] ["+filepath.Join(w.logDir, "work", "warden")+"] [--sprig-fence] [-]") {
		t.Fatalf("the tree run lacks --sprig-fence:\n%s", log)
	}
	if !strings.Contains(log, "tree run env DAISUGI_SPRIG_FENCE=[") {
		t.Fatalf("the tree run lacks DAISUGI_SPRIG_FENCE:\n%s", log)
	}
}

// The folders the project's approved envelope lets a worker read and write
// are opened in its fence too: read only for file_read, read and write for
// file_write. Without this the kernel denied every read outside the
// project, whatever the envelope said (the Trellis stalls of 2026-11-06).
func TestAFencedWorkerGetsTheEnvelopesFoldersInItsFence(t *testing.T) {
	w := newWorkServer(t)
	armWarden(t, w.Server)
	notes := t.TempDir()
	out := t.TempDir()
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{"file_read":["`+notes+`","`+out+`"],"file_write":["`+out+`"]}},"tokens":0,"turns":0}`)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("the brief", "default", "haiku"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	for _, want := range []string{`"--allow","` + notes + `"`, `"--allow","` + out + `:rw"`} {
		if !strings.Contains(log, want) {
			t.Fatalf("the sprig fence lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, `"--allow","`+notes+`:rw"`) {
		t.Fatalf("a file_read folder must not be writable:\n%s", log)
	}
}

func TestTheRootIsMadeOnce(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	for i := 0; i < 2; i++ {
		if _, err := w.spawn("job"); err != nil {
			t.Fatal(err)
		}
		w.waitWork(t, i+1)
	}
	if n := strings.Count(read(w.e.log), "tree root pwd"); n != 1 {
		t.Fatalf("the root was made %d times", n)
	}
	if got := w.toolStatus(); !strings.Contains(got, "trellis-2") {
		t.Fatalf("status = %q", got)
	}
}

func TestAKeyForAnotherProfileReachesTreeRunOnFd3Only(t *testing.T) {
	w := newWorkServer(t)
	sentinel := "sk-SENTINEL-worker"
	t.Setenv("FAKE_SENTINEL", sentinel)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job", "desk"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	for _, want := range []string{"[agent-profiles] [bundle] [--fd] [3]", "[--profile=desk]", "[--keys-fd] [3]", "bundle: has the key"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the fake's log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, sentinel) {
		t.Fatalf("the sentinel is in the fake's log:\n%s", log)
	}
	if hits := grepTree([]string{w.logDir, w.stewardDir(), w.cfg.DataDir}, sentinel); len(hits) != 0 {
		t.Fatalf("the sentinel reached %v", hits)
	}
	for _, n := range noteTexts(w.Server) {
		if strings.Contains(n, sentinel) {
			t.Fatalf("the sentinel is in a note: %s", n)
		}
	}
}

// unpin makes the work server's project a recent dir that sits under no
// trusted root.
func (w *workServer) unpin(t *testing.T) {
	t.Helper()
	if err := config.Save(config.Config{Steward: config.Steward{TrustedRoots: []string{"/nowhere/code"}}}); err != nil {
		t.Fatal(err)
	}
	if err := saveRecentDirs(w.cfg.DataDir, []string{w.project}); err != nil {
		t.Fatal(err)
	}
}

// A project outside every trusted root with no envelope is refused with one
// line, and the owner gets a card for the default envelope.
func TestAMissingEnvelopeRefusesTheSpawnWithOneLine(t *testing.T) {
	w := newWorkServer(t)
	w.unpin(t)
	if err := os.Remove(filepath.Join(envelopeDir(), "trellis.json")); err != nil {
		t.Fatal(err)
	}
	_, err := w.spawn("job")
	want := "no envelope for the project trellis: " + w.project + " is outside the trusted roots, so the owner has a card for the default envelope"
	if err == nil || !strings.HasPrefix(err.Error(), want) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("err = %v", err)
	}
	b, rerr := os.ReadFile(w.proposalPath("trellis"))
	if rerr != nil || !strings.Contains(string(b), "deny_git_push") || !strings.Contains(string(b), `"by":"default"`) {
		t.Fatalf("the card for the default = %s, %v", b, rerr)
	}
	if _, serr := os.Stat(filepath.Join(envelopeDir(), "trellis.json")); serr == nil {
		t.Fatal("an untrusted project got an envelope file with no card")
	}
	writeEnvelope(t, "trellis", `not json`)
	if _, err := w.spawn("job"); err == nil || !strings.Contains(err.Error(), "does not read") {
		t.Fatalf("err = %v", err)
	}
	if got := w.toolStatus(); got != "no workers" {
		t.Fatalf("a refused spawn made a worker: %q", got)
	}
}

func TestAnUnknownProjectIsRefusedWithTheList(t *testing.T) {
	w := newWorkServer(t)
	_, err := w.toolSpawn("glean", "job", "", "")
	if err == nil || !strings.Contains(err.Error(), `"glean" is not one of the operator's projects. They are: trellis`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := w.toolSpawn("trellis", "  ", "", ""); err == nil {
		t.Fatal("an empty brief was taken")
	}
}

func TestAWorkerThatWasRunningWhenCoppiceStoppedIsReportedAtTheNextStart(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_MS", "20000")
	if _, err := w.spawn("a job that outlives the server"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the worker to start", 10*time.Second, func() bool { return strings.Contains(read(w.e.inflight), "tree run start") })
	logDir := w.logDir
	if err := w.Server.Close(); err != nil {
		t.Fatal(err)
	}
	// The worker's process group is gone with the server.
	var pid string
	for _, l := range strings.Split(read(w.e.inflight), "\n") {
		if p, ok := strings.CutPrefix(l, "tree run start "); ok {
			pid = p
		}
	}
	if _, err := os.Stat("/proc/" + pid); !os.IsNotExist(err) {
		t.Fatalf("the worker %s still runs: %v", pid, err)
	}
	// A new server on the same log dir reports it once.
	s2, _ := chatServer(t, w.e, func(f *StewardConfig) { f.LogDir = logDir })
	s2.StartSteward()
	fc := s2.ensureChat()
	fc.mu.Lock()
	n := fc.chat.Count()
	var last pando.Message
	for i := int64(0); i < n; i++ {
		m, _ := fc.chat.Message(i)
		if m.Kind == pando.KindWork {
			last = m
		}
	}
	fc.mu.Unlock()
	if last.Text != "[trellis-1] was stopped when coppice stopped" {
		t.Fatalf("the restart said %q", last.Text)
	}
	if !strings.Contains(s2.toolStatus(), "| stopped |") {
		t.Fatalf("status = %q", s2.toolStatus())
	}
}

func TestTheProfileAndTheModelOfASpawnAreChecked(t *testing.T) {
	w := newWorkServer(t)
	for _, bad := range [][2]string{{"--evil", ""}, {"desk", "-x"}, {"a b", ""}, {"desk", "m;rm"}, {"-", ""}} {
		if _, err := w.toolSpawn("trellis", "job", bad[0], bad[1]); err == nil {
			t.Fatalf("profile %q model %q was taken", bad[0], bad[1])
		}
	}
	if got := w.toolStatus(); got != "no workers" {
		t.Fatalf("a refused spawn made a worker: %q", got)
	}
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.toolSpawn("trellis", "job", "default", "claude-3.5:v2@x/y"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
}

func TestTheWorkerKnowsTheDataDir(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	want := "env COPPICE_DATA_DIR=" + w.cfg.DataDir + "\n"
	if strings.Count(log, want) != 2 {
		t.Fatalf("tree root and tree run both need COPPICE_DATA_DIR=%s:\n%s", w.cfg.DataDir, log)
	}
}

// A worker the steward spawned is not the owner's chat: it gets no memory
// view and its messages are not logged into the work store (round 7 fix).
func TestASpawnedWorkerIsMarkedSoPandoSkipsIt(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	if log := read(w.e.log); !strings.Contains(log, "tree run env DAISUGI_PANDO_SKIP=1\n") {
		t.Fatalf("the worker has no skip marker:\n%s", log)
	}
	// It also names the skip list of the data home, so each model call of the
	// worker's tree lists its session id before it runs.
	want := "tree run env DAISUGI_PANDO_SKIPLIST=" + pando.SkipListIn(steward.StoreRoot(w.cfg.DataDir)) + "\n"
	if log := read(w.e.log); !strings.Contains(log, want) {
		t.Fatalf("the worker has no skip list, want %q:\n%s", want, log)
	}
}

func TestClosingStopsAWorkerWithSIGTERMFirst(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_MS", "30000")
	if _, err := w.spawn("a long job"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the worker to start", 10*time.Second, func() bool { return strings.Contains(read(w.e.inflight), "tree run start") })
	if err := w.Server.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(w.e.inflight), "tree run got TERM") {
		t.Fatalf("the worker was killed without a SIGTERM:\n%s", read(w.e.inflight))
	}
}

func TestAGrandchildHoldingFd3DoesNotHangTheBundleRead(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_SENTINEL", "sk-SENTINEL-grandchild")
	t.Setenv("FAKE_BUNDLE_GRANDCHILD", "1")
	start := time.Now()
	data, err := w.bundleBytes(context.Background())
	if err != nil || !strings.Contains(string(data), "sk-SENTINEL-grandchild") {
		t.Fatalf("bundleBytes = %q, %v", data, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the read waited %s for a grandchild", took)
	}
}

func TestABundleReadEndsAtTheDeadline(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_BUNDLE_GRANDCHILD", "1")
	t.Setenv("FAKE_BUNDLE_HANG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := w.bundleBytes(ctx); err == nil {
		t.Fatal("a hung command gave a bundle")
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("the read ran %s past a 500 ms deadline", took)
	}
}

func TestRunCmdReportsThePidOnceAndPassesTheKeyOnDescriptor3(t *testing.T) {
	var pids []int
	stdout, _, code, err := runCmd(context.Background(), cmdOpts{
		Dir: t.TempDir(), Name: "/bin/sh", Args: []string{"-c", "cat <&3; echo $$ >&2"},
		FD3: []byte("key-bytes"), HasFD3: true, Env: []string{"PATH=/usr/bin:/bin"},
		Started: func(pid int) { pids = append(pids, pid) },
	})
	if err != nil || code != 0 || stdout != "key-bytes" {
		t.Fatalf("runCmd = %q, %d, %v", stdout, code, err)
	}
	if len(pids) != 1 || pids[0] <= 0 {
		t.Fatalf("Started got %v", pids)
	}
}

// A steward is logged once. With the chat open, coppice logs a worker's report
// as a work message and does not also type it into a pane. With no chat (a
// pane steward, whose harness the live feed reads from its own database),
// coppice writes nothing to the store.
func TestAWorkersReportIsLoggedByOnePathOnly(t *testing.T) {
	w := newWorkServer(t)
	fc := w.fcOf()
	w.logWork(fc, "[w-1] with a chat")
	if got := w.workMessages(t); len(got) != 1 || got[0] != "[w-1] with a chat" {
		t.Fatalf("with a chat the report is one work message: %q", got)
	}
	w.talkMu.Lock()
	queued := len(w.talk.reports)
	w.talkMu.Unlock()
	if queued != 0 {
		t.Fatalf("a logged report was also typed into a pane: %d", queued)
	}
	// The chat is gone: a pane steward. Nothing more enters the store.
	fc.mu.Lock()
	chat := fc.chat
	fc.chat = nil
	fc.mu.Unlock()
	w.logWork(fc, "[w-2] with no chat")
	fc.mu.Lock()
	fc.chat = chat
	fc.mu.Unlock()
	if got := w.workMessages(t); len(got) != 1 {
		t.Fatalf("with no chat coppice wrote to the store: %q", got)
	}
}

// An approve writes a new envelope file. The next worker runs under a new
// root made from that file, never the root of the older one (ledger
// 2026-11-06: worker 3 was refused against the old root).
func TestAnApprovedEnvelopeMakesANewRootForTheNextWorker(t *testing.T) {
	w := newWorkServer(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	t.Setenv("FAKE_TREE_STATUS_DOC", `{"nodes":[]}`)
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	first := w.rootSession(t)
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_2","generated_by":"test","task":"t","permissions":{"network":true}},"tokens":0,"turns":0}`)
	second := w.rootSession(t)
	if first == second || !strings.HasPrefix(second, "project-trellis-") {
		t.Fatalf("roots %q and %q", first, second)
	}
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 2)
	log := read(w.e.log)
	if n := strings.Count(log, "tree root pwd"); n != 2 {
		t.Fatalf("the root was made %d times:\n%s", n, log)
	}
	if !strings.Contains(log, "[tree] [run] [--parent] ["+second+"]") {
		t.Fatalf("the second worker is not under the new root %s:\n%s", second, log)
	}
	b, err := os.ReadFile(filepath.Join(w.logDir, "work", "roots", second+".envelope.json"))
	if err != nil || !strings.Contains(string(b), "env_2") {
		t.Fatalf("the new root's envelope = %s, %v", b, err)
	}
}

// A worker asks for what the root has left, not the whole budget: the
// second Trellis worker asked for 3M when 2.74M was left (2026-11-06).
func TestAWorkerAsksForWhatTheRootHasLeft(t *testing.T) {
	w := newWorkServer(t)
	root := w.rootSession(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	t.Setenv("FAKE_TREE_STATUS_DOC", `{"nodes":[{"session":"`+root+`","tokens":{"budget":50000,"used":0,"left":41000},"turns":{"budget":30,"used":0,"left":22}}]}`)
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	if !strings.Contains(log, "[--tokens] [41000] [--turns] [22] [--max-run-s] [1800] [--data-dir]") {
		t.Fatalf("the worker did not ask for what is left:\n%s", log)
	}
}

// An envelope with no budget (the default) makes a root with none, and a
// worker that asks for none.
func TestAnEnvelopeWithNoBudgetAsksForNone(t *testing.T) {
	w := newWorkServer(t)
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":0,"turns":0}`)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	if strings.Contains(log, "[--tokens]") || strings.Contains(log, "[--turns]") || strings.Contains(log, "[tree] [status]") {
		t.Fatalf("a budget was asked for:\n%s", log)
	}
}

// A spent budget fails the worker with one line that says where to raise it.
func TestASpentBudgetFailsTheWorkerWithOneLine(t *testing.T) {
	w := newWorkServer(t)
	root := w.rootSession(t)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	t.Setenv("FAKE_TREE_STATUS_DOC", `{"nodes":[{"session":"`+root+`","tokens":{"left":0},"turns":{"left":4}}]}`)
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	msgs := w.waitWork(t, 1)
	if !strings.Contains(msgs[0], "failed: the project's budget is spent: 0 tokens and 4 turns left") {
		t.Fatalf("message = %q", msgs[0])
	}
	if strings.Contains(read(w.e.log), "[tree] [run]") {
		t.Fatal("a worker ran on a spent budget")
	}
}

// A known project (pinned, or under a trusted root) with no envelope gets
// the default one at its first spawn, and no card.
func TestAKnownProjectSpawnsWithNoCard(t *testing.T) {
	w := newWorkServer(t)
	armWarden(t, w.Server)
	if err := os.Remove(filepath.Join(envelopeDir(), "trellis.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	b, err := os.ReadFile(filepath.Join(envelopeDir(), "trellis.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"granted_by": "default"`, `"tokens": 0`, `"deny_git_push"`, `"max_execution_time_s": 30`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the default envelope lacks %s:\n%s", want, b)
		}
	}
	if _, err := os.Stat(w.proposalPath("trellis")); err == nil {
		t.Fatal("a known project made a card")
	}
	log := read(w.e.log)
	if strings.Contains(log, "[--tokens]") || !strings.Contains(log, "[tree] [run]") {
		t.Fatalf("the worker did not run without a budget:\n%s", log)
	}
}

// A trusted root other than the pinned list: a project under ~/code is
// known with nothing pinned.
func TestAProjectUnderCodeIsKnown(t *testing.T) {
	w := newWorkServer(t)
	armWarden(t, w.Server)
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "code", "lilt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveRecentDirs(w.cfg.DataDir, []string{dir}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.toolSpawn("lilt", "job", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(envelopeDir(), "lilt.json")); err != nil {
		t.Fatal(err)
	}
}

// An envelope file written for another dir of the same name is refused.
func TestAnEnvelopeFileForAnotherDirIsRefused(t *testing.T) {
	w := newWorkServer(t)
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":0,"turns":0,"dir":"/elsewhere/trellis"}`)
	if _, err := w.spawn("job"); err == nil || !strings.Contains(err.Error(), "was written for /elsewhere/trellis") {
		t.Fatalf("err = %v", err)
	}
}

// Review r9 finding 5a: the default is card-free only for a worker that
// runs fenced, since the fence is the wall behind the deny list. A bare
// worker in a known project cards once, and the card says the fence is off.
func TestABareWorkerInAKnownProjectCardsOnce(t *testing.T) {
	w := newWorkServer(t)
	if err := os.Remove(filepath.Join(envelopeDir(), "trellis.json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_, err := w.spawn("job")
		if err == nil || !strings.Contains(err.Error(), "the fence is off") {
			t.Fatalf("spawn %d: err = %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(envelopeDir(), "trellis.json")); err == nil {
		t.Fatal("a bare worker got the default with no card")
	}
	pending := w.readProposals()
	if len(pending) != 1 {
		t.Fatalf("proposals = %d", len(pending))
	}
	if got := strings.Join(w.widerLines(pending[0]), "\n"); !strings.Contains(got, "the fence is off") {
		t.Fatalf("the card does not say the fence is off: %s", got)
	}
}

// Review r9 finding 8: a file that returns to earlier bytes makes a new
// root, not the old one with its budget spent.
func TestAFileBackToEarlierBytesGetsANewRoot(t *testing.T) {
	w := newWorkServer(t)
	a := `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":50000,"turns":30}`
	writeEnvelope(t, "trellis", a)
	first := w.rootSession(t)
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":90000,"turns":30}`)
	time.Sleep(10 * time.Millisecond)
	writeEnvelope(t, "trellis", a)
	if again := w.rootSession(t); again == first {
		t.Fatalf("the earlier root %s came back", first)
	}
}

// Review r9 finding 6: a steward worker has a run cap, [steward]
// worker_max_run_s (default 1800 s). Past it the worker fails with a line
// that names the setting and says long work is a lead.
func TestAWorkerPastItsRunCapIsToldToUseALead(t *testing.T) {
	w := newWorkServer(t)
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	two := 2
	cfg.Steward.WorkerMaxRunS = &two
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TREE_STATUS", `{"status":"failed","session":"s1","answer":null,"reasons":["agentic sub-agent failed: sprig ran past 2s and was killed"]}`)
	t.Setenv("FAKE_TREE_EXIT", "1")
	if _, err := w.spawn("job"); err != nil {
		t.Fatal(err)
	}
	msgs := w.waitWork(t, 1)
	if !strings.Contains(read(w.e.log), "[--max-run-s] [2] [--data-dir]") {
		t.Fatalf("the cap did not reach tree run:\n%s", read(w.e.log))
	}
	want := "ran past the worker run cap of 2 s ([steward] worker_max_run_s). Long work is a lead: spawn it with long_running: true"
	if !strings.Contains(msgs[0], want) {
		t.Fatalf("message = %q", msgs[0])
	}
}

// Review r9 fix 1, new finding 1: an envelope the default granted (with
// the fence on) is not honoured by a bare worker later. The spawn cards
// once and runs nothing.
func TestADefaultGrantIsNotUsedByABareWorkerLater(t *testing.T) {
	w := newWorkServer(t)
	writeEnvelope(t, "trellis", `{"envelope":{"id":"env_1","generated_by":"coppice default","task":"t","permissions":{}},"tokens":0,"turns":0,"dir":"`+w.project+`","granted_by":"default"}`)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	_, err := w.spawn("job")
	if err == nil || !strings.Contains(err.Error(), "the fence is off") || !strings.Contains(err.Error(), "granted by default") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(read(w.e.log), "[tree] [run]") || strings.Contains(read(w.e.log), "[tree] [root]") {
		t.Fatal("a bare worker ran under a default grant")
	}
	if len(w.readProposals()) != 1 {
		t.Fatal("no card for the default")
	}
}
