package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// createHeadlessSprigPane starts a headless pane on the fake sprig
// script, never a real agent. COPPICE_SPRIG_BIN is process-wide, so it
// reaches the in-process server liveServer starts. internal/adapters/all,
// blank-imported at the top of cli_test.go, is what registers sprig here.
func createHeadlessSprigPane(t *testing.T, sock string) string {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("..", "..", "testdata", "adapters", "fake-sprig.sh"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_SPRIG_BIN", bin)
	cl, err := dialExisting(sock, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	res, err := cl.Do("pane.create", map[string]any{
		"cwd": t.TempDir(), "kind": "headless", "harness": "sprig",
		"cmd_argv": []string{"--session-dir", t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create returned no pane: %v", res)
	}
	return id
}

// coppice attach on a headless pane never reaches attach.Run: that loop
// only ever forwards raw keystrokes, which such an agent either refuses
// outright or, for claude in stream-json mode, takes straight onto its
// stdin past the ask guard. It prints one line instead, before any of
// attach.Run's own terminal switches run.
func TestAttachOnAHeadlessPanePrintsOneLineInsteadOfAttaching(t *testing.T) {
	sock, dir := liveServer(t)
	id := createHeadlessSprigPane(t, sock)
	var out, stderr bytes.Buffer
	c := onATerminal(&CLI{
		Version: "test", Socket: sock, DataDir: dir,
		In: strings.NewReader(""), Out: &out, Err: &stderr,
	})
	code := c.Run([]string{"attach", id})
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "takes whole messages") {
		t.Fatalf("stderr does not say why: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "coppice agent prompt "+id) {
		t.Fatalf("stderr does not say how: %q", stderr.String())
	}
	if out.Len() != 0 {
		t.Fatalf("attach.Run's own alternate-screen switch ran: %q", out.String())
	}
}

// A pty pane is unaffected by the headless check: it still reaches
// attach.Run, which then fails to dial the fake socket path used here,
// proving the new lookup did not swallow that path for a pty pane.
func TestAttachOnAPtyPaneStillReachesAttachRun(t *testing.T) {
	sock, dir := liveServer(t)
	cl, err := dialExisting(sock, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cl.Do("pane.create", map[string]any{
		"cwd": t.TempDir(), "kind": "pty", "cmd_argv": []string{"sh", "-c", "sleep 30"},
	})
	_ = cl.Close()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create returned no pane: %v", res)
	}
	var out, stderr bytes.Buffer
	c := onATerminal(&CLI{
		Version: "test", Socket: sock, DataDir: dir,
		In: strings.NewReader(""), Out: &out, Err: &stderr,
	})
	code := c.Run([]string{"attach", id})
	// stdin is a plain strings.Reader, not a real pty: attach.Run dials
	// fine, sends pane.attach, then has no real terminal to run raw mode
	// on and returns once ReadKeys' own goroutine sees EOF. Either way it
	// must not print the headless refusal for a pty pane.
	if strings.Contains(stderr.String(), "takes whole messages") {
		t.Fatalf("a pty pane got the headless refusal: %q (exit %d)", stderr.String(), code)
	}
}
