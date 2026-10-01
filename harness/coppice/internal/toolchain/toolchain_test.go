package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGhosttyPrefixHonoursOverride(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", "/opt/gvt")
	if got := GhosttyPrefix(); got != "/opt/gvt" {
		t.Fatalf("GhosttyPrefix() = %q, want /opt/gvt", got)
	}
}

func writePC(t *testing.T, prefix string) {
	t.Helper()
	pc := filepath.Join(prefix, "share", "pkgconfig")
	if err := os.MkdirAll(pc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "libghostty-vt-static.pc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// With no override, the prefix is under $XDG_DATA_HOME/opendaisugi, or
// ~/.local/share/opendaisugi when XDG_DATA_HOME is unset. A build already
// at the old ~/.local/ghostty-vt keeps working.
func TestGhosttyPrefixDefaultsUnderXDGDataHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPPICE_GHOSTTY_PREFIX", "")
	t.Setenv("XDG_DATA_HOME", "")
	if got, want := GhosttyPrefix(), filepath.Join(home, ".local", "share", "opendaisugi", "ghostty-vt"); got != want {
		t.Fatalf("XDG unset: GhosttyPrefix() = %q, want %q", got, want)
	}
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_DATA_HOME", xdg)
	if got, want := GhosttyPrefix(), filepath.Join(xdg, "opendaisugi", "ghostty-vt"); got != want {
		t.Fatalf("XDG set: GhosttyPrefix() = %q, want %q", got, want)
	}
	legacy := filepath.Join(home, ".local", "ghostty-vt")
	writePC(t, legacy)
	if got := GhosttyPrefix(); got != legacy {
		t.Fatalf("old build present: GhosttyPrefix() = %q, want %q", got, legacy)
	}
}

// toolchain.sh changes no global Go setting and writes nothing onto PATH:
// zig and cmake go under the XDG data directory, pinned by version and
// sha256, and are used even when another zig or cmake is on PATH.
func TestToolchainScriptHasNoGlobalSideEffects(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/toolchain.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, bad := range []string{"go env -w", "coppice-pkg-config", "uv tool install", "ln -s", ".local/bin", "XDG_BIN_HOME", "command -v zig", "command -v cmake", "$HOME/.local/zig-"} {
		if strings.Contains(s, bad) {
			t.Errorf("toolchain.sh holds %q", bad)
		}
	}
	for _, good := range []string{
		`${XDG_DATA_HOME:-$HOME/.local/share}/opendaisugi`,
		`CMAKE_VERSION="4.4.3"`,
		"bae3c4954623ec4d62e62c70443f0da7988b733111c2871fcc6a31ead5137e20",
		"520ff2ba3afb7a1e34a0ab222c0f4e89ad334dc7462814886e099bdf6ba8cbb3",
		"6c95b37116bb5c714656e4f76931ebdcb739209a1aee91cf51408ccfe137694e",
		`export PATH="$zig_dir:$cmake_bin:$PATH"`,
		`echo "  export PATH=`,
	} {
		if !strings.Contains(s, good) {
			t.Errorf("toolchain.sh lacks %q", good)
		}
	}
}

func TestHaveGhosttyVTIsFalseWithoutThePkgconfigFile(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", t.TempDir())
	if HaveGhosttyVT() {
		t.Fatal("HaveGhosttyVT() = true for an empty prefix, want false")
	}
}

func TestHaveGhosttyVTIsTrueWhenThePkgconfigFileExists(t *testing.T) {
	dir := t.TempDir()
	pc := filepath.Join(dir, "share", "pkgconfig")
	if err := os.MkdirAll(pc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "libghostty-vt-static.pc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_GHOSTTY_PREFIX", dir)
	if !HaveGhosttyVT() {
		t.Fatal("HaveGhosttyVT() = false with the pkgconfig file present, want true")
	}
}

// The skip reason is a user-facing string. It must teach the next command,
// per the global copy rule.
func TestSkipReasonNamesTheInstallerCommand(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", t.TempDir())
	got := SkipReason()
	if !strings.Contains(got, "scripts/toolchain.sh") {
		t.Fatalf("SkipReason() = %q, want it to name scripts/toolchain.sh", got)
	}
	if strings.Contains(got, "—") {
		t.Fatalf("SkipReason() = %q, want no em-dash", got)
	}
}

func TestSkipReasonIsEmptyWhenTheLibraryIsPresent(t *testing.T) {
	dir := t.TempDir()
	pc := filepath.Join(dir, "share", "pkgconfig")
	if err := os.MkdirAll(pc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "libghostty-vt-static.pc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_GHOSTTY_PREFIX", dir)
	if got := SkipReason(); got != "" {
		t.Fatalf("SkipReason() = %q, want empty", got)
	}
}

// RequireOrSkip is the one helper CI's silent-green guard depends on: when
// COPPICE_REQUIRE_TOOLCHAIN is unset, a missing toolchain skips a test
// exactly as SkipReason always has. When it is set, the same missing
// toolchain fails the test instead - CI sets it so a wrong
// COPPICE_GHOSTTY_PREFIX cannot turn a whole suite green by skipping every
// cgo test silently (a wrong prefix has been measured to skip 202 of 484
// tests while the suite still reports ok).
//
// The skip case runs inside a real subtest: t.Run reports true for a
// subtest that skipped cleanly, and a skip does not taint the parent's own
// result, so this is safe to observe directly. The fail case cannot use the
// same trick - a failing subtest taints its parent regardless of what the
// parent does afterward, so t.Run's own return value cannot be checked
// without this test itself failing. skipFatalTracker, below, is what
// stands in for t there instead: it records that Fatalf was called rather
// than actually stopping the goroutine the way a real *testing.T's Fatal
// would.
func TestRequireOrSkipSkipsWhenNotRequired(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", t.TempDir())
	t.Setenv("COPPICE_REQUIRE_TOOLCHAIN", "")
	ranPast := false
	ok := t.Run("inner", func(t *testing.T) {
		RequireOrSkip(t)
		ranPast = true
	})
	if !ok {
		t.Fatal("the inner subtest failed, want a clean skip")
	}
	if ranPast {
		t.Fatal("RequireOrSkip did not actually skip when the toolchain is missing")
	}
}

func TestRequireOrSkipFailsWhenRequired(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", t.TempDir())
	t.Setenv("COPPICE_REQUIRE_TOOLCHAIN", "1")
	ft := &skipFatalTracker{}
	RequireOrSkip(ft)
	if !ft.failed {
		t.Fatal("RequireOrSkip did not fail: COPPICE_REQUIRE_TOOLCHAIN was set " +
			"and the toolchain is missing")
	}
	if ft.skipped {
		t.Fatal("RequireOrSkip skipped instead of failing when COPPICE_REQUIRE_TOOLCHAIN was set")
	}
}

// skipFatalTracker is a minimal skipFataler that records what RequireOrSkip
// called instead of acting on it, so a test can assert on the outcome
// without a real t.Fatal's runtime.Goexit ending the goroutine.
type skipFatalTracker struct {
	failed  bool
	skipped bool
}

func (f *skipFatalTracker) Helper()                           {}
func (f *skipFatalTracker) Skip(args ...any)                  { f.skipped = true }
func (f *skipFatalTracker) Fatalf(format string, args ...any) { f.failed = true }

func TestRequireOrSkipDoesNothingWhenTheLibraryIsPresent(t *testing.T) {
	dir := t.TempDir()
	pc := filepath.Join(dir, "share", "pkgconfig")
	if err := os.MkdirAll(pc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "libghostty-vt-static.pc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_GHOSTTY_PREFIX", dir)
	t.Setenv("COPPICE_REQUIRE_TOOLCHAIN", "1") // must not matter: nothing to require here
	ranPast := false
	ok := t.Run("inner", func(t *testing.T) {
		RequireOrSkip(t)
		ranPast = true
	})
	if !ok || !ranPast {
		t.Fatal("RequireOrSkip stopped a test the library is actually present for")
	}
}
