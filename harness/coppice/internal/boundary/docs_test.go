// These three checks live here, not in internal/cli:
// internal/cli is cgo-tainted (it imports internal/server,
// which imports internal/pane, which imports internal/vt), so a test there
// cannot even link on a machine that never ran scripts/toolchain.sh - exactly
// the machine a docs-only contributor is most likely to be on. internal/boundary
// already exists as the repo's cgo-free home for a check of this shape (see
// PINS.md). internal/cli/cli_test.go keeps the one test that needs it: the
// usage text lives there as an unexported const.
package boundary_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readme(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// This list is hand-kept, not derived from
// internal/cli's own verbSpecs - that package is cgo-tainted (see this
// file's own package comment) and cannot be imported from here. Adding a
// verb to verbSpecs does not make this test notice; adding it to the
// README's own Command reference table does. Eleven of the twenty-two CLI
// verbs used to be missing from the README entirely, which is why this
// test exists. Whoever adds a verb must add it to both places by hand - this
// list and the table - or this test stops meaning what its name says.
func TestREADMEDocumentsEveryTopLevelCommand(t *testing.T) {
	text := readme(t)
	for _, cmd := range []string{
		"coppice server start", "coppice server stop", "coppice server status",
		"coppice server token", "coppice workspace create", "coppice workspace list",
		"coppice tab create", "coppice tab list", "coppice pane create",
		"coppice pane list", "coppice pane send-text", "coppice pane send-keys",
		"coppice pane run", "coppice pane read", "coppice pane close",
		"coppice pane resize", "coppice pane wait-output", "coppice pane explain",
		"coppice pane fork", "coppice pane forget", "coppice pane resume",
		"coppice floor note", "coppice skill foreman",
		"coppice agent allow", "coppice agent deny",
		"coppice agent list", "coppice agent get", "coppice agent prompt",
		"coppice agent wait", "coppice agent read", "coppice attach",
		"coppice web cert init", "coppice web cert show", "coppice web cert tailscale",
		"coppice web serve", "coppice web token",
		"--remote", "--stdio", "--foreground",
	} {
		if !strings.Contains(text, cmd) {
			t.Fatalf("README does not document %q", cmd)
		}
	}
}

// The README must say what a restart does not restore, in the same words the
// server does. Two different answers to that question is worse than one.
//
// This asserts the EXACT sentence, not just its lowercase tokens: server.RestartNote
// is the one source of truth, but this package cannot import internal/server
// to compare against it directly without pulling cgo into a package that
// exists specifically to stay free of it (see this file's own package
// comment). internal/server's own TestRestartNoteExactWording pins the same
// literal on that side, so a future edit to the sentence in either place
// that does not also update the other fails a test, not silently drifts.
func TestREADMERepeatsTheRestartHonesty(t *testing.T) {
	text := readme(t)
	const restartNote = "A restart brings back the layout, the labels and the working directories. " +
		"It does not bring back running processes. Panes come back closed or unknown, never idle. " +
		"A headless pane resumes only when its adapter recorded a harness session id."
	if !strings.Contains(text, restartNote) {
		t.Fatalf("README does not contain server.RestartNote's exact sentence: %q", restartNote)
	}
}

// STE100: no em-dashes in any coppice markdown file, not only the README.
// A ban enforced against one file is a cleanup, not a ban: the next doc a
// contributor writes could reintroduce them and nothing would catch it. This
// walks the same tree TestOnlyInternalVTImportsLibghostty walks, for the
// same reason: "harness/coppice" is exactly two levels up from this package.
func TestNoMarkdownFileHasAnEmDash(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "—") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these markdown files contain an em-dash, which the house register does not use: %v", offenders)
	}
}
