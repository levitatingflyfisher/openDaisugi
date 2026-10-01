package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// endedWorker runs one worker to its end with the fake daisugi, whose
// session is "s1", and returns the path its session file should have.
func endedWorker(t *testing.T, w *workServer) string {
	t.Helper()
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"done","reasons":[]}`)
	if got, err := w.spawn("fix the thing"); err != nil || got != "started trellis-1" {
		t.Fatalf("spawn = %q, %v", got, err)
	}
	w.waitWork(t, 1)
	return w.workerSessionPath(w.ensureChat(), "s1")
}

func writeSession(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestZoomByLabelReturnsTheNewest32KiBOfTheWorkersOwnLog(t *testing.T) {
	w := newWorkServer(t)
	path := endedWorker(t, w)
	var b strings.Builder
	for i := 0; b.Len() < 100<<10; i++ {
		fmt.Fprintf(&b, "step %06d: the worker did a thing\n", i)
	}
	writeSession(t, path, b.String())
	got, err := w.toolZoomWorker(w.ensureChat(), "trellis-1")
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := strings.Cut(got, "\n")
	if !strings.HasPrefix(head, "(the first ") || !strings.HasSuffix(b.String(), body) || len(body) > workerLogBytes || len(body) < workerLogBytes-64 {
		t.Fatalf("head %q, body %d bytes", head, len(body))
	}
	if !strings.HasPrefix(body, "step ") {
		t.Fatalf("the log does not start at a line: %.40q", body)
	}
	// A short log comes whole, with no note.
	writeSession(t, path, "one\ntwo\n")
	if got, err := w.toolZoomWorker(w.ensureChat(), "trellis-1"); err != nil || got != "one\ntwo\n" {
		t.Fatalf("short log = %q, %v", got, err)
	}
}

func TestZoomByLabelAnswersPlainlyWhenThereIsNothingToRead(t *testing.T) {
	w := newWorkServer(t)
	fc := w.ensureChat()
	if _, err := w.toolZoomWorker(fc, "nobody-1"); err == nil || !strings.Contains(err.Error(), "There are no workers") {
		t.Fatalf("no workers: %v", err)
	}
	t.Setenv("FAKE_TREE_MS", "5000")
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"done","reasons":[]}`)
	if _, err := w.spawn("slow work"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.toolZoomWorker(fc, "trellis-1"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("running: %v", err)
	}
	if _, err := w.toolZoomWorker(fc, "nobody-1"); err == nil || !strings.Contains(err.Error(), "The workers are trellis-1") {
		t.Fatalf("unknown label: %v", err)
	}
}

func TestZoomByLabelNeverReadsOutsideTheSessions(t *testing.T) {
	w := newWorkServer(t)
	fc := w.ensureChat()
	secret := filepath.Join(w.e.dir, "secret.txt")
	writeSession(t, secret, "SECRET")
	endedWorker(t, w)
	for _, session := range []string{"../../../../../secret.txt", "../../" + filepath.Base(w.e.dir) + "/secret.txt"} {
		w.workMu.Lock()
		w.work[0].SessionFile = filepath.Join(w.workDir(fc), "gate", "sessions", session)
		w.workMu.Unlock()
		if got, err := w.toolZoomWorker(fc, "trellis-1"); err == nil || strings.Contains(got, "SECRET") {
			t.Fatalf("session %q read %q, %v", session, got, err)
		}
	}
	w.workMu.Lock()
	w.work[0].SessionFile = secret
	w.workMu.Unlock()
	if got, err := w.toolZoomWorker(fc, "trellis-1"); err == nil || strings.Contains(got, "SECRET") {
		t.Fatalf("an absolute path read %q, %v", got, err)
	}
	// A link inside the sessions that points outside is not followed.
	link := w.workerSessionPath(fc, "s1")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	w.workMu.Lock()
	w.work[0].SessionFile = link
	w.workMu.Unlock()
	if got, err := w.toolZoomWorker(fc, "trellis-1"); err == nil || strings.Contains(got, "SECRET") {
		t.Fatalf("a link read %q, %v", got, err)
	}
}

func TestTheZoomToolTakesALabelOrAnIDAndAnNButNotBoth(t *testing.T) {
	w := newWorkServer(t)
	fc := w.ensureChat()
	path := endedWorker(t, w)
	writeSession(t, path, "worker step one\n")
	ts, err := w.openTurnSocket(fc, 7)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.Close)
	ts.Started(os.Getpid())
	r := toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"label": "trellis-1"}})
	if !r.OK || r.Result != "worker step one\n" {
		t.Fatalf("zoom by label = %+v", r)
	}
	// Numbers keep today's meaning.
	r = toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"id": 0, "n": 1}})
	if !r.OK {
		t.Fatalf("zoom by numbers = %+v", r)
	}
	r = toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"label": "trellis-1", "id": 0, "n": 1}})
	if r.OK || !strings.Contains(r.Error, "not both") {
		t.Fatalf("both = %+v", r)
	}
	r = toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"label": 5}})
	if r.OK {
		t.Fatalf("a label that is not text = %+v", r)
	}
	r = toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{}})
	if r.OK || !strings.Contains(r.Error, "id is missing") {
		t.Fatalf("nothing = %+v", r)
	}
}

func TestTheWorkerTableNamesTheFileSprigWrites(t *testing.T) {
	w := newWorkServer(t)
	path := endedWorker(t, w)
	if filepath.Base(path) != "s1.jsonl" {
		t.Fatalf("path = %s", path)
	}
	// sprig names the file <id>.jsonl; the table must point at that file.
	w.workMu.Lock()
	got := w.work[0].SessionFile
	w.workMu.Unlock()
	if got != path {
		t.Fatalf("table has %s, sprig writes %s", got, path)
	}
	writeSession(t, filepath.Join(filepath.Dir(path), "s1.jsonl"), "{\"type\":\"session\"}\nstep\n")
	if out, err := w.toolZoomWorker(w.ensureChat(), "trellis-1"); err != nil || !strings.HasSuffix(out, "step\n") {
		t.Fatalf("zoom = %q, %v", out, err)
	}
}

func TestAFifoInPlaceOfTheLogIsNotOpenedForBlocking(t *testing.T) {
	w := newWorkServer(t)
	path := endedWorker(t, w)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := w.toolZoomWorker(w.ensureChat(), "trellis-1"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a fifo was read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("zoom blocked on a fifo")
	}
}
