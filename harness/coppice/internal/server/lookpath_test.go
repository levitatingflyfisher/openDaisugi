package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
)

// A bare command name resolves through PATH at spawn, the way a shell
// would, so a harness installed under a user's own bin directory starts
// without the caller spelling out its path.
func TestSpawnResolvesTheCommandOnPath(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeharness")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s := newPaneServer(t)
	id := createShellPane(t, s, "fakeharness")
	if _, ok := s.Live(id); !ok {
		t.Fatal("pane not live")
	}
}

// A name PATH cannot find is a spawn failure whose message says so and
// names the two ways out.
func TestSpawnNamesPathInTheError(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"/","cmd_argv":["no-such-harness-xyz"],"label":"x","kind":"pty"}`)
	resp := got[0]
	if resp.OK || resp.Error.Code != proto.ErrSpawnFailed {
		t.Fatalf("expected spawn_failed, got %+v", resp)
	}
	want := "no-such-harness-xyz is not on PATH. Install it or give a full path."
	if !strings.Contains(resp.Error.Message, want) {
		t.Fatalf("error does not teach: %q, want it to contain %q", resp.Error.Message, want)
	}
}
