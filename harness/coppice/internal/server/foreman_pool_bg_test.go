package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// Background jobs (FM-R-97, FM-R-98): they wait for an idle entry, yield to
// foreground work by stopping their process group, and go on when the entry
// has been idle again. A queue request from the command is a background job.

func TestBackgroundJobsWaitForAnIdleEntryAndYieldToForegroundWork(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	writePools(t, `
idle_after = "2m"
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "`+urlA+`"
`)
	t.Setenv("FAKE_TREE_MS", "6000")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	// A foreground job ran just now: the entry is not idle.
	if _, err := w.spawn("fg one", "desk"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "fg one to end", 30*time.Second, func() bool { return len(w.workMessages(t)) >= 1 })
	got, err := w.toolSpawnOpts(w.fcOf(), "trellis", "bg one", "desk", "", true)
	if err != nil || !strings.HasPrefix(got, "queued trellis-2: ") {
		t.Fatalf("background spawn = %q, %v", got, err)
	}
	// Two minutes of no foreground work pass: the background job starts.
	w.clockSkew.Add(int64(2*time.Minute + time.Second))
	w.poolPump(w.fcOf())
	w.waitLine(t, "[trellis-2] started on a")
	// Foreground work arrives: the background worker is paused at once.
	got, err = w.spawn("fg two", "desk")
	if err != nil || (!strings.HasPrefix(got, "started trellis-3") && !strings.HasPrefix(got, "queued trellis-3")) {
		t.Fatalf("foreground spawn = %q, %v", got, err)
	}
	w.waitLine(t, "[trellis-2] paused on a to make room")
	pid := w.pidOf("trellis-2")
	if pid == 0 {
		t.Fatal("no pid for the paused worker")
	}
	waitUntil(t, "the group to stop", 5*time.Second, func() bool { return procState(pid) == 'T' })
	if st := w.toolStatus(); !strings.Contains(st, "trellis-2 | trellis | desk | m1 |") || !strings.Contains(st, "| paused |") {
		t.Fatalf("status = %q", st)
	}
	// Foreground ends; after idle_after the background worker goes on.
	w.waitLine(t, "[trellis-3] report")
	w.poolPump(w.fcOf())
	if procState(pid) != 'T' {
		t.Fatalf("the background worker resumed before the entry was idle")
	}
	w.clockSkew.Add(int64(2*time.Minute + time.Second))
	w.poolPump(w.fcOf())
	w.waitLine(t, "[trellis-2] resumed on a")
	w.waitLine(t, "[trellis-2] report")
}

// A yield stops the whole worker, the model process included (FM-R-107): the
// fake tree run starts a grandchild in a session of its own, as tree run
// starts sprig, and the grandchild must stop and then go on.
func TestAYieldStopsAGrandchildInItsOwnGroupAndTheResumeStartsItAgain(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	writePools(t, `
idle_after = "2m"
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "`+urlA+`"
`)
	gc := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Setenv("FAKE_TREE_GRANDCHILD", gc)
	t.Setenv("FAKE_TREE_MS", "4000")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	w.clockSkew.Add(int64(3 * time.Minute))
	if got, err := w.toolSpawnOpts(w.fcOf(), "trellis", "bg one", "desk", "", true); err != nil || !strings.HasPrefix(got, "started trellis-1") {
		t.Fatalf("background spawn = %q, %v", got, err)
	}
	var gpid int
	waitUntil(t, "the grandchild to start", 10*time.Second, func() bool {
		b, err := os.ReadFile(gc)
		if err != nil {
			return false
		}
		gpid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if gpid <= 1 || procState(gpid) == 0 {
			return false
		}
		g, _ := syscall.Getpgid(gpid)
		return g == gpid
	})
	t.Cleanup(func() { _ = syscall.Kill(gpid, syscall.SIGKILL) })
	if gpid == w.pidOf("trellis-1") {
		t.Fatal("the grandchild is the worker")
	}
	if _, err := w.spawn("fg", "desk"); err != nil {
		t.Fatal(err)
	}
	w.waitLine(t, "[trellis-1] paused on a to make room")
	waitUntil(t, "the grandchild to stop", 5*time.Second, func() bool { return procState(gpid) == 'T' })
	// The room is not free for a second foreground job while it is stopped
	// only in part: both stay counted by the state, and the leader is stopped too.
	if st := procState(w.pidOf("trellis-1")); st != 'T' {
		t.Fatalf("the leader state = %c", st)
	}
	w.waitLine(t, "[trellis-2] report")
	w.clockSkew.Add(int64(3 * time.Minute))
	w.poolPump(w.fcOf())
	w.waitLine(t, "[trellis-1] resumed on a")
	waitUntil(t, "the grandchild to go on", 5*time.Second, func() bool { s := procState(gpid); return s != 'T' && s != 0 })
}

// A stream that fails the moment a paused job goes on is the pause, not the
// entry (FM-R-107): the entry does not cool, and the job runs again.
func TestAFailureRightAfterAResumeDoesNotCoolTheEntry(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	writePools(t, `
idle_after = "2m"
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "`+urlA+`"
`)
	t.Setenv("FAKE_TREE_MS", "1500")
	t.Setenv("FAKE_TREE_ERR", "anthropic api 429: rate_limit_error")
	t.Setenv("FAKE_TREE_EXIT", "1")
	t.Setenv("FAKE_TREE_ERR_IF", "bg one")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	w.clockSkew.Add(int64(3 * time.Minute))
	if got, err := w.toolSpawnOpts(w.fcOf(), "trellis", "bg one", "desk", "", true); err != nil || !strings.HasPrefix(got, "started trellis-1") {
		t.Fatalf("background spawn = %q, %v", got, err)
	}
	if _, err := w.spawn("fg", "desk"); err != nil {
		t.Fatal(err)
	}
	w.waitLine(t, "[trellis-1] paused on a to make room")
	w.waitLine(t, "[trellis-2] report")
	w.clockSkew.Add(int64(3 * time.Minute))
	w.poolPump(w.fcOf())
	w.waitLine(t, "[trellis-1] resumed on a")
	w.waitLine(t, "[trellis-1] stopped right after it resumed")
	for _, e := range w.queue(t).Entries {
		if e.State == "cooling" {
			t.Fatalf("the entry cooled after a failure that came with the resume: %+v", e)
		}
	}
}

// A paused job whose pool or entry is removed from pools.toml goes on with
// one line; it is not left stopped for good (FM-R-108).
func TestAPausedJobWhosePoolOrEntryIsRemovedGoesOn(t *testing.T) {
	for _, tc := range []struct{ name, after string }{
		{"pool", "[pool.other]\nmodel = \"m1\"\n[[pool.other.entries]]\nname = \"a\"\nbase_url = \"" + urlA + "\"\n"},
		{"entry", "[pool.desk]\nmodel = \"m1\"\n[[pool.desk.entries]]\nname = \"b\"\nbase_url = \"" + urlB + "\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkServer(t)
			setSentinel(t, "sk-pool")
			writePools(t, `
idle_after = "2m"
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "`+urlA+`"
`)
			gc := filepath.Join(t.TempDir(), "grandchild.pid")
			t.Setenv("FAKE_TREE_GRANDCHILD", gc)
			t.Setenv("FAKE_TREE_MS", "20000")
			w.clockSkew.Add(int64(3 * time.Minute))
			if got, err := w.toolSpawnOpts(w.fcOf(), "trellis", "bg one", "desk", "", true); err != nil || !strings.HasPrefix(got, "started trellis-1") {
				t.Fatalf("background spawn = %q, %v", got, err)
			}
			var gpid int
			waitUntil(t, "the grandchild", 10*time.Second, func() bool {
				b, err := os.ReadFile(gc)
				if err != nil {
					return false
				}
				gpid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				g, _ := syscall.Getpgid(gpid)
				return gpid > 1 && g == gpid
			})
			t.Cleanup(func() { _ = syscall.Kill(gpid, syscall.SIGKILL) })
			if _, err := w.spawn("fg", "desk"); err != nil {
				t.Fatal(err)
			}
			w.waitLine(t, "[trellis-1] paused on a to make room")
			waitUntil(t, "the stop", 5*time.Second, func() bool { return procState(gpid) == 'T' })
			writePools(t, tc.after)
			w.poolPump(w.fcOf())
			w.waitLine(t, "[trellis-1] resumed on a: it is no longer in "+foreman.PoolsFile)
			waitUntil(t, "the grandchild to go on", 5*time.Second, func() bool { s := procState(gpid); return s != 'T' && s != 0 })
			// The pool comes back: the job that went on is still not paused again.
			writePools(t, `
idle_after = "2m"
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "`+urlA+`"
`)
			set, _ := foreman.LoadPools(w.poolsPath())
			w.workMu.Lock()
			v := w.yieldLocked(set.Pools["desk"], w.clock())
			w.workMu.Unlock()
			if v != nil {
				t.Fatalf("a job that was resumed in place was paused again: %s", v.Label)
			}
			if procState(gpid) == 'T' {
				t.Fatal("the grandchild stopped again")
			}
		})
	}
}

// A new run under a label must keep its pid when the old run ends (FM-R-108).
func TestAnOldRunsEndDoesNotForgetTheNewRunsPid(t *testing.T) {
	w := newWorkServer(t)
	w.workMu.Lock()
	w.pool.pids = map[string]int{"trellis-1": 200}
	w.workMu.Unlock()
	w.poolForget("trellis-1", 100) // the old run ends after the new one started
	if got := w.pidOf("trellis-1"); got != 200 {
		t.Fatalf("pid = %d, want the new run's 200", got)
	}
	w.poolForget("trellis-1", 200)
	if got := w.pidOf("trellis-1"); got != 0 {
		t.Fatalf("pid = %d after its own end", got)
	}
}

func TestAQueueRequestFromTheCommandBecomesABackgroundJob(t *testing.T) {
	w := newWorkServer(t)
	writePools(t, twoEntries)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := foreman.WriteQueueRequest(w.workDirOf(), foreman.QueueRequest{Project: "trellis", Brief: "tidy up", Profile: "desk"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.poolPoll(w.fcOf())
	w.waitLine(t, "[queue add] started trellis-1 on a")
	w.waitLine(t, "[trellis-1] report\nok")
	if entries, _ := os.ReadDir(filepath.Join(w.workDirOf(), foreman.QueueRequestDir)); len(entries) != 0 {
		t.Fatalf("the request was not taken: %v", entries)
	}
}

func TestAForegroundJobDoesNotWaitAcrossARestartButABackgroundJobDoes(t *testing.T) {
	w := newWorkServer(t)
	writePools(t, `
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "`+urlA+`"
`)
	t.Setenv("FAKE_TREE_MS", "20000")
	if _, err := w.spawn("busy", "desk"); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.spawn("fg wait", "desk"); !strings.HasPrefix(got, "queued trellis-2") {
		t.Fatalf("got %q", got)
	}
	if got, _ := w.toolSpawnOpts(w.fcOf(), "trellis", "bg wait", "desk", "", true); !strings.HasPrefix(got, "queued trellis-3") {
		t.Fatalf("got %q", got)
	}
	logDir := w.logDir
	if err := w.Server.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := chatServer(t, w.e, func(f *ForemanConfig) { f.LogDir = logDir })
	s2.StartForeman()
	fc := s2.ensureChat()
	s2.loadWorkers(fc)
	st := s2.toolStatus()
	if !strings.Contains(st, "trellis-1 |") || !strings.Contains(st, "| stopped |") {
		t.Fatalf("status = %q", st)
	}
	var rows = map[string]string{}
	for _, l := range strings.Split(st, "\n") {
		f := strings.Split(l, " | ")
		rows[f[0]] = f[5]
	}
	if rows["trellis-2"] != "stopped" || rows["trellis-3"] != "queued" {
		t.Fatalf("states after the restart: %v", rows)
	}
}

func procState(pid int) byte {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	return s[i+2]
}
