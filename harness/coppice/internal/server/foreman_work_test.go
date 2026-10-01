package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/foreman"
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
		if m.Kind == foreman.KindWork {
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
			if recs, _ := foreman.ReadTurns(w.logDir); len(recs) != 0 {
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
	want := filepath.Join(w.logDir, "work", "gate", "sessions", "s1")
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
	for _, want := range []string{
		"tree root pwd: " + filepath.Join(w.logDir, "work"),
		"[tree] [root] [" + filepath.Join(w.logDir, "work", "roots", "trellis.envelope.json") + "] [--session] [project-trellis] [--data-dir]",
		"[--tokens] [50000] [--turns] [30]",
		"tree run pwd: " + w.project,
		"[tree] [run] [--parent] [project-trellis] [--profile=default] [--model=haiku] [--data-dir] [" + filepath.Join(w.logDir, "work") + "] [--root] [" + filepath.Join(w.logDir, "work", "gate") + "] [-]",
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
	if req.Task != "the brief" || req.Envelope.ID != "env_1" || strings.Join(req.Tools, ",") != "read,write,edit,bash" {
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
	armGate(t, w.Server)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("the brief", "default", "haiku"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	if !strings.Contains(log, "[--root] ["+filepath.Join(w.logDir, "work", "gate")+"] [--sprig-fence] [-]") {
		t.Fatalf("the tree run lacks --sprig-fence:\n%s", log)
	}
	if !strings.Contains(log, "tree run env DAISUGI_SPRIG_FENCE=[") {
		t.Fatalf("the tree run lacks DAISUGI_SPRIG_FENCE:\n%s", log)
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
	if hits := grepTree([]string{w.logDir, w.foremanDir(), w.cfg.DataDir}, sentinel); len(hits) != 0 {
		t.Fatalf("the sentinel reached %v", hits)
	}
	for _, n := range noteTexts(w.Server) {
		if strings.Contains(n, sentinel) {
			t.Fatalf("the sentinel is in a note: %s", n)
		}
	}
}

func TestAMissingEnvelopeRefusesTheSpawnWithOneLine(t *testing.T) {
	w := newWorkServer(t)
	if err := os.Remove(filepath.Join(envelopeDir(), "trellis.json")); err != nil {
		t.Fatal(err)
	}
	_, err := w.spawn("job")
	want := "no envelope for the project trellis. Write one at " + filepath.Join(envelopeDir(), "trellis.json")
	if err == nil || !strings.HasPrefix(err.Error(), want) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("err = %v", err)
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
	s2, _ := chatServer(t, w.e, func(f *ForemanConfig) { f.LogDir = logDir })
	s2.StartForeman()
	fc := s2.ensureChat()
	fc.mu.Lock()
	n := fc.chat.Count()
	var last foreman.Message
	for i := int64(0); i < n; i++ {
		m, _ := fc.chat.Message(i)
		if m.Kind == foreman.KindWork {
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
