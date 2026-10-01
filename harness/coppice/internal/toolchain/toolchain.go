// Package toolchain answers one question for the rest of coppice: are the
// build-time pieces of libghostty-vt present on this machine? cgo tests ask it
// so they skip with a reason instead of failing with a linker error.
package toolchain

import (
	"os"
	"path/filepath"
)

// GhosttyPrefix is where scripts/toolchain.sh installs libghostty-vt:
// COPPICE_GHOSTTY_PREFIX when set; else ~/.local/ghostty-vt when a build is
// already there (an older toolchain.sh put it there); else
// $XDG_DATA_HOME/opendaisugi/ghostty-vt, with ~/.local/share for an unset
// XDG_DATA_HOME. toolchain.sh and preflight.sh apply the same rule.
func GhosttyPrefix() string {
	if p := os.Getenv("COPPICE_GHOSTTY_PREFIX"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	legacy := filepath.Join(home, ".local", "ghostty-vt")
	if hasPC(legacy) {
		return legacy
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "opendaisugi", "ghostty-vt")
}

func hasPC(prefix string) bool {
	_, err := os.Stat(filepath.Join(prefix, "share", "pkgconfig", "libghostty-vt-static.pc"))
	return err == nil
}

// HaveGhosttyVT reports whether the static pkg-config file exists. That file is
// what the cgo directive in go-libghostty resolves, so its presence is the
// honest test of "can this package link".
func HaveGhosttyVT() bool {
	return hasPC(GhosttyPrefix())
}

// SkipReason is empty when the build deps are present. Otherwise it is the
// message a skipped test prints. It names the command that fixes the machine.
func SkipReason() string {
	if HaveGhosttyVT() {
		return ""
	}
	return "libghostty-vt is not built. Run harness/coppice/scripts/toolchain.sh to install it."
}

// skipFataler is the subset of testing.TB RequireOrSkip needs. A real
// *testing.T or *testing.B satisfies it already: Go's interfaces are
// structural, so any type with matching methods qualifies, without this
// package needing to import "testing" or name testing.TB at all.
// testing.TB itself could not stand in for this: it carries an unexported
// method precisely to refuse any type declared outside package testing,
// which would make toolchain_test.go unable to build its own fake for
// TestRequireOrSkipFailsWhenRequired - t.Run's own subtest failure would
// otherwise taint this whole test's result, with no way to observe Fatalf
// having fired without also failing the test proving it does.
type skipFataler interface {
	Helper()
	Skip(args ...any)
	Fatalf(format string, args ...any)
}

// RequireOrSkip is the one place every cgo-tainted test's own skip helper
// calls through to. When the toolchain is missing it skips t, the same as
// every caller already did by hand. When COPPICE_REQUIRE_TOOLCHAIN is also
// set, it fails t instead: CI sets that variable so a wrong
// COPPICE_GHOSTTY_PREFIX cannot turn a whole suite green by skipping every
// cgo test silently, the way SkipReason's own quiet skip otherwise would.
func RequireOrSkip(t skipFataler) {
	t.Helper()
	r := SkipReason()
	if r == "" {
		return
	}
	if os.Getenv("COPPICE_REQUIRE_TOOLCHAIN") != "" {
		t.Fatalf("COPPICE_REQUIRE_TOOLCHAIN is set: %s", r)
		return
	}
	t.Skip(r)
}
