package cli

import (
	"os"
	"path/filepath"
	"testing"

	"daisugi-verify/internal/testtmp"
)

// TestMain gives the package one private TMPDIR, removed after the last test.
// It also names the test binary daisugi: install writes the path the binary
// was run by only when that is one of our own names (install.SelfEntry), and
// the checks after a write need that path to exist and to run.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "daisugi-entry-")
	if err == nil {
		exe := filepath.Join(dir, "daisugi")
		if os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755) == nil {
			os.Args[0] = exe
		}
	}
	code := testtmp.Main(m)
	if err == nil {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
