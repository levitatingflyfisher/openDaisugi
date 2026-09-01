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

// A directory that is gone fails the spawn with a message that names the
// directory. exec's own wording blames the command, so an operator looks
// for a missing binary that is there.
func TestSpawnNamesAMissingDirectory(t *testing.T) {
	s := newPaneServer(t)
	missing := filepath.Join(t.TempDir(), "gone")
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+missing+`","cmd_argv":["/bin/sh"],"label":"x","kind":"pty"}`)
	resp := got[0]
	if resp.OK || resp.Error.Code != proto.ErrSpawnFailed {
		t.Fatalf("expected spawn_failed, got %+v", resp)
	}
	want := "the directory " + missing + " does not exist. Pick another directory."
	if !strings.Contains(resp.Error.Message, want) {
		t.Fatalf("error does not name the directory: %q, want it to contain %q", resp.Error.Message, want)
	}
}

// send-keys reaches every control key, ctrl-space among them, so a script
// or a foreman can leave a tile the way a person does.
func TestNamedKeysCoverEveryControlKey(t *testing.T) {
	for c := 'a'; c <= 'z'; c++ {
		name := "ctrl+" + string(c)
		if namedKeys[name] != string(rune(c-'a'+1)) {
			t.Errorf("%s is missing or wrong", name)
		}
	}
	for name, want := range map[string]string{
		"ctrl+space": "\x00", "shift+tab": "\x1b[Z", "home": "\x1b[H", "end": "\x1b[F",
		"pgup": "\x1b[5~", "pgdn": "\x1b[6~", "delete": "\x1b[3~", "insert": "\x1b[2~",
	} {
		if namedKeys[name] != want {
			t.Errorf("%s is %q, want %q", name, namedKeys[name], want)
		}
	}
}
