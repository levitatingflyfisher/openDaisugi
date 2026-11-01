package cli

import (
	"os"
	"path/filepath"
	"testing"

	"daisugi-verify/pando"
)

// The owner's broken_keep reaches the Pando store too, not only coppice's
// own copies.
func TestBrokenKeepReachesThePandoStore(t *testing.T) {
	defer pando.SetBrokenKeep(pando.BrokenKeep())
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "coppice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "coppice", "coppice.toml"), []byte("broken_keep = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pando.SetBrokenKeep(5)
	applyStateSettings()
	if got := pando.BrokenKeep(); got != 0 {
		t.Fatalf("pando keeps %d, want 0 (keep all)", got)
	}
}
