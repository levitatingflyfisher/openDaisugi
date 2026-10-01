package server

import (
	"fmt"
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/testhome"
)

// TestMain points HOME and every XDG directory at a scratch directory
// before any test runs (testhome.Isolate), so a test server never reads,
// writes or dials anything in the operator's home.
func TestMain(m *testing.M) {
	dir, err := testhome.Isolate("coppice-server-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// OPENDAISUGI_HOME names a canary: a test that fell back to a default
	// data home wrote there, and fails the run.
	code := testhome.CheckCanary(m.Run())
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
