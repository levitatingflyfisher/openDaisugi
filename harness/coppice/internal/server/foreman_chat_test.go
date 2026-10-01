package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

var ctx0 = context.Background()

// chatServer is a test server in chat mode with its log dir in the scratch.
func chatServer(t *testing.T, e *fakeEnv, tweak func(*ForemanConfig)) (*Server, string) {
	t.Helper()
	logDir := filepath.Join(e.dir, "log", "foreman")
	f := ForemanConfig{Mode: "chat", LogDir: logDir}
	if tweak != nil {
		tweak(&f)
	}
	dir := filepath.Join(e.dir, "data")
	s, err := New(Config{SocketPath: filepath.Join(e.dir, "s.sock"), DataDir: dir,
		ForemanDir: filepath.Join(e.dir, "state", "coppice", "foreman"), HookHome: e.dir, Foreman: f})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, logDir
}

// longMessages logs n messages that need the model, under the chat lock.
func longMessages(t *testing.T, fc *foremanChat, n int) {
	t.Helper()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for i := 0; i < n; i++ {
		if _, err := fc.chat.Append(ctx0, foreman.KindUser, fmt.Sprintf("%d: ", i)+strings.Repeat("word ", 200)); err != nil {
			t.Fatal(err)
		}
	}
	fc.wake()
}

func pendingOf(fc *foremanChat) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.chat.Pending()
}

func TestPaneModeOpensNoChat(t *testing.T) {
	e := newFakeEnv(t)
	s := newTestServer(t)
	s.StartForeman()
	if s.fc != nil || s.chatMode() {
		t.Fatal("pane mode opened a chat")
	}
	if _, err := os.Stat(foreman.LogDir(s.cfg.DataDir)); !os.IsNotExist(err) {
		t.Fatalf("pane mode made a log dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.foremanDir(), "compactor")); !os.IsNotExist(err) {
		t.Fatalf("pane mode made a compactor dir: %v", err)
	}
	_ = e
}

func TestChatModeOpensTheLogAtStart(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	s.StartForeman()
	fc := s.ensureChat()
	if fc.problem != "" || fc.chat == nil {
		t.Fatalf("problem: %s", fc.problem)
	}
	for _, d := range []string{"main", "tree"} {
		if fi, err := os.Stat(filepath.Join(logDir, d)); err != nil || !fi.IsDir() {
			t.Fatalf("no %s dir in the log dir: %v", d, err)
		}
	}
	if fi, _ := os.Stat(logDir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("log dir mode %o", fi.Mode().Perm())
	}
}

func TestAHistoryThatCannotBeReadIsMovedAsideAndNoted(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	if err := os.MkdirAll(filepath.Join(logDir, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(logDir, "main", "2026-10-08.jsonl")
	if err := os.WriteFile(bad, []byte("this is not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.StartForeman()
	fc := s.ensureChat()
	if fc.problem != "" || fc.chat == nil {
		t.Fatalf("problem: %s", fc.problem)
	}
	moved, _ := filepath.Glob(logDir + ".broken-*")
	if len(moved) != 1 {
		t.Fatalf("moved dirs = %v", moved)
	}
	if b := read(filepath.Join(moved[0], "main", "2026-10-08.jsonl")); b != "this is not json\n" {
		t.Fatalf("the old history was changed: %q", b)
	}
	fc.mu.Lock()
	m, err := fc.chat.Message(0)
	fc.mu.Unlock()
	if err != nil || m.Kind != foreman.KindNote || !strings.Contains(m.Text, "could not be read") {
		t.Fatalf("chat note = %+v, %v", m, err)
	}
	found := false
	for _, n := range noteTexts(s) {
		found = found || strings.Contains(n, "could not be read")
	}
	if !found {
		t.Fatalf("no floor note: %v", noteTexts(s))
	}
}

func TestALogThatCannotBeOpenedIsAPlainErrorAndTheFloorStarts(t *testing.T) {
	e := newFakeEnv(t)
	blocker := filepath.Join(e.dir, "blocker")
	if err := os.WriteFile(blocker, []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := chatServer(t, e, func(f *ForemanConfig) { f.LogDir = filepath.Join(blocker, "foreman") })
	s.StartForeman()
	fc := s.ensureChat()
	if fc.chat != nil || !strings.Contains(fc.problem, "could not be opened") || !strings.Contains(fc.problem, "The rest of the floor works") {
		t.Fatalf("problem = %q", fc.problem)
	}
	if len(noteTexts(s)) == 0 || !strings.Contains(noteTexts(s)[0], "could not be opened") {
		t.Fatalf("notes = %v", noteTexts(s))
	}
	// The rest of the floor works.
	got := roundTrip(t, s, `{"id":"1","cmd":"server.status"}`)
	if !got[0].OK {
		t.Fatalf("server.status = %+v", got[0])
	}
}

func TestTheCompactorBuildsTheNodesThatNeedTheModel(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	s.StartForeman()
	fc := s.ensureChat()
	longMessages(t, fc, 3)
	waitUntil(t, "the compactor", 90*time.Second, func() bool { return pendingOf(fc) == 0 })
	fc.mu.Lock()
	n, ok := fc.chat.Node(foreman.NodeID{L: 0, I: 0})
	fc.mu.Unlock()
	if !ok || n.Text != "a short summary" {
		t.Fatalf("node 0 = %+v %v", n, ok)
	}
	log := read(e.log)
	for _, want := range []string{"[--tools] [none]", "[--system-file]", "[--json]", "[--wire] [claude-code]", "[--model] [haiku]"} {
		if !strings.Contains(log, want) {
			t.Fatalf("argv lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, "fd3") {
		t.Fatalf("the default profile sent a key:\n%s", log)
	}
	usage := read(filepath.Join(logDir, foreman.UsageFile))
	if strings.Count(usage, "\n") < 3 || strings.Contains(usage, "short summary") || strings.Contains(usage, "word word") {
		t.Fatalf("usage.jsonl = %q", usage)
	}
	if !strings.Contains(usage, `"input":5,"output":2,"cache_read":1,"cache_write":0`) {
		t.Fatalf("usage.jsonl = %q", usage)
	}
}

func TestAKeyOfAnotherProfileGoesOnFd3AndNowhereElse(t *testing.T) {
	e := newFakeEnv(t)
	sentinel := "sk-SENTINEL-compactor"
	setSentinel(t, sentinel)
	s, logDir := chatServer(t, e, func(f *ForemanConfig) { f.CompactorProfile = "desk" })
	s.StartForeman()
	fc := s.ensureChat()
	longMessages(t, fc, 2)
	waitUntil(t, "the compactor", 90*time.Second, func() bool { return pendingOf(fc) == 0 })
	log := read(e.log)
	for _, want := range []string{"[agent-profiles] [bundle] [--fd] [3]", "[--wire] [openai]", "[--base-url] [http://127.0.0.1:1/v1]",
		"[--model] [desk-model]", "[--key-fd] [3]", "fd3: 21 bytes", "key: matches"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the fakes' log lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, sentinel) {
		t.Fatalf("the sentinel is in the fake's log:\n%s", log)
	}
	hits := grepTree([]string{logDir, s.foremanDir(), s.cfg.DataDir}, sentinel)
	if len(hits) != 0 {
		t.Fatalf("the sentinel reached %v", hits)
	}
	for _, n := range noteTexts(s) {
		if strings.Contains(n, sentinel) {
			t.Fatalf("the sentinel is in a note: %s", n)
		}
	}
}

func TestTheCompactorRunsOneCallAtATime(t *testing.T) {
	e := newFakeEnv(t)
	t.Setenv("FAKE_MS", "100")
	s, _ := chatServer(t, e, nil)
	s.StartForeman()
	fc := s.ensureChat()
	longMessages(t, fc, 8)
	waitUntil(t, "the compactor", 180*time.Second, func() bool { return pendingOf(fc) == 0 })
	if m := e.maxInflight(); m != 1 {
		t.Fatalf("most calls at once = %d, want 1", m)
	}
}

func TestATooLongAnswerIsAskedAgainThenFits(t *testing.T) {
	e := newFakeEnv(t)
	t.Setenv("FAKE_MODE", "longthenfit")
	s, logDir := chatServer(t, e, nil)
	s.StartForeman()
	fc := s.ensureChat()
	longMessages(t, fc, 1)
	waitUntil(t, "the compactor", 90*time.Second, func() bool { return pendingOf(fc) == 0 })
	fc.mu.Lock()
	n, _ := fc.chat.Node(foreman.NodeID{})
	fc.mu.Unlock()
	if n.Text != "a fitting line" {
		t.Fatalf("node = %q", n.Text)
	}
	if got := strings.Count(read(filepath.Join(logDir, foreman.UsageFile)), "\n"); got != 2 {
		t.Fatalf("%d calls", got)
	}
}

func TestAnAnswerThatIsAlwaysTooLongIsClippedAfterFiveCalls(t *testing.T) {
	e := newFakeEnv(t)
	t.Setenv("FAKE_MODE", "alwayslong")
	s, logDir := chatServer(t, e, nil)
	s.StartForeman()
	fc := s.ensureChat()
	longMessages(t, fc, 1)
	waitUntil(t, "the compactor", 30*time.Second, func() bool { return pendingOf(fc) == 0 })
	fc.mu.Lock()
	n, _ := fc.chat.Node(foreman.NodeID{})
	fc.mu.Unlock()
	if len(n.Text) != foreman.DefaultNodeBytes {
		t.Fatalf("node is %d bytes", len(n.Text))
	}
	if got := strings.Count(read(filepath.Join(logDir, foreman.UsageFile)), "\n"); got != 5 {
		t.Fatalf("%d calls", got)
	}
}

func TestACallThatHangsIsKilledAndTheNodeStaysPending(t *testing.T) {
	e := newFakeEnv(t)
	t.Setenv("FAKE_MODE", "slow")
	s, _ := chatServer(t, e, nil)
	s.compactTimeout = 300 * time.Millisecond
	s.compactRetry = time.Hour
	s.StartForeman()
	fc := s.ensureChat()
	longMessages(t, fc, 1)
	waitUntil(t, "the timeout note", 90*time.Second, func() bool {
		for _, n := range noteTexts(s) {
			if strings.Contains(n, "timed out") {
				return true
			}
		}
		return false
	})
	if pendingOf(fc) == 0 {
		t.Fatal("the node was built")
	}
	// The fake died with its process group.
	var pid string
	for _, l := range strings.Split(read(e.inflight), "\n") {
		if p, ok := strings.CutPrefix(l, "start "); ok {
			pid = p
		}
	}
	if pid == "" {
		t.Fatal("the fake never started")
	}
	waitUntil(t, "the fake to die", 10*time.Second, func() bool {
		_, err := os.Stat("/proc/" + pid)
		return os.IsNotExist(err)
	})
}

func TestOpeningTheChatSweepsTheOldFilesOfTurnsThatNeverEnded(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := chatServer(t, e, nil)
	dir := s.turnDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := []string{"t-3.system", "t-3.view", "t-3.sock", "t-12.system"}
	keep := []string{"t-3.other", "notes.txt", "t-9.system.keep"}
	old := time.Now().Add(-time.Hour)
	for _, n := range append(append([]string{}, stale...), keep...) {
		path := filepath.Join(dir, n)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A turn that is running now has a file this new.
	fresh := filepath.Join(dir, "t-20.sock")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "t-7.sock"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.StartForeman()
	for _, n := range stale {
		if _, err := os.Lstat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Fatalf("%s is still there (%v)", n, err)
		}
	}
	for _, n := range append(keep, "t-7.sock", "t-20.sock") {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s was removed: %v", n, err)
		}
	}
}

func TestAServerWithAnotherDataDirNeverTouchesAnotherServersTurnFiles(t *testing.T) {
	e := newFakeEnv(t)
	a, _ := chatServer(t, e, nil)
	if err := os.MkdirAll(a.turnDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(a.turnDir(), "t-5.sock")
	if err := os.WriteFile(live, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second server, its own data dir and log dir, in the same state home.
	e2 := newFakeEnv(t)
	b, _ := chatServer(t, e2, nil)
	b.StartForeman()
	if a.turnDir() == b.turnDir() {
		t.Fatalf("both servers use %s", a.turnDir())
	}
	if _, err := os.Lstat(live); err != nil {
		t.Fatalf("the first server's turn file was removed: %v", err)
	}
}
