package steward

import (
	"daisugi-verify/pando"
	"fmt"
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/testhome"
)

// TestMain points HOME and every XDG directory at a scratch directory
// before any test runs (testhome.Isolate), so no test reads or writes the
// operator's real steward log.
func TestMain(m *testing.M) {
	dir, err := testhome.Isolate("coppice-steward-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Most tests append hundreds of lines and do not test durability; the
	// ones that do (realSync) turn fsync back on.
	pando.SetNoSync(true)
	code := testhome.CheckCanary(m.Run())
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
