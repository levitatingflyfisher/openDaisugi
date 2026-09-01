package plugins

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestTheNeovimRosterSpecPasses runs the Lua spec of the Neovim plugin in a
// headless nvim. Without nvim on PATH it skips, and the skip names the gap.
func TestTheNeovimRosterSpecPasses(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("nvim not on PATH. A listed gap: the Lua spec did not run.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nvim, "--headless", "--clean", "-c", "luafile tests/roster_spec.lua")
	cmd.Dir = filepath.Join("..", "..", "plugins", "nvim")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the Lua spec failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
