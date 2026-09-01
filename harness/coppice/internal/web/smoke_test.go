package web

import (
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/opendaisugi/coppice/internal/testhome"
)

// browserToolsSkipReason is empty when Node and Playwright are both present,
// and otherwise the message a skipped test prints. It names the command
// that fixes the machine.
func browserToolsSkipReason() string {
	if _, err := exec.LookPath("node"); err != nil {
		return "node is not on PATH, so the Playwright pass did not run"
	}
	// The Playwright install is a tool of the machine, like the ghostty
	// prefix, so it is looked up in the home the process started with;
	// no test server sees that home.
	home := testhome.RealHome()
	if home == "" {
		return "no home directory, so the Playwright install path is unknown"
	}
	if _, err := os.Stat(filepath.Join(home, ".cache", "oh-visual-loop", "node_modules", "playwright")); err != nil {
		return "playwright is not installed. Run iss-skills/skills/visual-loop/scripts/web-setup.sh"
	}
	return ""
}

// requireBrowserToolsOrSkip mirrors toolchain.RequireOrSkip: a missing Node
// or Playwright skips t with a reason, and fails t instead when
// COPPICE_REQUIRE_TOOLCHAIN is set, so a broken CI machine cannot go green
// by skipping every browser test silently.
func requireBrowserToolsOrSkip(t *testing.T) {
	t.Helper()
	reason := browserToolsSkipReason()
	if reason == "" {
		return
	}
	if os.Getenv("COPPICE_REQUIRE_TOOLCHAIN") != "" {
		t.Fatalf("COPPICE_REQUIRE_TOOLCHAIN is set: %s", reason)
		return
	}
	t.Skip(reason)
}

// The Playwright pass drives a real browser against a real server. It costs
// seconds and needs Node and Chromium, so it runs on request and skips with
// its reason otherwise, which is the rule for every live test in this
// module.
func TestPhoneSmoke(t *testing.T) {
	if os.Getenv("COPPICE_SMOKE") != "1" {
		t.Skip("set COPPICE_SMOKE=1 to run the Playwright pass")
	}
	requireBrowserToolsOrSkip(t)
	cmd := exec.Command("uv", "run", "--no-sync", "python", "scripts/phone_smoke.py")
	cmd.Dir = "../../../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("phone_smoke.py failed:\n%s", out)
	}
}

// TestTheCheckedInScreenshotsExist is the eyeballing record. If a picture is
// gone, or the wrong size, nobody has actually looked at this client in a
// while. It reads five committed files and needs no browser, so it runs
// unconditionally: gating it on Playwright would make it skip on exactly the
// machines that never installed a browser to begin with, CI included.
func TestTheCheckedInScreenshotsExist(t *testing.T) {
	for _, name := range []string{"roster.png", "pane.png", "new.png", "settings.png", "https-roster.png"} {
		path := filepath.Join("..", "..", "testdata", "phone", name)
		f, err := os.Open(path)
		if err != nil {
			t.Errorf("%s is missing. Run uv run --no-sync python scripts/phone_smoke.py", name)
			continue
		}
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Errorf("%s will not decode as an image: %v", name, err)
			continue
		}
		if cfg.Width != 360 || cfg.Height != 780 {
			t.Errorf("%s is %dx%d, want 360x780", name, cfg.Width, cfg.Height)
		}
	}
}
