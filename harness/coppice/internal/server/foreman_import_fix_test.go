package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// Fix round 1 of the import review (IM-R-6, 7, 10, 11, 12).

func scansOf(fc *foremanChat) int {
	fc.estMu.Lock()
	defer fc.estMu.Unlock()
	return fc.scans
}

// FM-R-83: many floor.import status calls make one scan, not one each.
func TestManyStatusCallsMakeOneScan(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := importServer(t, e, importHome(t))
	s.StartForeman()
	fc := s.ensureChat()
	// The offer's own scan has finished; a fresh one is wanted, and held open.
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	release := make(chan struct{})
	fc.estMu.Lock()
	fc.scanHold = func() { <-release }
	fc.estOK = false
	base := fc.scans
	fc.estMu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			call(t, s, map[string]any{"id": fmt.Sprint(i), "cmd": "floor.import"})
		}(i)
	}
	wg.Wait()
	close(release)
	waitUntil(t, "the scan", 10*time.Second, func() bool {
		fc.estMu.Lock()
		defer fc.estMu.Unlock()
		return fc.estOK
	})
	if got := scansOf(fc) - base; got != 1 {
		t.Fatalf("%d scans for 12 status calls, want 1", got)
	}
	r := result(t, call(t, s, map[string]any{"id": "z", "cmd": "floor.import"}))
	if r["sessions"] != float64(2) || r["messages"] != float64(3) {
		t.Fatalf("reply = %v", r)
	}
}

// IM-R-6: after No thanks, the floor's status reads no transcript.
func TestNoScanAfterADecline(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := importServer(t, e, importHome(t))
	s.StartForeman()
	fc := s.ensureChat()
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	call(t, s, map[string]any{"id": "1", "cmd": "floor.import", "action": "decline"})
	fc.estMu.Lock()
	fc.estOK = false
	base := fc.scans
	fc.estMu.Unlock()
	for i := 0; i < 3; i++ {
		r := result(t, call(t, s, map[string]any{"id": fmt.Sprint("s", i), "cmd": "floor.import"}))
		if r["state"] != "declined" || r["estimating"] != false || r["sessions"] != float64(0) {
			t.Fatalf("reply = %v", r)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := scansOf(fc) - base; got != 0 {
		t.Fatalf("%d scans after a decline", got)
	}
}

// IM-R-7: the offer says where the text goes.
func TestTheOfferSaysTheForemansModelSummarizesTheText(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := importServer(t, e, importHome(t))
	s.StartForeman()
	fc := s.ensureChat()
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	fc.mu.Lock()
	m, _ := fc.chat.Message(0)
	fc.mu.Unlock()
	if !strings.Contains(m.Text, "summarized by the foreman's model like any chat message") {
		t.Fatalf("offer = %q", m.Text)
	}
}

// IM-R-10: the poll knob is a test knob, clamped.
func TestThePollKnobIsTestOnlyAndClamped(t *testing.T) {
	s := &Server{}
	cases := []struct {
		hooks, v string
		every    time.Duration
		on       bool
	}{
		{"", "1", 5 * time.Second, true},
		{"", "off", 5 * time.Second, true},
		{"1", "1", 10 * time.Millisecond, true},
		{"1", "250", 250 * time.Millisecond, true},
		{"1", "3600000", 5 * time.Second, true},
		{"1", "off", 0, false},
		{"1", "junk", 5 * time.Second, true},
	}
	for _, c := range cases {
		t.Setenv("COPPICE_TEST_HOOKS", c.hooks)
		t.Setenv("COPPICE_IMPORT_POLL_MS", c.v)
		if every, on := s.pollEvery(); every != c.every || on != c.on {
			t.Errorf("hooks %q poll %q: %v %v, want %v %v", c.hooks, c.v, every, on, c.every, c.on)
		}
	}
}

// FM-R-78: sessions of coppice's own in the home are not offered, and a
// compactor session is not imported when the owner accepts.
func TestOwnSessionsAreNotOfferedOrImported(t *testing.T) {
	e := newFakeEnv(t)
	home := importHome(t)
	s, dir := importServer(t, e, home)
	// A compactor call that sprig left before the fix: a tmp cwd, the
	// compactor's question.
	p := filepath.Join(home, ".claude", "projects", "-tmp", "k.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"user","uuid":"k1","sessionId":"k","timestamp":"2026-03-03T10:00:00.000Z","cwd":"/tmp","message":{"role":"user","content":%q}}`+"\n",
		"Limit: 9 bytes.\n"+foreman.CompactorMarker+"\n")
	// A session whose cwd is the foreman's compactor dir.
	line += fmt.Sprintf(`{"type":"assistant","uuid":"k2","sessionId":"k","timestamp":"2026-03-03T10:00:01.000Z","cwd":%q,"message":{"role":"assistant","content":[{"type":"text","text":"sum"}]}}`+"\n",
		filepath.Join(s.compactorDir(), "x"))
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := filepath.Join(home, ".claude", "projects", "-cd", "k3.jsonl")
	_ = os.MkdirAll(filepath.Dir(p2), 0o755)
	_ = os.WriteFile(p2, []byte(fmt.Sprintf(`{"type":"user","uuid":"k3","sessionId":"k3","timestamp":"2026-03-03T10:00:00.000Z","cwd":%q,"message":{"role":"user","content":"hello"}}`+"\n",
		filepath.Join(s.cfg.DataDir, "foreman", "compactor"))), 0o644)
	s.StartForeman()
	fc := s.ensureChat()
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	fc.mu.Lock()
	m, _ := fc.chat.Message(0)
	fc.mu.Unlock()
	if !strings.Contains(m.Text, "2 past chats (about 3 messages)") {
		t.Fatalf("offer = %q", m.Text)
	}
	call(t, s, map[string]any{"id": "1", "cmd": "floor.import", "action": "accept"})
	waitUntil(t, "the import", 20*time.Second, func() bool {
		st, _ := foreman.ReadImportState(dir)
		return st.Word() == "done"
	})
	if n := countKind(fc, foreman.KindPast); n != 3 {
		t.Fatalf("%d past messages, want the 3 real ones", n)
	}
}

// FM-R-85: the progress action reads the state alone and starts no scan.
func TestProgressStartsNoScan(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := importServer(t, e, importHome(t))
	s.StartForeman()
	fc := s.ensureChat()
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	fc.estMu.Lock()
	fc.estOK = false
	base := fc.scans
	fc.estMu.Unlock()
	r := result(t, call(t, s, map[string]any{"id": "1", "cmd": "floor.import", "action": "progress"}))
	if r["state"] != "off" || r["estimating"] != false {
		t.Fatalf("reply = %v", r)
	}
	time.Sleep(100 * time.Millisecond)
	if got := scansOf(fc) - base; got != 0 {
		t.Fatalf("%d scans for a progress call", got)
	}
}

// IM-R-12 (round 2): the Import button joins the scan that runs, instead of
// parsing every transcript on its own beside it.
func TestTheImportButtonSharesTheRunningScan(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := importServer(t, e, importHome(t))
	s.StartForeman()
	fc := s.ensureChat()
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	fc.estMu.Lock()
	fc.scanHold = func() { <-release }
	fc.estOK = false
	base := fc.scans
	fc.estMu.Unlock()
	call(t, s, map[string]any{"id": "st", "cmd": "floor.import"}) // starts the scan
	waitUntil(t, "the scan starts", 10*time.Second, func() bool { return scansOf(fc)-base == 1 })
	done := make(chan map[string]any, 1)
	go func() {
		done <- result(t, call(t, s, map[string]any{"id": "ac", "cmd": "floor.import", "action": "accept"}))
	}()
	select {
	case <-done:
		t.Fatal("the accept returned while a scan was held: it scanned on its own")
	case <-time.After(300 * time.Millisecond):
	}
	open()
	select {
	case r := <-done:
		if r["state"] != "running" || r["total"] != float64(3) {
			t.Fatalf("reply = %v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the accept did not return")
	}
	// One shared scan; the rescan after the accept is the only other.
	waitUntil(t, "the rescan", 10*time.Second, func() bool {
		fc.estMu.Lock()
		defer fc.estMu.Unlock()
		return fc.estOK
	})
	if got := scansOf(fc) - base; got != 2 {
		t.Fatalf("%d scans, want 2 (the shared one and the rescan)", got)
	}
}

// Fix round 3 of the import review (F-1, F-2, F-4).

func startedImport(t *testing.T) (*Server, *foremanChat, string) {
	t.Helper()
	e := newFakeEnv(t)
	home := importHome(t)
	s, _ := importServer(t, e, home)
	s.StartForeman()
	fc := s.ensureChat()
	waitUntil(t, "the offer", 10*time.Second, func() bool { return countKind(fc, foreman.KindNote) == 1 })
	waitUntil(t, "the estimate", 10*time.Second, func() bool {
		fc.estMu.Lock()
		defer fc.estMu.Unlock()
		return fc.estOK
	})
	return s, fc, home
}

func newClaudeSession(t *testing.T, home string) {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "p", "s9.jsonl")
	line := `{"type":"user","uuid":"u9","sessionId":"s9","timestamp":"2026-04-01T10:00:00.000Z","cwd":"/w/proj","message":{"role":"user","content":"a new chat"}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// F-1: a transcript that appeared after the cached scan is still queued.
func TestAnAcceptSeesAChatWrittenAfterTheCachedScan(t *testing.T) {
	s, _, home := startedImport(t)
	newClaudeSession(t, home)
	r := result(t, call(t, s, map[string]any{"id": "ac", "cmd": "floor.import", "action": "accept"}))
	if r["total"] != float64(4) {
		t.Fatalf("reply = %v: want the new chat counted (4 messages)", r)
	}
}

// F-1: a cache older than a minute is not trusted, though no file changed.
func TestAnAcceptRescansAfterAMinute(t *testing.T) {
	s, fc, _ := startedImport(t)
	fc.estMu.Lock()
	fc.estAt = time.Now().Add(-61 * time.Second)
	base := fc.scans
	fc.estMu.Unlock()
	call(t, s, map[string]any{"id": "ac", "cmd": "floor.import", "action": "accept"})
	if got := scansOf(fc) - base; got < 1 {
		t.Fatalf("%d scans: the stale cache served the accept", got)
	}
}

// F-1: a fresh cache serves the accept with no new scan.
func TestAnAcceptUsesAFreshCache(t *testing.T) {
	s, fc, _ := startedImport(t)
	base := scansOf(fc)
	r := result(t, call(t, s, map[string]any{"id": "ac", "cmd": "floor.import", "action": "accept"}))
	if r["total"] != float64(3) {
		t.Fatalf("reply = %v", r)
	}
	// Only the rescan after the accept may run.
	waitUntil(t, "the rescan", 10*time.Second, func() bool {
		fc.estMu.Lock()
		defer fc.estMu.Unlock()
		return fc.estOK
	})
	if got := scansOf(fc) - base; got != 1 {
		t.Fatalf("%d scans, want only the rescan", got)
	}
}

// F-1: an accept that finds nothing says so.
func TestAnAcceptThatFindsNothingSaysSo(t *testing.T) {
	s, _, _ := startedImport(t)
	r := result(t, call(t, s, map[string]any{"id": "ac", "cmd": "floor.import", "action": "accept", "since": "2030-01-01"}))
	if r["note"] != "nothing new to import" {
		t.Fatalf("reply = %v", r)
	}
}

// F-2: a panic in a scan frees the flight; the next caller is not stuck.
func TestAPanickingScanFreesTheFlight(t *testing.T) {
	s, fc, _ := startedImport(t)
	for _, own := range []bool{false, true} {
		fc.estMu.Lock()
		fc.scanHold = func() { panic("scan blew up") }
		fc.estOK = false
		fc.estMu.Unlock()
		func() {
			defer func() { _ = recover() }()
			if own {
				s.scanWith(fc, fc.ctx, foreman.ImportOpts{})
			} else {
				s.estimate(fc, fc.ctx, true)
			}
		}()
		fc.estMu.Lock()
		running := fc.estRun != nil
		fc.scanHold = nil
		fc.estMu.Unlock()
		if running {
			t.Fatalf("own=%v: the flight stayed busy after a panic", own)
		}
		done := make(chan int, 1)
		go func() { done <- len(s.estimate(fc, fc.ctx, true)) }()
		select {
		case n := <-done:
			if n != 2 {
				t.Fatalf("own=%v: %d sessions", own, n)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("own=%v: the next scan waits for ever", own)
		}
	}
}

// F-4: a cancelled own scan queues nothing.
func TestACancelledOwnScanGivesNothing(t *testing.T) {
	s, fc, _ := startedImport(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ss := s.scanWith(fc, ctx, foreman.ImportOpts{Tools: true}); len(ss) != 0 {
		t.Fatalf("got %v from a cancelled scan", ss)
	}
}
