package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// Broken pool state follows the fresh-start rule (FM-R-112): the file is
// moved aside to <name>.broken-<time>, one line says where, and the foreman
// goes on without it. Nothing is deleted or overwritten.

func brokenCopies(t *testing.T, dir, base string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), base+".broken-") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func TestABadPoolsFileIsMovedAsideAndAPlainProfileStillSpawns(t *testing.T) {
	w := newWorkServer(t)
	setSentinel(t, "sk-pool")
	const junk = "this is = not [valid toml\n"
	writePools(t, junk)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	got, err := w.spawn("plain job", "desk")
	if err != nil || !strings.HasPrefix(got, "started trellis-1") {
		t.Fatalf("a profile that names no pool failed because of pools.toml: %q, %v", got, err)
	}
	dir := filepath.Dir(w.poolsPath())
	moved := brokenCopies(t, dir, foreman.PoolsFile)
	if len(moved) != 1 {
		t.Fatalf("broken copies = %v", moved)
	}
	if b, _ := os.ReadFile(moved[0]); string(b) != junk {
		t.Fatalf("the moved file changed: %q", b)
	}
	if _, err := os.Stat(w.poolsPath()); !os.IsNotExist(err) {
		t.Fatalf("the bad pools file is still in place: %v", err)
	}
	w.waitLine(t, foreman.PoolsFile+" did not read")
	w.waitLine(t, filepath.Base(moved[0]))
	// A second spawn finds no file and says nothing more.
	if _, err := w.spawn("another", "desk"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range w.workMessages(t) {
		if strings.Contains(m, "did not read") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the line came %d times", n)
	}
}

func TestAPoolsFileThatBreaksWhileJobsWaitIsMovedAsideOnTheNextLook(t *testing.T) {
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
	if got, _ := w.spawn("waits", "desk"); !strings.HasPrefix(got, "queued trellis-2") {
		t.Fatalf("got %q", got)
	}
	writePools(t, "[[[")
	w.poolPoll(w.fcOf())
	w.waitLine(t, foreman.PoolsFile+" did not read")
	if len(brokenCopies(t, filepath.Dir(w.poolsPath()), foreman.PoolsFile)) != 1 {
		t.Fatal("the bad file was not kept")
	}
}

func TestABadQueueRequestAndABadQueueFileAreMovedAsideNotDeleted(t *testing.T) {
	w := newWorkServer(t)
	writePools(t, `
[pool.desk]
model = "m1"
[[pool.desk.entries]]
name = "a"
base_url = "`+urlA+`"
`)
	in := filepath.Join(w.workDirOf(), foreman.QueueRequestDir)
	if err := os.MkdirAll(in, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "1-1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.poolPoll(w.fcOf())
	w.waitLine(t, "did not read and were moved aside")
	if len(brokenCopies(t, in, "1-1.json")) != 1 {
		t.Fatalf("the bad request was not kept: %v", brokenCopies(t, in, "1-1.json"))
	}
	// A queue file that does not read at a start is moved aside too, and the
	// background job it held is told as dropped.
	t.Setenv("FAKE_TREE_MS", "20000")
	if _, err := w.spawn("busy", "desk"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.toolSpawnOpts(w.fcOf(), "trellis", "bg wait", "desk", "", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.workDirOf(), foreman.QueueFile), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	logDir := w.logDir
	if err := w.Server.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := chatServer(t, w.e, func(f *ForemanConfig) { f.LogDir = logDir })
	s2.StartForeman()
	fc2 := s2.ensureChat()
	s2.loadWorkers(fc2)
	if len(brokenCopies(t, w.workDirOf(), foreman.QueueFile)) != 1 {
		t.Fatalf("the bad queue file was not kept: %v", brokenCopies(t, w.workDirOf(), foreman.QueueFile))
	}
	found := false
	fc2.mu.Lock()
	for i := int64(0); i < fc2.chat.Count(); i++ {
		if m, err := fc2.chat.Message(i); err == nil {
			found = found || strings.Contains(m.Text, "was dropped when coppice stopped: the queue file did not read")
		}
	}
	fc2.mu.Unlock()
	if !found {
		t.Fatal("the held background job was not told as dropped for the bad queue file")
	}
}
