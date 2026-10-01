package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/foreman"
)

// Pools and the queue (FM-R-93 to FM-R-101). The fake daisugi stands in for
// every model: it logs the base URLs of the bundle it was given and fails
// for the one URL FAKE_FAIL_URL names. No test dials a model.

const (
	urlA = "http://127.0.0.1:11/v1"
	urlB = "http://127.0.0.1:12/v1"
	urlL = "http://127.0.0.1:13/v1"
)

func writePools(t *testing.T, text string) {
	t.Helper()
	p := filepath.Join(filepath.Dir(config.Path()), foreman.PoolsFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

const twoEntries = `
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "` + urlA + `"
[[pool.desk.entries]]
name = "b"
base_url = "` + urlB + `"
`

func (w *workServer) workDirOf() string { return filepath.Join(w.logDir, "work") }

func (w *workServer) queue(t *testing.T) foreman.QueueState {
	t.Helper()
	q, err := foreman.ReadQueue(w.workDirOf())
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func (w *workServer) waitLine(t *testing.T, sub string) {
	t.Helper()
	waitUntil(t, "the line "+sub, 60*time.Second, func() bool {
		for _, m := range w.workMessages(t) {
			if strings.Contains(m, sub) {
				return true
			}
		}
		return false
	})
}

func TestASpawnOnAPoolTakesAnEntryWithAFreeSlotElseWaitsInTheQueue(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	writePools(t, twoEntries)
	t.Setenv("FAKE_TREE_MS", "1500")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	for i, want := range []string{"started trellis-1 on a", "started trellis-2 on b"} {
		got, err := w.spawn("job", "desk")
		if err != nil || got != want {
			t.Fatalf("spawn %d = %q, %v; want %q", i, got, err, want)
		}
	}
	got, err := w.spawn("job", "desk")
	if err != nil || !strings.HasPrefix(got, "queued trellis-3: ") {
		t.Fatalf("the third spawn = %q, %v", got, err)
	}
	q := w.queue(t)
	if len(q.Jobs) != 3 || q.Jobs[2].Label != "trellis-3" || q.Jobs[2].State != "waiting" {
		t.Fatalf("queue = %+v", q.Jobs)
	}
	if !strings.Contains(w.toolStatus(), "trellis-3 | trellis | desk | m1 |") || !strings.Contains(w.toolStatus(), "| queued |") {
		t.Fatalf("status = %q", w.toolStatus())
	}
	w.waitLine(t, "[trellis-3] started on")
	w.waitWork(t, 4)
	log := read(w.e.log)
	// Each worker's bundle carries the base URL of the entry it got, and the
	// pool's one model.
	for _, want := range []string{urlA, urlB, "--profile=desk", "--model=m1"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the fake's log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "sk-pool") {
		t.Fatalf("the key is in the log:\n%s", log)
	}
}

func TestAPoolServesOneModelOnly(t *testing.T) {
	w := newWorkServer(t)
	writePools(t, twoEntries)
	_, err := w.spawn("job", "desk", "other")
	if err == nil || !strings.Contains(err.Error(), "the pool desk serves the model m1 only") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("err = %v", err)
	}
	if w.toolStatus() != "no workers" {
		t.Fatalf("a refused spawn made a worker: %q", w.toolStatus())
	}
}

func TestAnEntryThatDoesNotAnswerIsSkippedAndRecheckedWithBackOff(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	writePools(t, twoEntries)
	t.Setenv("FAKE_FAIL_URL", urlA)
	t.Setenv("FAKE_FAIL_ERR", "dial tcp 127.0.0.1:11: connect: connection refused")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if got, err := w.spawn("job", "desk"); err != nil || got != "started trellis-1 on a" {
		t.Fatalf("spawn = %q, %v", got, err)
	}
	w.waitLine(t, "[trellis-1] a did not answer")
	msgs := w.waitWork(t, 2)
	if !strings.Contains(strings.Join(msgs, "\n"), "[trellis-1] report\nok") {
		t.Fatalf("messages = %q", msgs)
	}
	var a foreman.QueueEntry
	for _, e := range w.queue(t).Entries {
		if e.Name == "a" {
			a = e
		}
	}
	if a.State != "cooling" || a.Reason != "cannot connect" || a.Until == "" {
		t.Fatalf("entry a = %+v", a)
	}
	// While a cools, a new job goes to b.
	if got, _ := w.spawn("job", "desk"); got != "started trellis-2 on b" {
		t.Fatalf("spawn = %q", got)
	}
	w.waitWork(t, 3)
	// Past the back-off the entry is tried again, and a second failure waits
	// longer than the first.
	w.clockSkew.Add(int64(foreman.ConnCoolStart + time.Second))
	if got, _ := w.spawn("job", "desk"); got != "started trellis-3 on a" {
		t.Fatalf("spawn after the back-off = %q", got)
	}
	w.waitLine(t, "[trellis-3] a did not answer")
	var a2 foreman.QueueEntry
	for _, e := range w.queue(t).Entries {
		if e.Name == "a" {
			a2 = e
		}
	}
	u2, _ := time.Parse(foreman.DateLayout, a2.Until)
	if got := u2.Sub(w.clock()); got < foreman.ConnCoolStart+10*time.Second {
		t.Fatalf("the second back-off is %s, no longer than the first", got)
	}
}

func TestWhenEveryAccountOfTheModelIsOutTheJobIsRequeuedAndALocalPoolGoesOn(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	writePools(t, twoEntries+`
[pool.free]
model = "free-model"
[[pool.free.entries]]
name = "l"
base_url = "`+urlL+`"
`)
	// Both cloud entries answer with a quota text; the local one is fine.
	t.Setenv("FAKE_FAIL_URL", urlA+" "+urlB)
	t.Setenv("FAKE_FAIL_ERR", "anthropic api 429: rate_limit_error")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job", "desk"); err != nil {
		t.Fatal(err)
	}
	w.waitLine(t, "[trellis-1] a is out of quota")
	w.waitLine(t, "[trellis-1] paused: every account of the model m1 is out")
	q := w.queue(t)
	if len(q.Jobs) != 1 || q.Jobs[0].State != "waiting" {
		t.Fatalf("queue = %+v", q.Jobs)
	}
	// The local model keeps going.
	if got, err := w.spawn("local job", "free"); err != nil || got != "started trellis-2 on l" {
		t.Fatalf("local spawn = %q, %v", got, err)
	}
	w.waitLine(t, "[trellis-2] report")
}

func TestAWorkerIsNeverMovedToAnotherModel(t *testing.T) {
	w := newWorkServer(t)
	writePools(t, twoEntries+`
[pool.free]
model = "free-model"
[[pool.free.entries]]
name = "l"
base_url = "`+urlL+`"
`)
	t.Setenv("FAKE_FAIL_URL", urlA)
	t.Setenv("FAKE_FAIL_ERR", "rate_limit_error")
	t.Setenv("FAKE_TREE_MS", "300")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job", "desk"); err != nil {
		t.Fatal(err)
	}
	w.waitLine(t, "[trellis-1] report")
	for _, l := range strings.Split(read(w.e.log), "\n") {
		if strings.HasPrefix(l, "run ids:") && !strings.Contains(l, "--model=m1") {
			t.Fatalf("a run left the model: %s", l)
		}
	}
	if strings.Contains(read(w.e.log), urlL) {
		t.Fatalf("a worker of the desk pool reached the free pool's entry")
	}
}

func (w *workServer) fcOf() *foremanChat { return w.Server.ensureChat() }

func (w *workServer) pidOf(label string) int {
	w.workMu.Lock()
	defer w.workMu.Unlock()
	return w.pool.pids[label]
}

func TestTheMachinesViewShowsAPoolEntryThatDoesNotAnswer(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	writePools(t, twoEntries)
	t.Setenv("FAKE_FAIL_URL", urlA)
	t.Setenv("FAKE_FAIL_ERR", "dial tcp 127.0.0.1:11: connect: connection refused")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("job", "desk"); err != nil {
		t.Fatal(err)
	}
	w.waitLine(t, "[trellis-1] a did not answer")
	w.waitWork(t, 2)
	t.Setenv("PATH", t.TempDir())
	m := result(t, roundTrip(t, w.Server, `{"id":"1","cmd":"floor.machines"}`)[0])
	pools, _ := m["pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("pools = %v", m["pools"])
	}
	a := pools[0].(map[string]any)
	if a["name"] != "a" || a["state"] != "cooling" || a["reason"] != "cannot connect" {
		t.Fatalf("entry a = %v", a)
	}
}
