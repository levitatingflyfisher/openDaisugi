// Package testtmp keeps a test package from leaving temp dirs behind. A
// package calls Main from its TestMain: TMPDIR then points at one new
// directory for the whole run, and the directory is removed after the last
// test, even when a test failed. A test panic and a -timeout panic end the
// binary before the code after m.Run runs, so they skip the removal, and so
// does a kill.
//
// Code under test makes temp dirs of its own (the neutral working directory
// of a claude run, the data dir of an agentic step), and a test that does
// not set a temp dir for it leaves the dir in /tmp, which is RAM on this
// box. This makes that harmless. Set COPPICE_TEST_KEEP_TMPDIR=1 to leave
// TMPDIR alone, so a script can see what the tests leave.
package testtmp

import (
	"os"
	"testing"
)

// Main runs the tests of the package with a private TMPDIR and returns the
// exit code. Use it as:
//
//	func TestMain(m *testing.M) { os.Exit(testtmp.Main(m)) }
func Main(m *testing.M) int {
	if os.Getenv("COPPICE_TEST_KEEP_TMPDIR") != "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "dtt-")
	if err != nil {
		return m.Run()
	}
	old, had := os.LookupEnv("TMPDIR")
	_ = os.Setenv("TMPDIR", dir)
	code := m.Run()
	if had {
		_ = os.Setenv("TMPDIR", old)
	} else {
		_ = os.Unsetenv("TMPDIR")
	}
	_ = os.RemoveAll(dir)
	return code
}
