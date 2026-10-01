package gate

import (
	"os"
	"path/filepath"
	"testing"
)

// GR-R-1: the gate reads DISARMED and the envelope through no link, and
// only from a regular file this user owns. A refused DISARMED is armed; a
// refused envelope is none. The compare cases `state ...` pin the same
// rules against Python; each test here needs the read to work in the plain
// case, so it fails when the check or the read is taken out.

const stateEnvelope = `{"generated_by": "t", "task": "t", "permissions": {"file_read": ["/work/**"]}}`

func stateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "gate")
	if err := os.MkdirAll(filepath.Join(root, "envelopes"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func stateFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestDisarmedIsHonoredOnlyAsAPlainOwnedFile(t *testing.T) {
	root := stateRoot(t)
	r := &runner{root: root}
	if r.isDisarmed() {
		t.Fatal("no file, yet disarmed")
	}
	p := filepath.Join(root, "DISARMED")
	stateFile(t, p, "x", 0o600)
	if !r.isDisarmed() {
		t.Fatal("a plain DISARMED did not disarm")
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if r.isDisarmed() {
		t.Fatal("a world-writable DISARMED disarmed")
	}
	os.Remove(p)
	other := filepath.Join(t.TempDir(), "elsewhere")
	stateFile(t, other, "x", 0o600)
	if err := os.Symlink(other, p); err != nil {
		t.Fatal(err)
	}
	if r.isDisarmed() {
		t.Fatal("a symlinked DISARMED disarmed")
	}
	os.Remove(p)
	stateFile(t, p, "x", 0o600)
	if err := os.Link(p, filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatal(err)
	}
	if r.isDisarmed() {
		t.Fatal("a hard-linked DISARMED disarmed")
	}
}

func TestAnEnvelopeIsLoadedOnlyAsAPlainOwnedFileBelowARealRoot(t *testing.T) {
	root := stateRoot(t)
	r := &runner{root: root}
	if r.loadEnvelope(nil) != nil {
		t.Fatal("no file, yet an envelope")
	}
	p := filepath.Join(root, "envelopes", "default.json")
	stateFile(t, p, stateEnvelope, 0o600)
	if r.loadEnvelope(nil) == nil {
		t.Fatal("a plain envelope was not loaded")
	}
	if err := os.Chmod(p, 0o646); err != nil {
		t.Fatal(err)
	}
	if r.loadEnvelope(nil) != nil {
		t.Fatal("a world-writable envelope was loaded")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "gate-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if (&runner{root: link}).loadEnvelope(nil) != nil {
		t.Fatal("an envelope below a symlinked root was loaded")
	}
	// A root reached through a symlinked parent is read.
	parent := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(filepath.Dir(root), parent); err != nil {
		t.Fatal(err)
	}
	if (&runner{root: filepath.Join(parent, "gate")}).loadEnvelope(nil) == nil {
		t.Fatal("a root behind a symlinked parent was refused")
	}
}
