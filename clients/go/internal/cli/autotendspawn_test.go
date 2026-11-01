package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/install"
)

// The detached auto-tend starts the path this binary ran from, never a bare
// "daisugi", and the words after it are hook auto-tend --data-dir DIR.
func TestAutoTendArgs(t *testing.T) {
	got := strings.Join(autoTendArgs("/d"), " ")
	if got != "hook auto-tend --data-dir /d" {
		t.Fatalf("got %q", got)
	}
}

func TestAutoTendProgramIsAnAbsolutePath(t *testing.T) {
	self, err := install.Self()
	if err != nil || !filepath.IsAbs(self) {
		t.Fatalf("self %q: %v", self, err)
	}
	if _, err := os.Stat(self); err != nil {
		t.Fatal(err)
	}
}
