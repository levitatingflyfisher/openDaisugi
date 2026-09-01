package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

// onATerminal makes c answer that its stdin is a terminal, so a test can
// drive attach's wire over a pipe. Every real run keeps the real check.
func onATerminal(c *CLI) *CLI {
	c.isTerminal = func() bool { return true }
	return c
}

// attach on a pipe cannot show a screen. It says so on stderr and exits 1
// before it ever dials, so a script that pipes into it learns the rule
// instead of seeing a server error or a hang.
func TestAttachWithoutATerminalExits1AndSaysWhy(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	c := &CLI{
		Version: "test", Socket: filepath.Join(dir, "server.sock"), DataDir: dir,
		In: strings.NewReader(""), Out: io.Discard, Err: &stderr,
	}
	code := c.Run([]string{"attach", "w1:p1"})
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	want := "attach needs a terminal. Run it from a shell, not a pipe."
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr does not teach: %q, want %q", stderr.String(), want)
	}
	assertNoAutostartArtifacts(t, dir)
}

// A [keys] leave the parser refuses stops attach with exit 1 and the file
// name, before any dial: a pane attached with a key nobody can press is a
// pane nobody can leave.
func TestAttachRefusesABadLeaveKeyBeforeDialing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte("[keys]\nleave = \"shift-tab\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	c := onATerminal(&CLI{
		Version: "test", Socket: filepath.Join(dir, "server.sock"), DataDir: dir,
		In: strings.NewReader(""), Out: io.Discard, Err: &stderr,
	})
	code := c.Run([]string{"attach", "w1:p1"})
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	for _, want := range []string{`bad [keys] leave "shift-tab" in `, config.Path(), "like ctrl-]"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q lacks %q", stderr.String(), want)
		}
	}
	assertNoAutostartArtifacts(t, dir)
}
