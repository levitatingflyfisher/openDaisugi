package pando

import (
	"os"
	"testing"

	"daisugi-verify/internal/testtmp"
)

// TestMain gives the package a private TMPDIR. Most tests append hundreds
// of lines and do not test durability; the ones that do (realSync) turn
// fsync back on.
func TestMain(m *testing.M) {
	noSync = true
	os.Exit(testtmp.Main(m))
}
