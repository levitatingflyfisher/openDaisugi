package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// Fake transcripts only, in a scratch home: two Claude Code messages and
// one Codex message that count.
func importHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	write := func(path string, lines ...string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", "projects", "p", "s1.jsonl"),
		`{"type":"user","uuid":"u1","sessionId":"s1","timestamp":"2026-03-01T10:00:00.000Z","cwd":"/w/proj","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"s1","timestamp":"2026-03-01T10:00:05.000Z","cwd":"/w/proj","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	write(filepath.Join(home, ".codex", "sessions", "2026", "03", "05", "rollout-x.jsonl"),
		`{"timestamp":"2026-03-05T08:00:00.000Z","type":"session_meta","payload":{"id":"c1","cwd":"/w/o"}}`,
		`{"timestamp":"2026-03-05T08:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"write a test"}]}}`)
	return home
}

func importServer(t *testing.T, e *fakeEnv, home string) (*Server, string) {
	t.Helper()
	s, dir := chatServer(t, e, func(f *ForemanConfig) { f.ImportHome = home })
	s.importPoll = 20 * time.Millisecond
	return s, dir
}

func countKind(fc *foremanChat, kind string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	n := 0
	for i := int64(0); i < fc.chat.Count(); i++ {
		if m, err := fc.chat.Message(i); err == nil && m.Kind == kind {
			n++
		}
	}
	return n
}

func TestAnImportIsOfferedOnceAndNeverStartedByTheServer(t *testing.T) {
	e := newFakeEnv(t)
	home := importHome(t)
	s, dir := importServer(t, e, home)
	s.StartForeman()
	fc := s.ensureChat()
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	fc.mu.Lock()
	m, _ := fc.chat.Message(0)
	fc.mu.Unlock()
	if !strings.Contains(m.Text, "2 past chats (about 3 messages)") || !strings.Contains(m.Text, "coppice foreman import") {
		t.Fatalf("offer = %q", m.Text)
	}
	time.Sleep(200 * time.Millisecond)
	if countKind(fc, foreman.KindPast) != 0 {
		t.Fatal("the server imported without being asked")
	}
	if st, _ := foreman.ReadImportState(dir); st.Word() != "off" || !st.Offered {
		t.Fatalf("state = %+v", st)
	}
	_ = s.Close()
	// A second start does not offer again.
	s2, _ := chatServer(t, newFakeEnv(t), func(f *ForemanConfig) { f.ImportHome, f.LogDir = home, dir })
	s2.StartForeman()
	fc2 := s2.ensureChat()
	time.Sleep(300 * time.Millisecond)
	if n := countKind(fc2, foreman.KindNote); n != 1 {
		t.Fatalf("%d notes after the second start, want the one offer", n)
	}
}

func TestNoOfferWhenThereAreNoOldChats(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := importServer(t, e, t.TempDir())
	s.StartForeman()
	fc := s.ensureChat()
	time.Sleep(300 * time.Millisecond)
	if fc.chat.Count() != 0 {
		t.Fatalf("%d messages with nothing to import", fc.chat.Count())
	}
}

func TestFloorImportAcceptFeedsTheQueueAndPauseHolds(t *testing.T) {
	e := newFakeEnv(t)
	s, dir := importServer(t, e, importHome(t))
	s.StartForeman()
	fc := s.ensureChat()
	// Paused before the first message: the click is held back.
	if err := foreman.SetPaused(dir, true); err != nil {
		t.Fatal(err)
	}
	r := result(t, call(t, s, map[string]any{"id": "1", "cmd": "floor.import", "action": "accept"}))
	if r["state"] != "paused" && r["state"] != "running" {
		t.Fatalf("reply = %v", r)
	}
	// accept clears a pause (the owner just chose); pause again at once.
	call(t, s, map[string]any{"id": "2", "cmd": "floor.import", "action": "pause"})
	time.Sleep(300 * time.Millisecond)
	before := countKind(fc, foreman.KindPast)
	time.Sleep(300 * time.Millisecond)
	if after := countKind(fc, foreman.KindPast); after != before {
		t.Fatalf("messages grew while paused: %d to %d", before, after)
	}
	r = result(t, call(t, s, map[string]any{"id": "3", "cmd": "floor.import", "action": "resume"}))
	if r["paused"] != false {
		t.Fatalf("resume = %v", r)
	}
	waitUntil(t, "the import", 20*time.Second, func() bool { return countKind(fc, foreman.KindPast) == 3 && pendingOf(fc) == 0 })
	waitUntil(t, "done", 5*time.Second, func() bool {
		st, _ := foreman.ReadImportState(dir)
		return st.Word() == "done" && st.Done == 3 && st.Total == 3
	})
	// The floor's chat leaves past messages out unless asked.
	got := result(t, call(t, s, map[string]any{"id": "4", "cmd": "floor.chat"}))
	// The offer note may be there; no past message is.
	for _, m := range got["messages"].([]any) {
		if role := m.(map[string]any)["role"]; role != "note" {
			t.Fatalf("floor.chat shows a %v line, want only the offer note: %v", role, m)
		}
	}
}

func TestImportWaitsWhileATurnRuns(t *testing.T) {
	e := newFakeEnv(t)
	s, dir := importServer(t, e, importHome(t))
	s.StartForeman()
	fc := s.ensureChat()
	s.talkMu.Lock()
	s.turns.running = true
	s.talkMu.Unlock()
	call(t, s, map[string]any{"id": "1", "cmd": "floor.import", "action": "accept"})
	time.Sleep(400 * time.Millisecond)
	if n := countKind(fc, foreman.KindPast); n != 0 {
		t.Fatalf("%d messages imported while a turn ran", n)
	}
	s.talkMu.Lock()
	s.turns.running = false
	s.talkMu.Unlock()
	waitUntil(t, "the import after the turn", 20*time.Second, func() bool {
		st, _ := foreman.ReadImportState(dir)
		return st.Word() == "done"
	})
}

func TestDecliningIsRememberedByTheServer(t *testing.T) {
	e := newFakeEnv(t)
	s, dir := importServer(t, e, importHome(t))
	s.StartForeman()
	r := result(t, call(t, s, map[string]any{"id": "1", "cmd": "floor.import", "action": "decline"}))
	if r["state"] != "declined" {
		t.Fatalf("reply = %v", r)
	}
	if st, _ := foreman.ReadImportState(dir); st.Word() != "declined" {
		t.Fatalf("state = %s", st.Word())
	}
	// Choosing later still works.
	r = result(t, call(t, s, map[string]any{"id": "2", "cmd": "floor.import", "action": "accept"}))
	if r["state"] != "running" && r["state"] != "done" {
		t.Fatalf("accept after a decline = %v", r)
	}
}

func TestFloorImportRefusesAPaneAndPaneMode(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := importServer(t, e, importHome(t))
	got := roundTrip(t, s, `{"id":"1","cmd":"hello","role":"pane","pane":"w1:p1"}`, `{"id":"2","cmd":"floor.import","action":"accept"}`)
	if got[1].OK || got[1].Error == nil || got[1].Error.Message != ImportRefusal {
		t.Fatalf("reply = %+v", got[1])
	}
	p := newTestServer(t)
	if r := call(t, p, map[string]any{"id": "3", "cmd": "floor.import"}); r.OK || !strings.Contains(r.Error.Message, "chat mode") {
		t.Fatalf("pane mode reply = %+v", r)
	}
}
