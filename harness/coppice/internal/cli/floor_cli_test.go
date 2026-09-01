package cli

import (
	"bytes"
	"strings"
	"testing"
)

// coppice with no arguments on a terminal opens the floor. q closes it and
// the exit code is 0.
func TestNoArgumentsOnATerminalOpensTheFloor(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir,
		In: strings.NewReader("q"), Out: &out, Err: &errb})
	code := c.Run(nil)
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "\x1b[H") || !strings.Contains(out.String(), "coppice ·") {
		t.Fatalf("no floor frame written: %q", out.String())
	}
}

// With nothing listening and autostart off, the floor is unreachable: exit
// 3 with the teaching message, before any screen is drawn.
func TestNoArgumentsWithNoServerExitsThree(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: dir + "/none.sock", DataDir: dir,
		In: strings.NewReader("q"), Out: &out, Err: &errb})
	if code := c.Run(nil); code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a screen was drawn with no server: %q", out.String())
	}
}

// --remote with no verb is refused the way attach refuses it: the floor
// dials a local socket, and a remote host must not be answered by the
// local one without a word.
func TestNoArgumentsWithRemoteIsRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: dir + "/none.sock", DataDir: dir,
		In: strings.NewReader("q"), Out: &out, Err: &errb})
	if code := c.Run([]string{"--remote", "ssh://host"}); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "not wired for --remote") || !strings.Contains(errb.String(), "floor") {
		t.Fatalf("stderr does not teach: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a screen was drawn: %q", out.String())
	}
	assertNoAutostartArtifacts(t, dir)
}

// The usage text names the bare command first.
func TestUsageNamesTheBareCommandFirst(t *testing.T) {
	lines := strings.Split(usage, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[2]) != "coppice                                  open the floor" {
		t.Fatalf("usage line 3 = %q", lines[2])
	}
}
