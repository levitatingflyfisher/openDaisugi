package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// Two `coppice server start` invocations racing on the same data directory.
// Exactly one may hold the lock, restore, and bind. The loser must fail
// before it reads or writes anything: without the lock it would restore the
// layout, spawn its own resumed panes, write its tree back, and only then
// fail to bind the socket.
func TestTwoRacingStartsLeaveOneServerAndOneLayout(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")

	// Seed a layout with one pane record, so an overwrite by the loser would
	// be visible as a lost pane id.
	seed := layout.New()
	ws := seed.CreateWorkspace("main", dir)
	tab, err := seed.CreateTab(ws.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreatePane(ws.ID, tab.ID, layout.Pane{
		Cwd: dir, Label: "seeded", Argv: []string{"sh"}, Kind: layout.KindPTY,
	}); err != nil {
		t.Fatal(err)
	}
	layoutPath := filepath.Join(dir, "layout.json")
	if err := seed.Save(layoutPath); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		s      *Server
		locked bool
		err    error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := New(Config{SocketPath: sock, DataDir: dir})
			if err != nil {
				results[i] = outcome{err: err}
				return
			}
			<-start // release both at the same moment
			if err := s.AcquireStartLock(); err != nil {
				results[i] = outcome{s: s, err: err}
				return
			}
			// Listen before Restore: a resumed pane's gate hook dials
			// COPPICE_SOCK, and Listen's own backlog is what makes that
			// connection queue instead of refuse while Restore is still
			// running. New already registered the pane, attach and agent
			// verbs, so this test does not repeat that.
			if err := s.Listen(); err != nil {
				results[i] = outcome{s: s, locked: true, err: err}
				return
			}
			if err := s.Restore(); err != nil {
				results[i] = outcome{s: s, locked: true, err: err}
				return
			}
			go func() { _ = s.Serve() }()
			results[i] = outcome{s: s, locked: true}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	var winner *Server
	for _, r := range results {
		if r.locked && r.err == nil {
			winners++
			winner = r.s
		}
	}
	if winners != 1 {
		t.Fatalf("%d of 2 starts came up, want exactly 1", winners)
	}
	defer winner.Close()
	if !winner.HoldsStartLock() {
		t.Fatal("the winner does not hold the start lock")
	}

	// The loser must have failed at the lock, not later. Both servers live in
	// this one test process, so a pid comparison could never discriminate
	// between them; HoldsStartLock is the real discriminator.
	for _, r := range results {
		if r.s == winner {
			continue
		}
		if !errors.Is(r.err, ErrAlreadyRunning) {
			t.Fatalf("the losing start failed with %v, want ErrAlreadyRunning at the lock", r.err)
		}
		if r.locked {
			t.Fatal("the losing start held the lock, so both restored")
		}
		if r.s == nil {
			continue
		}
		if r.s.HoldsStartLock() {
			t.Fatal("the losing start holds the start lock")
		}
		if len(r.s.Tree().Panes()) != 0 {
			t.Fatal("the losing start restored a tree, so it read the layout it was refused")
		}
	}

	// layout.json was written by the winner only, and the seeded pane
	// survived.
	b, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Panes map[string]json.RawMessage `json:"panes"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("layout.json is not readable after the race: %v\n%s", err, b)
	}
	if _, ok := snap.Panes["w1:p1"]; !ok {
		t.Fatalf("the seeded pane is gone, so a start overwrote the layout:\n%s", b)
	}

	// The socket belongs to the survivor.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("nothing is listening on %s after the race: %v", sock, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, `{"id":"1","cmd":"server.status"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := proto.NewDecoder(conn).Next()
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Socket string `json:"socket"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.Socket != sock {
		t.Fatalf("the listener reports socket %q, want %q", resp.Result.Socket, sock)
	}
}
