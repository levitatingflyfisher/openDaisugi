package foreman

import (
	"fmt"
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/testhome"
)

// TestMain points HOME and every XDG directory at a scratch directory
// before any test runs (testhome.Isolate), so no test reads or writes the
// operator's real foreman log.
func TestMain(m *testing.M) {
	dir, err := testhome.Isolate("coppice-foreman-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := testhome.CheckCanary(m.Run())
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
