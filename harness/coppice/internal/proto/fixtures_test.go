package proto

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Every fixture must parse and validate. If one does not, the Go and Python
// readers have already drifted.
func TestEveryEventFixtureParses(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "events")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 4 {
		t.Fatalf("testdata/events has %d fixtures, want at least four", len(entries))
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseStateEvent(b); err != nil {
			t.Fatalf("%s does not parse: %v", e.Name(), err)
		}
	}
}

// Spec-02 requires the Go fixtures to agree with the Python ones. Spec-01
// owns those bytes, so this test skips with a reason until they land and
// fails loudly the moment the two sides diverge.
func TestEventFixturesMatchTheFloorFixtures(t *testing.T) {
	goDir := filepath.Join("..", "..", "testdata", "events")
	pyDir := filepath.Join("..", "..", "..", "..", "tests", "floor", "testdata", "events")
	if _, err := os.Stat(pyDir); err != nil {
		t.Skip("tests/floor/testdata/events does not exist yet. Spec-01 creates it and owns the bytes.")
	}
	compareEventFixtureDirs(t, goDir, pyDir)
}

// compareEventFixtureDirs is what TestEventFixturesMatchTheFloorFixtures
// actually checks, factored out so its own logic has a test that does not
// depend on tests/floor/testdata/events existing (TestCompareEventFixtureDirsCatchesRealDivergence,
// below, exercises it directly against synthetic directories).
//
// Two things a sha256 comparison used to get wrong:
//
//   - It compared raw bytes, so a fixture that means the same event in both
//     languages - different key order, different float formatting, a
//     trailing newline one side has and the other does not - would fail a
//     check that has nothing to do with what actually needs to agree: what
//     ParseStateEvent returns. Comparing the parsed PaneStateEvent values
//     is the real requirement spec-02 is naming.
//   - It only ever walked goDir's own entries, so a fixture that exists
//     ONLY on the Python side - added there and never copied to
//     testdata/events - passed silently: it was never in the loop to check
//     at all. Iterating the union of both directories' filenames is what
//     catches that direction too.
//
// fatalT is the subset of *testing.T compareEventFixtureDirs needs. A real
// *testing.T satisfies it natively; TestCompareEventFixtureDirsCatchesRealDivergence's
// own fatalTracker, below, satisfies it too, so that test can observe a
// failure as a recorded fact instead of the escaping runtime.Goexit a real
// t.Fatal triggers.
type fatalT interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

func compareEventFixtureDirs(t fatalT, goDir, pyDir string) {
	t.Helper()
	names := map[string]bool{}
	for _, dir := range []string{goDir, pyDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			names[e.Name()] = true
		}
	}
	if len(names) == 0 {
		t.Fatalf("neither %s nor %s has any fixture", goDir, pyDir)
	}
	for name := range names {
		goPath, pyPath := filepath.Join(goDir, name), filepath.Join(pyDir, name)
		if _, err := os.Stat(goPath); err != nil {
			t.Fatalf("%s exists in %s but has no counterpart in %s. "+
				"Copy it there. Spec-01 is the direction of truth.", name, pyDir, goDir)
			continue
		}
		if _, err := os.Stat(pyPath); err != nil {
			t.Fatalf("%s exists in %s but has no counterpart in %s. "+
				"Copy it there, or delete it here. Spec-01 is the direction of truth.", name, goDir, pyDir)
			continue
		}
		goBytes, err := os.ReadFile(goPath)
		if err != nil {
			t.Fatal(err)
		}
		pyBytes, err := os.ReadFile(pyPath)
		if err != nil {
			t.Fatal(err)
		}
		goEv, err := ParseStateEvent(goBytes)
		if err != nil {
			t.Fatalf("%s (Go copy) does not parse: %v", name, err)
		}
		pyEv, err := ParseStateEvent(pyBytes)
		if err != nil {
			t.Fatalf("%s (Python copy) does not parse: %v", name, err)
		}
		if !reflect.DeepEqual(goEv, pyEv) {
			t.Fatalf("%s parses differently between the Go and Python fixtures:\n  go: %+v\n  py: %+v",
				name, goEv, pyEv)
		}
	}
}

// This is the union-iteration and parsed-event-comparison logic itself,
// proven against synthetic directories rather than tests/floor's, which
// does not exist in this tree yet (see the skip above).
func TestCompareEventFixtureDirsCatchesRealDivergence(t *testing.T) {
	base := `{"v":1,"session_id":"s1","harness":"claude-code","state":"idle","source":"gate","ts":1.0}`
	baseReordered := `{"session_id":"s1","v":1,"source":"gate","harness":"claude-code","ts":1.0,"state":"idle"}`
	different := `{"v":1,"session_id":"s1","harness":"claude-code","state":"working","source":"gate","ts":1.0}`

	t.Run("semantically equal fixtures with different bytes pass", func(t *testing.T) {
		goDir, pyDir := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(goDir, "a.json"), []byte(base), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pyDir, "a.json"), []byte(baseReordered), 0o644); err != nil {
			t.Fatal(err)
		}
		compareEventFixtureDirs(t, goDir, pyDir)
	})

	t.Run("a fixture present only in the Python dir is caught", func(t *testing.T) {
		ft := &fatalTracker{T: t}
		goDir, pyDir := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(pyDir, "only-python.json"), []byte(base), 0o644); err != nil {
			t.Fatal(err)
		}
		compareEventFixtureDirs(ft, goDir, pyDir)
		if !ft.failed {
			t.Fatal("a fixture that exists only in the Python dir was not caught")
		}
	})

	t.Run("a fixture present only in the Go dir is caught", func(t *testing.T) {
		ft := &fatalTracker{T: t}
		goDir, pyDir := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(goDir, "only-go.json"), []byte(base), 0o644); err != nil {
			t.Fatal(err)
		}
		compareEventFixtureDirs(ft, goDir, pyDir)
		if !ft.failed {
			t.Fatal("a fixture that exists only in the Go dir was not caught")
		}
	})

	t.Run("fixtures that parse to different events are caught", func(t *testing.T) {
		ft := &fatalTracker{T: t}
		goDir, pyDir := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(goDir, "a.json"), []byte(base), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pyDir, "a.json"), []byte(different), 0o644); err != nil {
			t.Fatal(err)
		}
		compareEventFixtureDirs(ft, goDir, pyDir)
		if !ft.failed {
			t.Fatal("fixtures with different parsed states were not caught")
		}
	})
}

// fatalTracker wraps a real *testing.T so compareEventFixtureDirs' own
// t.Fatalf calls can be observed by a subtest instead of ending it: it
// implements the one method compareEventFixtureDirs needs (fatalT, below)
// and records that a failure happened rather than calling runtime.Goexit,
// so the surrounding subtest can assert on the outcome instead of on the
// escaping failure itself.
type fatalTracker struct {
	*testing.T
	failed bool
}

func (f *fatalTracker) Fatalf(format string, args ...any) {
	f.failed = true
	f.Logf(format, args...)
}

func (f *fatalTracker) Fatal(args ...any) {
	f.failed = true
	f.Log(args...)
}
