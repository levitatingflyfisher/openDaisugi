package server

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// realDaisugi is the daisugi this repository builds (clients/go/daisugi),
// or COPPICE_TEST_DAISUGI. The test skips when neither exists.
func realDaisugi(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("COPPICE_TEST_DAISUGI"); p != "" {
		return p
	}
	_, file, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "clients", "go", "daisugi")
	if _, err := os.Stat(p); err != nil {
		t.Skip("no built daisugi at " + p + ". Run clients/go/scripts/build.sh, or set COPPICE_TEST_DAISUGI.")
	}
	return p
}

// hookCommand is the warden hook line a claude pane gets in its --settings
// document, for a pane pinned to session ("" for an unpinned pane).
func hookCommand(t *testing.T, s *Server, bin, session string) string {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(s.claudeSettings(bin, session)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Hooks["PreToolUse"][0].Hooks[0].Command
}

// runHook runs a hook line with one Claude PreToolUse payload on stdin and
// returns its exit code and output.
func runHook(t *testing.T, cmd, home, sessionInPayload, shell string) (int, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"session_id": sessionInPayload, "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": shell}, "cwd": "/tmp",
	})
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "HOME="+home)
	c.Stdin = bytes.NewReader(payload)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

// scratchWarden makes a scratch warden root in enforce mode. allow maps a
// session ("" is the default envelope) to the one shell head it allows.
func scratchWarden(t *testing.T, bin string, allow map[string]string) (root, home string) {
	t.Helper()
	tmp := t.TempDir()
	home = filepath.Join(tmp, "home")
	root = filepath.Join(tmp, "warden")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// The mode a hook with no --mode flag reads sits beside the warden root.
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte("warden_mode: enforce\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for session, head := range allow {
		name := session
		if name == "" {
			name = "default"
		}
		// The hook payload's cwd is /tmp, and an operand-less ls reads it.
		env := `{"id":"env_` + name + `","generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["` + head + `"],"file_read":["/tmp","/tmp/**"]}}`
		file := filepath.Join(tmp, name+".json")
		if err := os.WriteFile(file, []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"warden", "register", file, "--root", root}
		if session != "" {
			args = append(args, "--session", session)
		}
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "HOME="+home)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("daisugi %v: %v\n%s", args, err, out)
		}
	}
	return root, home
}

// AT-1: a pane that names another pane's session in its hook payload is
// still decided by its own hook line. The payload never selects an envelope
// (SEC-3), so pane B gets the default envelope, or a denial under a root
// with none, and never pane A's. design-delegation-tree.md asked for this
// before any envelope goes on a task.
func TestAPaneCannotNameAnotherPanesSession(t *testing.T) {
	bin := realDaisugi(t)
	s := newTestServer(t)
	// The default envelope allows ls. Session sA allows git.
	root, home := scratchWarden(t, bin, map[string]string{"": "ls", "sA": "git"})
	s.cfg.WardenRoot = root
	hookB := hookCommand(t, s, bin, "")
	hookA := hookCommand(t, s, bin, "sA")
	if strings.Contains(hookB, "--session") {
		t.Fatalf("pane B's hook is pinned: %s", hookB)
	}
	// B names sA and runs git, which sA allows and the default does not.
	if code, out := runHook(t, hookB, home, "sA", "git status"); code != 2 {
		t.Fatalf("B decided under sA's envelope: exit %d %s", code, out)
	}
	// B names sA and runs ls, which the default allows and sA does not: it
	// is decided under the default.
	if code, out := runHook(t, hookB, home, "sA", "ls"); code != 0 {
		t.Fatalf("B was not decided under the default envelope: exit %d %s", code, out)
	}
	// A is pinned by its own hook line: git passes, ls is denied, and the
	// payload names a session that does not exist.
	if code, out := runHook(t, hookA, home, "nobody", "git status"); code != 0 {
		t.Fatalf("A lost its envelope: exit %d %s", code, out)
	}
	if code, out := runHook(t, hookA, home, "default", "ls"); code != 2 {
		t.Fatalf("A read the default envelope: exit %d %s", code, out)
	}
	// A root with no default envelope: B is denied whatever the payload says.
	pinnedRoot, home2 := scratchWarden(t, bin, map[string]string{"sA": "git"})
	s.cfg.WardenRoot = pinnedRoot
	hookB2 := hookCommand(t, s, bin, "")
	if code, out := runHook(t, hookB2, home2, "sA", "git status"); code != 2 || !strings.Contains(out, "no envelope registered") {
		t.Fatalf("B under a pinned root: exit %d %s", code, out)
	}
}

// A pane pinned to a task's session never reads the default envelope; the
// same call from an unpinned pane does (W3K-12).
func TestAPinnedPaneNeverReadsDefault(t *testing.T) {
	bin := realDaisugi(t)
	s := newTestServer(t)
	root, home := scratchWarden(t, bin, map[string]string{"": "ls", "t1-aa": "git"})
	s.cfg.WardenRoot = root
	pinned := hookCommand(t, s, bin, "t1-aa")
	if !strings.Contains(pinned, "--session 't1-aa'") {
		t.Fatalf("the hook is not pinned: %s", pinned)
	}
	if code, out := runHook(t, pinned, home, "x", "ls"); code != 2 {
		t.Fatalf("the pinned pane read the default envelope: exit %d %s", code, out)
	}
	if code, out := runHook(t, hookCommand(t, s, bin, ""), home, "x", "ls"); code != 0 {
		t.Fatalf("the unpinned pane differs from the default: exit %d %s", code, out)
	}
}
