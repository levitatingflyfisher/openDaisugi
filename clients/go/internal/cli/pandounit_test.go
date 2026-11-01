package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runWithUnitHook(t *testing.T, home string, hook func(string, string) error, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(&Env{Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Environ: []string{"HOME=" + home, "PATH=/nonexistent"}, unitHookFn: hook})
	return code, out.String(), errb.String()
}

func TestInstallStartsThePandoServiceOnceAndUninstallStopsIt(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.MkdirAll(filepath.Join(home, ".hermes"), 0o700)
	var calls []string
	hook := func(a, u string) error { calls = append(calls, a); return nil }
	// The dry run says what it would do and calls nothing.
	code, out, errs := runWithUnitHook(t, home, hook, "install", "--dry-run", "--runtime", "claude", "--runtime", "hermes", "--only", "pando")
	if code != 0 || !strings.Contains(out, "enable and start it") || len(calls) != 0 {
		t.Fatalf("dry run: %d %v %q %q", code, calls, out, errs)
	}
	// Claude Code and Hermes share one unit: the dry run lists it once.
	if n := strings.Count(out, "follow user service"); n != 1 {
		t.Fatalf("the unit is listed %d times:\n%s", n, out)
	}
	if code, out, errs = runWithUnitHook(t, home, hook, "install", "--yes", "--runtime", "claude", "--runtime", "hermes", "--only", "pando"); code != 0 {
		t.Fatalf("install: %d %q %q", code, out, errs)
	}
	if strings.Join(calls, ",") != "enable" {
		t.Fatalf("install must start the service once: %v", calls)
	}
	calls = nil
	if code, out, errs = runWithUnitHook(t, home, hook, "install", "--uninstall", "--runtime", "claude", "--runtime", "hermes", "--only", "pando"); code != 0 {
		t.Fatalf("uninstall: %d %q %q", code, out, errs)
	}
	if strings.Join(calls, ",") != "disable,reload" {
		t.Fatalf("uninstall must stop and disable the service: %v", calls)
	}
}

func TestAScratchHomeNeverRunsSystemctl(t *testing.T) {
	e := &Env{home: t.TempDir(), env: map[string]string{}}
	if e.unitHook() != nil {
		t.Fatal("a home that is not the user's own must never reach systemctl")
	}
}

// When the user database cannot name the user, user.Current() falls back to
// $HOME. A scratch HOME would then pass for the real one. It must not.
func TestAFailedPasswdLookupNeverRunsSystemctl(t *testing.T) {
	home := t.TempDir()
	old := passwdHome
	defer func() { passwdHome = old }()
	passwdHome = func() (string, error) { return home, os.ErrNotExist }
	e := &Env{home: home, env: map[string]string{"PATH": "/usr/bin:/bin"}}
	if e.unitHook() != nil {
		t.Fatal("a failed passwd lookup must not reach systemctl")
	}
	passwdHome = func() (string, error) { return home, nil }
	// The same home from a good lookup is the user's own (systemctl on PATH).
	if _, err := os.Stat("/usr/bin/systemctl"); err == nil {
		if e.unitHook() == nil {
			t.Fatal("the user's own home should get the hook")
		}
	}
}
