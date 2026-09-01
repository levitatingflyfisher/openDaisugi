package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/server"
)

// registerAll wires what server.New cannot: server.stop, pane.explain and the
// manifest tick. This proves pane.explain answers instead of "no command",
// and that Close still shuts everything down cleanly afterward.
func TestRegisterAllWiresExplainAndTheTick(t *testing.T) {
	dir := t.TempDir()
	s, err := server.New(server.Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	// Registered right after the lock, not only at the end of this test: a
	// t.Fatal anywhere below must not leave a listening server, its start
	// lock and its manifest tick alive for the rest of the process.
	t.Cleanup(func() { _ = s.Close() })

	if err := registerAll(s); err != nil {
		t.Fatal(err)
	}
	// The tick half of registerAll's own promise: a mutation that deletes
	// StartManifestTick from registerAll makes this fail, where checking
	// only that Close does not hang would not - Close stops a tick if one
	// exists and is an equally harmless no-op if one never started.
	if !s.ManifestTickRunning() {
		t.Fatal("registerAll did not start the manifest tick")
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	c, err := Dial(filepath.Join(dir, "server.sock"), dir)
	if err != nil {
		t.Fatal(err)
	}
	_, doErr := c.Do("pane.explain", map[string]any{"pane": "w9:p9"})
	_ = c.Close()
	if doErr == nil {
		t.Fatal("pane.explain on a nonexistent pane returned no error, want no_such_pane or pane_closed")
	}
	if strings.Contains(doErr.Error(), `no command "pane.explain"`) {
		t.Fatalf("pane.explain answered %q, registerAll did not wire it", doErr)
	}

	// Close must stop the manifest tick registerAll started, not just leave
	// it running past this Server's own shutdown. A tick goroutine that
	// StartManifestTick left behind would not by itself make Close hang -
	// Close stops it directly - but a bare "Close returned no error" proves
	// nothing about whether that shutdown actually happened, only that
	// nothing panicked. Two overlapping Close calls both returning promptly
	// is the same idempotent-and-clean path server.status's own suite
	// exercises for the tick elsewhere; a hang here would mean the second
	// call is blocked on teardown the first one left unfinished.
	done := make(chan error, 2)
	go func() { done <- s.Close() }()
	go func() { done <- s.Close() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Close did not return within 3s; the manifest tick may still be running")
		}
	}
	if s.ManifestTickRunning() {
		t.Fatal("ManifestTickRunning is still true after Close")
	}
}

// detect.LoadSet measured against a broken override manifest returns
// err=nil, not an error - it only appends a warning, since both Set.add and
// Set.warn fail soft. registerAll's own explain-stub fallback, the "agent
// detection did not load" branch, is therefore unreachable through any
// override-directory content: it can only fire if the bundled, go:embed'd,
// manifests themselves fail to parse, which no override directory, broken
// or otherwise, can trigger. Not tested here as a broken-override scenario
// for that reason.
