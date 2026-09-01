package detect

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func loadedSet(t *testing.T) *Set {
	t.Helper()
	s, err := LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	if w := s.Warnings(); len(w) > 0 {
		t.Fatalf("bundled manifests produced warnings, so a pattern or a key drifted: %v", w)
	}
	return s
}

// Every bundled manifest must parse, which means every regex in every rule
// compiles under Go's engine. That is the tripwire for Rust-to-Go regex drift.
func TestEveryBundledManifestLoads(t *testing.T) {
	s := loadedSet(t)
	if len(s.Agents()) != 21 {
		t.Fatalf("%d manifests loaded, want exactly the 21 vendored: %v",
			len(s.Agents()), s.Agents())
	}
}

// The literal substring "commit:" passes even when the vendoring script wrote
// an empty value, which is exactly the broken path. Assert a real SHA.
func TestProvenanceRecordsTheVendoredCommit(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("manifests", "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, key := range []string{"source:", "licence: Apache-2.0"} {
		if !strings.Contains(text, key) {
			t.Fatalf("PROVENANCE is missing %q", key)
		}
	}
	re := regexp.MustCompile(`(?m)^commit:\s*([0-9a-f]{40})\s*$`)
	if !re.MatchString(text) {
		t.Fatalf("PROVENANCE has no 40 character commit sha:\n%s", text)
	}
}

func TestEveryVendoredFileCarriesAnAttributionLine(t *testing.T) {
	entries, err := os.ReadDir("manifests")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		f, err := os.Open(filepath.Join("manifests", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		first := sc.Text()
		_ = f.Close()
		if !strings.Contains(first, "herdrdev/herdr") || !strings.Contains(first, "Apache-2.0") {
			t.Fatalf("%s first line = %q, want the vendoring attribution", e.Name(), first)
		}
	}
}

// screenFixtures walks testdata/screens/<agent>/<state>-N.txt.
func screenFixtures(t *testing.T) map[string]map[string][]string {
	t.Helper()
	root := filepath.Join("..", "..", "testdata", "screens")
	out := map[string]map[string][]string{}
	agents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[a.Name()] = map[string][]string{}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".txt") {
				continue
			}
			state, _, _ := strings.Cut(strings.TrimSuffix(f.Name(), ".txt"), "-")
			out[a.Name()][state] = append(out[a.Name()][state],
				filepath.Join(root, a.Name(), f.Name()))
		}
	}
	return out
}

func unfixtured(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "screens", "unfixtured.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out[l] = true
	}
	return out
}

// Every manifest is either fixtured or listed as unfixtured. A manifest that is
// neither is an untested detection nobody declared.
func TestEveryManifestIsFixturedOrDeclaredUnfixtured(t *testing.T) {
	s := loadedSet(t)
	fx := screenFixtures(t)
	skip := unfixtured(t)
	agents := map[string]bool{}
	for _, id := range s.Agents() {
		agents[id] = true
	}

	var missing []string
	for _, id := range s.Agents() {
		if len(fx[id]) > 0 || skip[id] {
			continue
		}
		missing = append(missing, id)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these manifests have no fixture and are not in unfixtured.txt: %v. "+
			"Add a fixture under testdata/screens/<agent>/, or add the id to unfixtured.txt.",
			missing)
	}

	// unfixtured.txt is a to-do list, not a write-once record: an id that no
	// longer names a real manifest (a re-vendor dropped or renamed it) is a
	// stale line nobody will ever come back to delete on their own.
	var stale []string
	for id := range skip {
		if !agents[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("unfixtured.txt lists ids that are not real manifest ids: %v. "+
			"Remove the stale line.", stale)
	}

	// An id can't be both "no fixture yet" and "has a fixture directory": that
	// contradiction means someone added the fixture and forgot to delete the
	// unfixtured.txt line, which would otherwise sit there forever looking
	// like an honest gap that was already closed.
	var contradictory []string
	for id := range skip {
		if len(fx[id]) > 0 {
			contradictory = append(contradictory, id)
		}
	}
	sort.Strings(contradictory)
	if len(contradictory) > 0 {
		t.Fatalf("these ids are listed in unfixtured.txt but already have a fixture "+
			"directory: %v. Delete the now-stale unfixtured.txt line.", contradictory)
	}
}

// Each fixture must produce the state its filename claims, and - when the
// fixture declares one via "#rule:" - the specific rule its filename's state
// is supposed to come from. Plan 03's Python evaluator shares these fixtures
// and must agree on both; matching state alone would hide a re-vendor that
// keeps the state but shifts the winning rule.
func TestEachFixtureDetectsItsDeclaredState(t *testing.T) {
	s := loadedSet(t)
	for agent, byState := range screenFixtures(t) {
		c, ok := s.For(agent)
		if !ok {
			t.Fatalf("fixtures exist for %q but no manifest declares it", agent)
		}
		for wantState, paths := range byState {
			for _, p := range paths {
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				body := string(b)
				in := inputFromFixture(body)
				r := c.Evaluate(in)
				if !r.Matched {
					t.Fatalf("%s matched no rule, want state %s", p, wantState)
				}
				if string(r.State) != wantState {
					t.Fatalf("%s detected %s by rule %s, want %s",
						p, r.State, r.RuleID, wantState)
				}
				if wantRule := fixtureExpectedRule(body); wantRule != "" && r.RuleID != wantRule {
					t.Fatalf("%s detected state %s via rule %s, want rule %s",
						p, r.State, r.RuleID, wantRule)
				}
			}
		}
	}
}

// Ambiguity means two rules of EQUAL priority match and disagree about the
// state. Two rules of different priority both matching is not ambiguity: that
// is what priority is for, and Herdr's own manifests rely on it.
func TestNoFixtureIsAmbiguousAtEqualPriority(t *testing.T) {
	s := loadedSet(t)
	for agent, byState := range screenFixtures(t) {
		c, _ := s.For(agent)
		for _, paths := range byState {
			for _, p := range paths {
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				r := c.Evaluate(inputFromFixture(string(b)))
				byPriority := map[int]map[State][]string{}
				for _, e := range r.Evaluated {
					if !e.Matched {
						continue
					}
					if byPriority[e.Priority] == nil {
						byPriority[e.Priority] = map[State][]string{}
					}
					byPriority[e.Priority][e.State] = append(byPriority[e.Priority][e.State], e.ID)
				}
				for prio, states := range byPriority {
					if len(states) > 1 {
						t.Fatalf("%s: rules at priority %d disagree: %v. "+
							"Give one of them a different priority in the override, "+
							"or fix the fixture.", p, prio, states)
					}
				}
			}
		}
	}
}

// inputFromFixture reads the optional header lines a fixture may carry.
// "#osc_title: …" and "#osc_progress: …" on the first lines set those fields;
// "#rule: …" (read separately by fixtureExpectedRule) is likewise a header,
// not screen text; everything else is the screen. Headers are recognized
// only as a contiguous run at the TOP of the file, one per line -- see
// testdata/screens/README.md for the format this and fixtureExpectedRule
// both implement. The first line that is not one of the three recognized
// prefixes ends the header run; every line from there on, including one that
// happens to start with "#", is screen text verbatim. Recognizing a header
// prefix anywhere in the file (not just at the top) would risk mangling real
// captured screen content that coincidentally starts the same way.
func splitFixtureHeaders(body string) (headers, screen []string) {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) {
		l := lines[i]
		if strings.HasPrefix(l, "#osc_title:") || strings.HasPrefix(l, "#osc_progress:") ||
			strings.HasPrefix(l, "#rule:") {
			headers = append(headers, l)
			i++
			continue
		}
		break
	}
	return headers, lines[i:]
}

func inputFromFixture(body string) Input {
	in := Input{}
	headers, screen := splitFixtureHeaders(body)
	for _, l := range headers {
		switch {
		case strings.HasPrefix(l, "#osc_title:"):
			in.OSCTitle = strings.TrimSpace(strings.TrimPrefix(l, "#osc_title:"))
		case strings.HasPrefix(l, "#osc_progress:"):
			in.OSCProgress = strings.TrimSpace(strings.TrimPrefix(l, "#osc_progress:"))
		}
	}
	in.Screen = strings.Join(screen, "\n")
	return in
}

// fixtureExpectedRule reads a fixture's optional "#rule: <id>" header: the
// rule id Evaluate is expected to pick, not just the state it asserts. Plan
// 03's Python evaluator shares these same fixtures and must produce the same
// (state, rule) pair for each one; a fixture that only pins state would let a
// re-vendor shift which rule wins on a tied state without either engine's
// test suite noticing. Returns "" when the fixture carries no such header.
func fixtureExpectedRule(body string) string {
	headers, _ := splitFixtureHeaders(body)
	for _, l := range headers {
		if strings.HasPrefix(l, "#rule:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "#rule:"))
		}
	}
	return ""
}

// Headers are recognized only as a contiguous run at the
// TOP of the file, one per line -- the format testdata/screens/README.md
// documents for the Python evaluator to port. The first line that doesn't
// match a recognized "#" prefix ends the header run; everything from there
// on, including a line that happens to start with "#", is screen text
// verbatim. This test proves the returned screen carries none of the header
// lines.
func TestFixtureHeadersAreStrippedBeforeEvaluation(t *testing.T) {
	// No trailing newline in this literal on purpose: a real fixture file
	// does end in one, but Region()'s own line-splitting already normalizes
	// exactly that (see TestBottomLinesKeepsTrailingBlankLinesLikeHerdr's
	// single-terminator rule) -- this test is about header removal, not
	// trailing-newline handling, so it keeps those two concerns apart.
	body := "#rule: should_not_leak\n#osc_title: also stripped\n#osc_progress: 1;2\nreal screen text"
	in := inputFromFixture(body)
	if in.Screen != "real screen text" {
		t.Fatalf("Screen = %q, want no header lines in it", in.Screen)
	}
	if in.OSCTitle != "also stripped" {
		t.Fatalf("OSCTitle = %q", in.OSCTitle)
	}
	if in.OSCProgress != "1;2" {
		t.Fatalf("OSCProgress = %q", in.OSCProgress)
	}
	if got := fixtureExpectedRule(body); got != "should_not_leak" {
		t.Fatalf("fixtureExpectedRule = %q, want should_not_leak", got)
	}
}

// A "#" line AFTER the header run is not a header: it is screen text, exactly
// as testdata/screens/README.md documents (headers are only headers at the
// top). Otherwise a real captured screen that happens to show literal text
// starting with "#rule:" or "#osc_title:" partway down would be silently
// mangled.
func TestAHeaderPrefixAfterScreenTextStartsIsNotStripped(t *testing.T) {
	// No trailing newline, same reasoning as TestFixtureHeadersAreStrippedBeforeEvaluation.
	body := "#rule: real_header\nfirst screen line\n#osc_title: this is screen text, not a header"
	in := inputFromFixture(body)
	want := "first screen line\n#osc_title: this is screen text, not a header"
	if in.Screen != want {
		t.Fatalf("Screen = %q, want %q", in.Screen, want)
	}
	if in.OSCTitle != "" {
		t.Fatalf("OSCTitle = %q, want empty: that line was screen text, not a header", in.OSCTitle)
	}
}

// Demonstrates WHY stripping matters, not just that it happens: a rule whose
// region is the screen's first non-empty line only matches the real content
// when the header above it has actually been removed.
func TestUnstrippedHeaderWouldChangeTheVerdict(t *testing.T) {
	c := compile(t, `
id = "x"
min_engine_version = 3
[[rules]]
id = "clean_first_line"
state = "idle"
region = "top_non_empty_lines(1)"
contains = ["ready"]
`)
	// The header's rule id deliberately does not contain the needle "ready"
	// itself, or the polluted-screen assertion below would pass for the
	// wrong reason (the header text matching by accident, not the header
	// having shifted which line the region actually is).
	body := "#rule: clean_first_line\nready\n"

	// Correct behavior: inputFromFixture strips the header, so the screen's
	// first non-empty line is "ready" and the rule matches.
	r := c.Evaluate(inputFromFixture(body))
	if !r.Matched || r.RuleID != "clean_first_line" {
		t.Fatalf("with the header stripped, want a match on the real first line: %+v", r)
	}

	// If a header were left in -- the bug this whole fixture contract
	// guards against -- the screen's first non-empty line would be the
	// header text itself, not "ready", and the same rule would stop
	// matching even though nothing about the real screen changed.
	r = c.Evaluate(Input{Screen: body})
	if r.Matched {
		t.Fatalf("an unstripped header should have broken the match by shifting "+
			"the region's first line, but got: %+v", r)
	}
}
