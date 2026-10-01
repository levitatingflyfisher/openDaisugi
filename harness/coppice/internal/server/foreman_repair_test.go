package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// U3: a bad tree line is repaired at open, not moved aside, so the call log
// and a dangling spawn start survive it. The start is noted once and is
// never run again.
func TestADanglingStartSurvivesATreeRepair(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	s.StartForeman()
	fc := s.ensureChat()
	appendLog(t, s, foreman.KindUser, "start the worker")
	if err := foreman.AppendCall(fc.dir, foreman.CallRecord{Call: "c0.1", Phase: foreman.CallStart, Turn: 0, Tool: "spawn",
		Args: `{"project":"web","brief":"fix it"}`, At: "2026-10-13T10:00:03Z"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(logDir, "tree", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("tree files = %v", files)
	}
	if err := os.WriteFile(files[0], []byte("not a node\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, _ := chatServer(t, e, func(f *ForemanConfig) { f.LogDir = logDir })
	s2.StartForeman()
	fc2 := s2.ensureChat()
	if fc2.problem != "" || fc2.chat == nil {
		t.Fatalf("problem: %s", fc2.problem)
	}
	if moved, _ := filepath.Glob(logDir + ".broken-*"); len(moved) != 0 {
		t.Fatalf("the history moved: %v", moved)
	}
	var notes []string
	fc2.mu.Lock()
	for i := int64(0); i < fc2.chat.Count(); i++ {
		if m, _ := fc2.chat.Message(i); m.Kind == foreman.KindNote {
			notes = append(notes, m.Text)
		}
	}
	fc2.mu.Unlock()
	spawnNotes, repairNotes := 0, 0
	for _, n := range notes {
		if strings.HasPrefix(n, "spawn(") {
			spawnNotes++
		}
		if strings.HasPrefix(n, "Foreman tree repaired") {
			repairNotes++
		}
	}
	if spawnNotes != 1 || repairNotes != 1 {
		t.Fatalf("notes = %q", notes)
	}
	recs, _ := foreman.ReadCalls(logDir)
	starts := 0
	for _, r := range recs {
		if r.Phase == foreman.CallStart {
			starts++
		}
	}
	if d := foreman.DanglingCalls(recs); len(d) != 0 || starts != 1 {
		t.Fatalf("dangling %+v, starts %d: a spawn ran again", d, starts)
	}
}

// 3.8: the zoom tool takes offset and size, and a bad id answers with the
// root ids.
func TestZoomToolPagesAndHintsRoots(t *testing.T) {
	_, fc, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	fc.mu.Lock()
	ids, err := fc.chat.Append(ctx0, foreman.KindUser, "0123456789abcdefghij")
	fc.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r := toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"id": ids[0], "n": 1, "offset": 8, "size": 8}})
	if !r.OK || r.Result != "89abcdef\n[bytes 8-16 of 20. More: offset 16]" {
		t.Fatalf("zoom page = %+v", r)
	}
	r = toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"id": ids[0] + 50, "n": 1}})
	if r.OK || !strings.Contains(r.Error, "The root nodes are ") {
		t.Fatalf("zoom bad id = %+v", r)
	}
	r = toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"id": ids[0], "n": 1, "size": "x"}})
	if r.OK {
		t.Fatalf("zoom bad size = %+v", r)
	}
}
