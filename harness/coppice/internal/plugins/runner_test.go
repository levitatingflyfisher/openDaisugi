package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// syncBuffer is a bytes.Buffer safe to write from the runner's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shellPolicy writes a user policy whose run is a shell script.
func shellPolicy(t *testing.T, id, script string) Plugin {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	m := `{"id":"` + id + `","kind":"policy","run":"run.sh","config":{"budget":40}}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, probs := Load([]string{root}, []string{id})
	if len(probs) != 0 || len(ps) != 1 {
		t.Fatalf("%+v", probs)
	}
	return ps[0]
}

func newRunner(t *testing.T, log *syncBuffer) *Runner {
	t.Helper()
	return &Runner{
		Socket:      "/nowhere/s.sock",
		WorkDir:     filepath.Join(t.TempDir(), "work"),
		Log:         slog.New(slog.NewTextHandler(log, nil)),
		Backoff:     func(int) time.Duration { return 5 * time.Millisecond },
		StableAfter: time.Minute,
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTheRunnerGivesUpAfterFiveTries(t *testing.T) {
	count := filepath.Join(t.TempDir(), "starts")
	p := shellPolicy(t, "flaky", "echo x >> "+count+"\nexit 3\n")
	var log syncBuffer
	r := newRunner(t, &log)
	var mu sync.Mutex
	launched, exited := 0, 0
	r.Launch = func(id string, start func() (int, error)) (int, error) {
		mu.Lock()
		launched++
		mu.Unlock()
		return start()
	}
	r.Exited = func(int) { mu.Lock(); exited++; mu.Unlock() }
	r.Start([]Plugin{p})
	defer r.Stop()
	waitFor(t, "the give-up line", func() bool { return strings.Contains(log.String(), "gave up") })
	time.Sleep(50 * time.Millisecond)
	raw, _ := os.ReadFile(count)
	if n := strings.Count(string(raw), "x"); n != 5 {
		t.Fatalf("started %d times, want 5", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if launched != 5 || exited != 5 {
		t.Fatalf("launched %d exited %d", launched, exited)
	}
	if got := strings.Count(log.String(), "plugin exited"); got != 5 {
		t.Fatalf("logged %d exits, want 5:\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "plugin=flaky") {
		t.Fatalf("%s", log.String())
	}
}

func TestAPolicyGetsItsSocketIdAndConfig(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env")
	p := shellPolicy(t, "envy", `printf '%s\n%s\n%s\n%s\n%s\n' "$COPPICE_SOCKET" "$COPPICE_SOCK" "$COPPICE_PLUGIN" "$COPPICE_PLUGIN_CONFIG" "$COPPICE_PANE" > `+out+".part && mv "+out+".part "+out+"\nsleep 30\n")
	var log syncBuffer
	r := newRunner(t, &log)
	r.Config = map[string]map[string]any{"envy": {"budget": 12, "topic": "t"}}
	t.Setenv("COPPICE_PANE", "w1:p1")
	r.Start([]Plugin{p})
	defer r.Stop()
	waitFor(t, "the env file", func() bool { _, err := os.Stat(out); return err == nil })
	raw, _ := os.ReadFile(out)
	lines := strings.Split(string(raw), "\n")
	if lines[0] != "/nowhere/s.sock" || lines[1] != "/nowhere/s.sock" || lines[2] != "envy" || lines[4] != "" {
		t.Fatalf("%q", lines)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(lines[3]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["budget"] != float64(12) || cfg["topic"] != "t" {
		t.Fatalf("%v", cfg)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Stop takes the whole policy down, the processes it started among it.
func TestStopKillsThePolicyAndWhatItStarted(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "pids")
	p := shellPolicy(t, "sticky", "sleep 60 &\necho $$ $! > "+pids+".part\nmv "+pids+".part "+pids+"\nwait\n")
	var log syncBuffer
	r := newRunner(t, &log)
	r.Start([]Plugin{p})
	waitFor(t, "the pid file", func() bool { _, err := os.Stat(pids); return err == nil })
	raw, _ := os.ReadFile(pids)
	var nums []int
	for _, f := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(f)
		nums = append(nums, n)
	}
	if len(nums) != 2 || !alive(nums[0]) || !alive(nums[1]) {
		t.Fatalf("pids %v", nums)
	}
	r.Stop()
	waitFor(t, "the policy and its child to go", func() bool { return !alive(nums[0]) && !alive(nums[1]) })
	if strings.Contains(log.String(), "gave up") {
		t.Fatalf("a stop is not a failure:\n%s", log.String())
	}
}

// A shipped policy runs from a fresh copy of its files under the work
// directory, beside the shared library, each start.
func TestAShippedPolicyRunsFromItsWrittenFiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "here")
	fsys := fstest.MapFS{
		"calm/manifest.json":   {Data: []byte(`{"id":"calm","kind":"policy","run":"calm.sh"}`)},
		"calm/calm.sh":         {Data: []byte("#!/bin/sh\npwd > " + out + ".part\nls ../_lib >> " + out + ".part\nmv " + out + ".part " + out + "\nsleep 30\n")},
		"_lib/floor_client.py": {Data: []byte("# lib\n")},
	}
	ps, probs := LoadFrom([]Source{{FS: fsys, Shipped: true}}, []string{"calm"})
	if len(probs) != 0 {
		t.Fatalf("%+v", probs)
	}
	var log syncBuffer
	r := newRunner(t, &log)
	r.Lib = mustSub(t, fsys, "_lib")
	stale := filepath.Join(r.WorkDir, "calm", "stale.txt")
	_ = os.MkdirAll(filepath.Dir(stale), 0o700)
	_ = os.WriteFile(stale, []byte("old"), 0o600)
	r.Start(ps)
	defer r.Stop()
	waitFor(t, "the policy to run", func() bool { _, err := os.Stat(out); return err == nil })
	raw, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want, _ := filepath.EvalSymlinks(filepath.Join(r.WorkDir, "calm"))
	if lines[0] != want || lines[1] != "floor_client.py" {
		t.Fatalf("%q, want dir %q", lines, want)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a stale file from an older start is still there")
	}
	for _, name := range []string{"calm", "calm/calm.sh", "calm/manifest.json", "_lib", "_lib/floor_client.py"} {
		st, err := os.Stat(filepath.Join(r.WorkDir, name))
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: mode %v %v", name, st.Mode(), err)
		}
	}
}

func TestTheRunnerStartsOnlyPolicies(t *testing.T) {
	root := writePlugins(t, "v")
	ps, _ := Load([]string{root}, []string{"v"})
	var log syncBuffer
	r := newRunner(t, &log)
	started := false
	r.Launch = func(string, func() (int, error)) (int, error) { started = true; return 0, nil }
	r.Start(ps)
	r.Stop()
	if started {
		t.Fatal("a view was started as a process")
	}
}

func TestPythonPoliciesRunUnderWhicheverPythonResolves(t *testing.T) {
	p := Plugin{Manifest: Manifest{ID: "py", Kind: KindPolicy, Run: "py.py"}}
	argv := commandFor(p, "/d")
	if len(argv) != 2 || argv[1] != "/d/py.py" || (argv[0] != "python3" && argv[0] != "python") {
		t.Fatalf("%q", argv)
	}
	p.Run = "tool"
	if strings.Join(commandFor(p, "/d"), " ") != "/d/tool" {
		t.Fatal(commandFor(p, "/d"))
	}
}

func TestPythonInterpreterPrefersPython3(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "python3" {
			return "/usr/bin/python3", nil
		}
		return "", errors.New("not found")
	}
	if got := pythonInterpreterWith(lookPath); got != "python3" {
		t.Fatalf("want python3, got %q", got)
	}
}

func TestPythonInterpreterFallsBackToPythonWhenNoPython3(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "python" {
			return "/usr/bin/python", nil
		}
		return "", errors.New("not found")
	}
	if got := pythonInterpreterWith(lookPath); got != "python" {
		t.Fatalf("want python, got %q", got)
	}
}

func TestPythonInterpreterDefaultsToPython3WhenNeitherResolves(t *testing.T) {
	lookPath := func(name string) (string, error) {
		return "", errors.New("not found")
	}
	if got := pythonInterpreterWith(lookPath); got != "python3" {
		t.Fatalf("want python3 fallback, got %q", got)
	}
}

func mustSub(t *testing.T, fsys fstest.MapFS, dir string) fs.FS {
	t.Helper()
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// A policy that exits 0 has nothing more to do, such as a notifier with no
// settings. It is not started again.
func TestAPolicyThatExitsCleanlyIsNotStartedAgain(t *testing.T) {
	count := filepath.Join(t.TempDir(), "starts")
	p := shellPolicy(t, "done", "echo x >> "+count+"\nexit 0\n")
	var log syncBuffer
	r := newRunner(t, &log)
	r.Start([]Plugin{p})
	waitFor(t, "the finished line", func() bool { return strings.Contains(log.String(), "plugin finished") })
	time.Sleep(100 * time.Millisecond)
	r.Stop()
	raw, _ := os.ReadFile(count)
	if n := strings.Count(string(raw), "x"); n != 1 {
		t.Fatalf("started %d times, want 1", n)
	}
}

// Policies start at once, side by side. None of them may remove the
// shared library while another reads it.
func TestShippedPoliciesShareTheLibraryWithoutARace(t *testing.T) {
	fsys := fstest.MapFS{"_lib/floor_client.py": {Data: []byte("# lib\n")}}
	var ids []string
	for _, id := range []string{"a", "b", "c", "d"} {
		ids = append(ids, id)
		fsys[id+"/manifest.json"] = &fstest.MapFile{Data: []byte(`{"id":"` + id + `","kind":"policy","run":"p.sh"}`)}
		fsys[id+"/p.sh"] = &fstest.MapFile{Data: []byte("#!/bin/sh\nfor i in 1 2 3 4 5 6 7 8 9 10; do test -f ../_lib/floor_client.py || exit 9; sleep 0.02; done\nsleep 30\n")}
	}
	ps, probs := LoadFrom([]Source{{FS: fsys, Shipped: true}}, ids)
	if len(probs) != 0 {
		t.Fatalf("%+v", probs)
	}
	for round := 0; round < 3; round++ {
		var log syncBuffer
		r := newRunner(t, &log)
		r.Lib = mustSub(t, fsys, "_lib")
		r.Start(ps)
		time.Sleep(400 * time.Millisecond)
		r.Stop()
		if strings.Contains(log.String(), "plugin exited") {
			t.Fatalf("round %d: a policy lost the library:\n%s", round, log.String())
		}
	}
}

// sessionChild writes a policy script that starts sleep in a process group
// of its own, in the policy's session, and writes its pid to pidFile.
func sessionChild(pidFile, after string) string {
	return "python3 -c 'import subprocess; p = subprocess.Popen([\"sleep\", \"60\"], process_group=0); open(\"" +
		pidFile + ".part\", \"w\").write(str(p.pid))'\nmv " + pidFile + ".part " + pidFile + "\n" + after
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	waitFor(t, "the pid file", func() bool { _, err := os.Stat(path); return err == nil })
	raw, _ := os.ReadFile(path)
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A process that left the policy's group but stayed in its session goes
// when the policy exits.
func TestAPolicyExitTakesItsWholeSession(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child")
	p := shellPolicy(t, "leaver", sessionChild(pidFile, "sleep 0.3\nexit 3\n"))
	var log syncBuffer
	r := newRunner(t, &log)
	r.Tries = 1
	r.Start([]Plugin{p})
	defer r.Stop()
	child := readPID(t, pidFile)
	waitFor(t, "the give-up line", func() bool { return strings.Contains(log.String(), "gave up") })
	waitFor(t, "the session member to go", func() bool { return !alive(child) || zombie(child) })
}

func TestStopTakesTheWholeSession(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child")
	p := shellPolicy(t, "stayer", sessionChild(pidFile, "sleep 60\n"))
	var log syncBuffer
	r := newRunner(t, &log)
	r.Start([]Plugin{p})
	child := readPID(t, pidFile)
	r.Stop()
	waitFor(t, "the session member to go", func() bool { return !alive(child) || zombie(child) })
}

// zombie reports whether pid has exited and waits for its parent to reap it.
func zombie(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && strings.HasPrefix(strings.TrimSpace(s[i+1:]), "Z")
}

func TestTheDefaultBackoffDoublesToThirtySeconds(t *testing.T) {
	r := &Runner{}
	want := map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second,
		5: 16 * time.Second, 6: 30 * time.Second, 40: 30 * time.Second, 70: 30 * time.Second}
	for try, d := range want {
		if got := r.backoff(try); got != d {
			t.Errorf("backoff(%d) = %v, want %v", try, got, d)
		}
	}
}

// A run that lasts past StableAfter counts as a success, so the count of
// failures in a row starts again.
func TestALongRunResetsTheFailureCount(t *testing.T) {
	count := filepath.Join(t.TempDir(), "starts")
	p := shellPolicy(t, "slow", "echo x >> "+count+"\nsleep 0.15\nexit 3\n")
	var log syncBuffer
	r := newRunner(t, &log)
	r.Tries = 2
	r.StableAfter = 50 * time.Millisecond
	r.Start([]Plugin{p})
	defer r.Stop()
	waitFor(t, "four starts", func() bool {
		raw, _ := os.ReadFile(count)
		return strings.Count(string(raw), "x") >= 4
	})
	if strings.Contains(log.String(), "gave up") {
		t.Fatalf("gave up on a policy whose runs each lasted past StableAfter:\n%s", log.String())
	}
}
