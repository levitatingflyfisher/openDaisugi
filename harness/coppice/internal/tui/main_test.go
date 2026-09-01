package tui

import (
	"fmt"
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/testhome"
)

// TestMain points the config and state dirs at an empty one before any
// test runs. A
// sentence typed at the prompt goes to floor.talk, and the test server
// reads the config file of this process: with the owner's real file, a
// test could start the owner's real harness as the foreman. A test that
// needs a config sets its own dir with t.Setenv.
func TestMain(m *testing.M) {
	// HOME and every XDG directory point at a scratch one: a foreman the
	// test server starts works under XDG_STATE_HOME, never in the owner's
	// home, and nothing reads the owner's config or gate hooks.
	dir, err := testhome.Isolate("coppice-tui-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
