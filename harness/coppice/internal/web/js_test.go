package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// passCount matches Node's own "# pass N" summary line.
var passCount = regexp.MustCompile(`# pass (\d+)`)

// testCallLine matches a test( call at the start of a line, the shape
// every test in this suite uses to register one. A test( that shows up
// inside a string or a comment never starts a line this way, so neither is
// counted.
var testCallLine = regexp.MustCompile(`(?m)^test\(`)

// declaredTestCount counts how many test( calls path declares.
func declaredTestCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(testCallLine.FindAllIndex(raw, -1))
}

// The client's pure logic is JavaScript, so its unit tests are Node's. This
// runs them inside go test when node is here and skips with the reason when
// it is not, which is the rule for every live test in this module. When
// COPPICE_REQUIRE_TOOLCHAIN is set, a missing node fails the test instead of
// skipping it, so a broken CI machine cannot go green by skipping silently.
//
// Each file runs in its own node process, and its own pass count is held
// against its own declared test( count. A single combined run whose total
// pass count only has to clear a floor tied to the number of files, not
// the number of tests, would let one file's tests silently stop
// registering or running as long as the rest of the suite still carries
// the total past that floor. Checking one file at a time against what it
// itself declares closes that gap.
func TestJSUnitTestsPass(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		reason := "node is not on PATH, so the client's JS unit tests did not run"
		if os.Getenv("COPPICE_REQUIRE_TOOLCHAIN") != "" {
			t.Fatalf("COPPICE_REQUIRE_TOOLCHAIN is set: %s", reason)
		}
		t.Skip(reason)
	}

	files, err := filepath.Glob("static/_tests/*.test.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no *.test.mjs files found under static/_tests, so the loop below would run nothing")
	}

	for _, f := range files {
		want := declaredTestCount(t, f)
		if want == 0 {
			t.Fatalf("%s declares no test( calls at the start of a line", f)
		}

		out, err := exec.Command("node", "--test", f).CombinedOutput()
		if err != nil {
			t.Fatalf("node --test %s failed:\n%s", f, out)
		}

		// A file whose tests were accidentally commented out, or whose
		// import throws, still exits 0 as long as nothing ran red, so the
		// exit code alone is not proof the suite ran. Require an explicit
		// "# fail 0" and a pass count at least as large as the number of
		// test( calls this one file declares, so a file whose tests
		// silently stopped registering, or stopped running, fails this
		// gate even while every other file stays green.
		if !strings.Contains(string(out), "# fail 0") {
			t.Fatalf("node --test %s did not report # fail 0:\n%s", f, out)
		}
		m := passCount.FindStringSubmatch(string(out))
		if m == nil {
			t.Fatalf("node --test %s printed no \"# pass N\" line:\n%s", f, out)
		}
		pass, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("could not read the pass count %q for %s: %v", m[1], f, err)
		}
		if pass < want {
			t.Fatalf("%s passed %d tests, fewer than the %d test( calls it declares:\n%s", f, pass, want, out)
		}
	}
}
