package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/pando"
	"github.com/opendaisugi/coppice/internal/steward"
)

const fakeSession = `{"type":"session","v":1,"id":"s1","harness":"sprig","cwd":"/x"}
{"type":"prompt","id":"a1","ts":1790000000,"text":"Fix the sqlite index"}
{"type":"assistant","id":"a2","ts":1790000010,"text":"I will read the file first.","toolUses":[{"id":"t1","name":"read"}]}
{"type":"tool_call","id":"a3","ts":1790000011,"toolUseId":"t1","name":"read","input":{"file_path":"db.go"},"detail":"db.go"}
{"type":"verdict","id":"a4","ts":1790000012,"toolUseId":"t1","decision":"allow"}
{"type":"tool_result","id":"a5","ts":1790000013,"toolUseId":"t1","ok":true,"summary":"package db"}
{"type":"assistant","id":"a6","ts":1790000020,"text":"Done. The index now rebuilds from files."}
`

func TestAWorkersTranscriptBecomesAChildStoreBesideTheParent(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	fc := s.ensureChat()
	if fc.problem != "" {
		t.Fatal(fc.problem)
	}
	fc.mu.Lock()
	fc.chat.Append(ctx0, pando.KindUser, "the parent line")
	fc.mu.Unlock()
	file := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(file, []byte(fakeSession), 0o600); err != nil {
		t.Fatal(err)
	}
	line := s.recordChild(fc, &workerRec{Label: "proj-1"}, file)
	if !strings.Contains(line, "child store proj-1") || !strings.Contains(line, "roots 0+4") {
		t.Fatalf("line = %q", line)
	}
	child := filepath.Join(logDir, "children", "proj-1")
	c, err := pando.OpenReadOnly(child)
	if err != nil || c.Count() != 4 {
		t.Fatalf("child: %v %v", c, err)
	}
	if m, _ := c.Message(2); m.Kind != pando.KindTool || !strings.Contains(m.Text, "read(db.go)\nok: package db") {
		t.Fatalf("tool message: %+v", m)
	}
	// The parent log did not take the child's messages.
	fc.mu.Lock()
	n := fc.chat.Count()
	fc.mu.Unlock()
	if n != 1 {
		t.Fatalf("parent has %d messages", n)
	}
	// Written again, it is not doubled.
	s.recordChild(fc, &workerRec{Label: "proj-1"}, file)
	if c2, _ := pando.OpenReadOnly(child); c2.Count() != 4 {
		t.Fatalf("doubled: %d", c2.Count())
	}
	// zoom with child reaches the word; a label cannot leave the children dir.
	got, err := s.zoomChild(fc, "proj-1", map[string]any{"id": float64(0), "n": float64(1)})
	if err != nil || got != "Fix the sqlite index" {
		t.Fatalf("zoom child = %q %v", got, err)
	}
	if _, err := s.zoomChild(fc, "../x", map[string]any{"id": float64(0), "n": float64(1)}); err == nil {
		t.Fatal("a label must not climb out")
	}
	if _, err := s.zoomChild(fc, "nobody-1", map[string]any{"id": float64(0), "n": float64(1)}); err == nil {
		t.Fatal("no such transcript")
	}
}

// Addendum B: a worker has no way to read a parent store.
func TestAWorkerHasNoMemoryTools(t *testing.T) {
	for _, bad := range []string{"zoom", "recall", "date"} {
		for _, have := range workerTools {
			if have == bad {
				t.Fatalf("a worker must not get %s", bad)
			}
		}
	}
}

func TestOldStewardDirMakesNoChatAndOneNote(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := chatServer(t, e, func(f *StewardConfig) { f.LogDir = "" })
	old := steward.LogDir(s.cfg.DataDir)
	if err := os.MkdirAll(filepath.Join(old, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	fc := s.ensureChat()
	if !strings.Contains(fc.problem, "daisugi pando migrate") || fc.chat != nil {
		t.Fatalf("problem %q chat %v", fc.problem, fc.chat)
	}
	want := filepath.Join(steward.StoreRoot(s.cfg.DataDir), "pando", "work")
	if _, err := os.Stat(want); err == nil {
		t.Fatal("no store may be made while the old dir stands")
	}
}

func TestNewStoreIsUnderPandoWork(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := chatServer(t, e, func(f *StewardConfig) { f.LogDir = "" })
	fc := s.ensureChat()
	if fc.problem != "" {
		t.Fatal(fc.problem)
	}
	want := filepath.Join(steward.StoreRoot(s.cfg.DataDir), "pando", "work")
	if fc.store != want {
		t.Fatalf("store = %s want %s", fc.store, want)
	}
	if _, err := os.Stat(filepath.Join(want, "main")); err != nil {
		t.Fatal(err)
	}
	// The chair's own dir exists too: turns and calls are written there.
	if st, err := os.Stat(steward.LogDir(s.cfg.DataDir)); err != nil || !st.IsDir() {
		t.Fatalf("the chair dir must be made: %v", err)
	}
}
