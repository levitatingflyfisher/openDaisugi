package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

// savedConfig points XDG_CONFIG_HOME at a temp dir and writes a config
// there with claude as the default, so the floor opens without a first
// run.
func savedConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "claude",
		Harness: map[string]config.Harness{"claude": {Command: "claude", State: "hooks"}}}); err != nil {
		t.Fatal(err)
	}
}

// emptyPath points PATH at a temp dir that holds no executable at all.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// fakeHarnessOnPath points PATH at a temp dir holding one executable
// shell script named name that sleeps. It is never the real harness.
func fakeHarnessOnPath(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return script
}

// coppice with no arguments on a terminal opens the floor. q closes it and
// the exit code is 0.
func TestNoArgumentsOnATerminalOpensTheFloor(t *testing.T) {
	sock, dir := liveServer(t)
	savedConfig(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir,
		In: strings.NewReader("\x03"), Out: &out, Err: &errb})
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: dir + "/none.sock", DataDir: dir,
		In: strings.NewReader("\x03"), Out: &out, Err: &errb})
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: dir + "/none.sock", DataDir: dir,
		In: strings.NewReader("\x03"), Out: &out, Err: &errb})
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

// With no config file and no harness on PATH, the floor cannot open. It
// says that the apps cannot be owned and exits 1, before any screen.
func TestNoConfigAndNoHarnessOnPathIsHonest(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	emptyPath(t)
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir,
		In: strings.NewReader("\x03"), Out: &out, Err: &errb})
	if code := c.Run(nil); code != 1 {
		t.Fatalf("exit %d, want 1: %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String()+errb.String(), "apps") {
		t.Fatalf("the refusal does not name the apps: %q %q", out.String(), errb.String())
	}
	if strings.Contains(out.String(), "\x1b[H") {
		t.Fatalf("a screen was drawn: %q", out.String())
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config file was written with nothing found")
	}
}

// With no config file and one fake harness on PATH, the floor writes the
// config without a question and opens with that harness as the default.
func TestNoConfigAndOneHarnessOnPathWritesTheFileAndOpens(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeHarnessOnPath(t, "claude")
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir,
		In: strings.NewReader("\x03"), Out: &out, Err: &errb})
	if code := c.Run(nil); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "Which") {
		t.Fatalf("asked a question with one harness: %q", out.String())
	}
	if !strings.Contains(out.String(), "claude is your default.") {
		t.Fatalf("no first-run line: %q", out.String())
	}
	got, ok, err := config.Load()
	if err != nil || !ok || got.Default != "claude" {
		t.Fatalf("config after the first run: %+v %v %v", got, ok, err)
	}
	if !strings.Contains(out.String(), "coppice ·") {
		t.Fatalf("no floor frame written: %q", out.String())
	}
}

// A config file that does not parse stops the floor with the parse error.
// It is never treated as missing, so a first run never overwrites it.
func TestABrokenConfigStopsTheFloorWithoutOverwritingIt(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeHarnessOnPath(t, "claude")
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := []byte("default = [unclosed\n")
	if err := os.WriteFile(config.Path(), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir,
		In: strings.NewReader("\x03"), Out: &out, Err: &errb})
	if code := c.Run(nil); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), config.Path()) {
		t.Fatalf("stderr does not name the file: %q", errb.String())
	}
	if b, _ := os.ReadFile(config.Path()); string(b) != string(broken) {
		t.Fatalf("the broken file was rewritten: %q", b)
	}
}
