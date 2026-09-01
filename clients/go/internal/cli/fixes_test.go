package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A runtime whose apply fails is named on stderr and the run exits 1; it
// is never reported as already configured.
func TestInstallNamesAFailedRuntime(t *testing.T) {
	home := t.TempDir()
	registerDefaultEnvelope(t, home)
	code, out, errs := run(t, home, "", "install", "--gate", "--enforce", "--yes", "--runtime", "claude")
	if code != 1 || strings.Contains(out, "already configured") || !strings.Contains(errs, "Failed: Claude Code: ") {
		t.Fatalf("no ~/.claude: exit %d\n%s\n%s", code, out, errs)
	}
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(p, []byte(`{"hooks": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run(t, home, "", "install", "--gate", "--enforce", "--yes")
	if code != 1 || strings.Contains(out, "already configured") || !strings.Contains(errs, "Failed: Claude Code: ") {
		t.Fatalf("hooks list: exit %d\n%s\n%s", code, out, errs)
	}
	if raw, _ := os.ReadFile(p); string(raw) != `{"hooks": []}` {
		t.Fatalf("wrote %s", raw)
	}
}

// registerDefaultEnvelope writes an envelope, so an enforce install has a
// policy to install against.
func registerDefaultEnvelope(t *testing.T, home string) {
	t.Helper()
	d := filepath.Join(home, ".opendaisugi", "gate", "envelopes")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "default.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An enforce install with no envelope refuses, exits 1 and writes nothing:
// its hook would deny every call. Shadow installs as before.
func TestEnforceInstallNeedsAPolicy(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, home, "", "install", "--gate", "--enforce", "--yes")
	if code != 1 || errs != EnforceNeedsPolicy+"\n" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err == nil {
		t.Fatal("settings.json was written")
	}
	if code, _, errs := run(t, home, "", "install", "--gate", "--yes"); code != 0 {
		t.Fatalf("shadow: exit %d, %s", code, errs)
	}
	registerDefaultEnvelope(t, home)
	if code, _, errs := run(t, home, "", "install", "--gate", "--enforce", "--yes"); code != 0 {
		t.Fatalf("enforce with a policy: exit %d, %s", code, errs)
	}
}

// A gate hook in a form this CLI does not read: status never says shadow
// for it, and uninstall leaves it, names it and exits 1.
func TestUnknownGateHook(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude", "settings.json")
	body := `{"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "nice python3 -m opendaisugi.gate --mode enforce"}]}]}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, home, "", "gate", "status"); code != 0 || !strings.Contains(out, "mode: unknown gate hook (global)") {
		t.Fatalf("status: %d %s", code, out)
	}
	code, out, _ := run(t, home, "", "install", "--gate", "--uninstall")
	if code != 1 || !strings.Contains(out, "Failures (left untouched): Claude Code: "+p+" holds a gate hook") {
		t.Fatalf("uninstall: %d %s", code, out)
	}
	if raw, _ := os.ReadFile(p); string(raw) != body {
		t.Fatalf("changed: %s", raw)
	}
}

// gate init --session a/b checks the file it would write (a_b.json).
func TestInitChecksTheFileItWrites(t *testing.T) {
	home := t.TempDir()
	d := filepath.Join(home, ".opendaisugi", "gate", "envelopes")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "a_b.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, home, "", "gate", "init", "--session", "a/b", "--workspace", "/work")
	if code != 1 || !strings.Contains(errs, "a_b.json") {
		t.Fatalf("exit %d %s", code, errs)
	}
	if raw, _ := os.ReadFile(filepath.Join(d, "a_b.json")); string(raw) != "{}" {
		t.Fatalf("overwritten: %s", raw)
	}
}

// editOnRead changes a file the moment the prompt reads its answer.
type editOnRead struct {
	path, text string
	answer     io.Reader
	done       bool
}

func (r *editOnRead) Read(b []byte) (int, error) {
	if !r.done {
		r.done = true
		if err := os.WriteFile(r.path, []byte(r.text), 0o644); err != nil {
			return 0, err
		}
	}
	return r.answer.Read(b)
}

// The plan is made again after the prompt: a hook added while it waited
// is kept, not overwritten by the plan read before it.
func TestInstallPlansAgainAfterThePrompt(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude", "settings.json")
	late := `{"model": "late"}`
	var out, errb bytes.Buffer
	code := Main(&Env{
		Args:    []string{"install", "--gate"},
		Stdin:   &editOnRead{path: p, text: late, answer: strings.NewReader("y\n")},
		Stdout:  &out,
		Stderr:  &errb,
		Environ: []string{"HOME=" + home, "PATH=/nonexistent"},
	})
	raw, _ := os.ReadFile(p)
	if code != 0 || !strings.Contains(string(raw), `"model": "late"`) || !strings.Contains(string(raw), "gate check") {
		t.Fatalf("exit %d\n%s\n%s", code, raw, errb.String())
	}
}

// A settings.json that is a symlink is written through, as Python does,
// and the run says so.
func TestInstallSaysWhenItWritesThroughASymlink(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(home, "dotfiles-settings.json")
	if err := os.WriteFile(real, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".claude", "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, home, "", "install", "--gate", "--yes")
	if code != 0 || !strings.Contains(errs, "note: "+link+" is a symlink; wrote "+real) {
		t.Fatalf("exit %d %s", code, errs)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	if raw, _ := os.ReadFile(real); !strings.Contains(string(raw), "gate check") {
		t.Fatalf("%s", raw)
	}
}

// gate status warns on stderr when a hook names a binary that is gone.
func TestGateStatusWarnsWhenTheHookProgramIsGone(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(home, "mise", "installs", "d", "0.43.0", "daisugi")
	cmd := "DAISUGI_GATE_HOOK=opendaisugi.gate '" + gone + "' gate check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2"
	body := `{"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "` + cmd + `"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, home, "", "gate", "status", "--root", filepath.Join(home, "g"))
	want := "runs " + gone + ", which does not exist, so every call is denied. Run: daisugi install --gate --enforce"
	if code != 0 || !strings.Contains(errs, want) {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}
