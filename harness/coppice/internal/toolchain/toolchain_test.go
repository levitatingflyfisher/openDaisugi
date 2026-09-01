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
