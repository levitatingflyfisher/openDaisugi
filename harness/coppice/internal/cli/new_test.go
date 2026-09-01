package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

func TestNewWithNoProjectUsesTheCallersOwnDirectory(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	here := t.TempDir()
	t.Chdir(here)
	code, out, errb := openCLI(t, sock, dir, "new")
	assertOpened(t, code, out, errb)
	rows := paneRows(t, sock, dir)
	if len(rows) != 1 || rows[0]["cwd"] != here || rows[0]["harness"] != "fakeh" {
		t.Fatalf("panes = %v, want one in %q running fakeh", rows, here)
	}
}

func TestNewWithAProjectNameMatchesByBaseName(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	project := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{project},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := openCLI(t, sock, dir, "new", filepath.Base(project))
	assertOpened(t, code, out, errb)
	rows := paneRows(t, sock, dir)
	if len(rows) != 1 || rows[0]["cwd"] != project {
		t.Fatalf("panes = %v, want one in %q", rows, project)
	}
}

func TestNewWithAPathMatchesByPathWhenNoNameMatches(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	project := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{project},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := openCLI(t, sock, dir, "new", project)
	assertOpened(t, code, out, errb)
	rows := paneRows(t, sock, dir)
	if len(rows) != 1 || rows[0]["cwd"] != project {
		t.Fatalf("panes = %v, want one in %q", rows, project)
	}
}

// Two pinned projects can share a base name (different parents, same leaf
// directory name). coppice new must never silently pick one - it refuses
// and names both paths, so the operator can pass the one they meant.
func TestNewWithAnAmbiguousProjectNameRefusesAndListsThePaths(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	parentA, parentB := t.TempDir(), t.TempDir()
	projectA := filepath.Join(parentA, "trellis")
	projectB := filepath.Join(parentB, "trellis")
	if err := os.MkdirAll(projectA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{projectA, projectB},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	code, _, errb := openCLI(t, sock, dir, "new", "trellis")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errb)
	}
	for _, want := range []string{projectA, projectB, "Pass the path"} {
		if !strings.Contains(errb, want) {
			t.Fatalf("stderr = %q, want it to contain %q", errb, want)
		}
	}
	if rows := paneRows(t, sock, dir); len(rows) != 0 {
		t.Fatalf("a pane was opened for an ambiguous name: %v", rows)
	}
}

func TestNewWithAnUnknownProjectNamesTheFix(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, _, errb := openCLI(t, sock, dir, "new", "no-such-project")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errb)
	}
	if !strings.Contains(errb, "project list") {
		t.Fatalf("stderr = %q, want it to point at project list", errb)
	}
}

func TestNewWithNoAttachPrintsThePaneIDWithNoTerminalMention(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, out, errb := openCLI(t, sock, dir, "new", "--no-attach")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	if !strings.HasPrefix(out, "opened ") || strings.Contains(out, "terminal") {
		t.Fatalf("stdout = %q", out)
	}
}

// With no config file at all, coppice new discovers a harness on PATH the
// same way a bare coppice HARNESS invocation would with nothing configured
// yet.
func TestNewWithNoConfigFileDiscoversAHarnessOnPATH(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeHarnessOnPath(t, "claude")
	code, out, errb := openCLI(t, sock, dir, "new", "--no-attach")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	rows := paneRows(t, sock, dir)
	if len(rows) != 1 || rows[0]["harness"] != "claude" {
		t.Fatalf("panes = %v, want one running the discovered claude", rows)
	}
}

// A config file that exists but names no default harness is a different
// case from no config at all: guessing from PATH here would silently pick
// a harness the operator never chose. coppice new refuses instead, with
// the exact fix the server itself gives for the same gap.
func TestNewWithAConfigFileButNoDefaultLineRefuses(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{
		Harness: map[string]config.Harness{"claude": {Command: "claude"}},
	}); err != nil {
		t.Fatal(err)
	}
	fakeHarnessOnPath(t, "claude")
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "new")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errb)
	}
	want := "no default harness in " + config.Path() + ". Run coppice open HARNESS once to set one."
	if !strings.Contains(errb, want) {
		t.Fatalf("stderr = %q, want it to contain %q", errb, want)
	}
}

func TestNewWithRemoteIsRefused(t *testing.T) {
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "--remote", "ssh://host", "new")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "--remote") {
		t.Fatalf("stderr = %q, want it to name --remote", errb)
	}
}
