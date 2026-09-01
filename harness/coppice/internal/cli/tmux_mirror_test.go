package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// noDir is a path no test makes. The refusals here come before any dial or
// any file, so the tests need no directory.
const noDir = "/nonexistent/coppice-tmux-mirror-test"

func TestTmuxMirrorIsAVerbAndNotAHarnessName(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	code, _, errText := runCLI(t, filepath.Join(noDir, "s.sock"), noDir, "tmux-mirror")
	if code != 1 || !strings.Contains(errText, "run coppice tmux-mirror inside tmux, or pass --session NAME") {
		t.Fatalf("code %d err %q", code, errText)
	}
}

func TestTmuxMirrorRefusesRemoteAndUnknownFlags(t *testing.T) {
	dir := noDir
	code, _, errText := runCLI(t, filepath.Join(dir, "s.sock"), dir, "--remote", "ssh://h", "tmux-mirror")
	if code != 1 || !strings.Contains(errText, "needs a local socket") {
		t.Fatalf("code %d err %q", code, errText)
	}
	code, _, errText = runCLI(t, filepath.Join(dir, "s.sock"), dir, "tmux-mirror", "--bogus")
	if code != 1 || !strings.Contains(errText, `unknown flag "--bogus"`) {
		t.Fatalf("code %d err %q", code, errText)
	}
	code, _, errText = runCLI(t, filepath.Join(dir, "s.sock"), dir, "tmux-mirror", "--session")
	if code != 1 || !strings.Contains(errText, "--session needs a name") {
		t.Fatalf("code %d err %q", code, errText)
	}
}

func TestUsageNamesTmuxMirror(t *testing.T) {
	if !strings.Contains(usage, "coppice tmux-mirror") {
		t.Fatal("usage does not name tmux-mirror")
	}
}
