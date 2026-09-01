package detect

import (
	"strings"
	"testing"
)

func compile(t *testing.T, body string) *Compiled {
	t.Helper()
	_, c, err := Parse("t.toml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHighestPriorityWins(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "low"
state = "idle"
priority = 10
contains = ["marker"]
[[rules]]
id = "high"
state = "blocked"
priority = 20
contains = ["marker"]
`)
	r := c.Evaluate(Input{Screen: "marker"})
	if !r.Matched || r.RuleID != "high" || r.State != StateBlocked {
		t.Fatalf("Evaluate = %+v, want the high rule", r)
	}
}

// Herdr keeps the earlier rule on a tie: `previous.priority >= rule.priority`
// does not replace. Porting that exactly matters, because several bundled
// manifests have equal-priority rules whose order is the tiebreak.
func TestATieKeepsTheEarlierRule(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "first"
state = "working"
priority = 5
contains = ["marker"]
[[rules]]
id = "second"
state = "idle"
priority = 5
contains = ["marker"]
`)
	if r := c.Evaluate(Input{Screen: "marker"}); r.RuleID != "first" {
		t.Fatalf("tie went to %q, want first", r.RuleID)
	}
}

func TestContainsIsCaseInsensitiveAndRegexIsNot(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "ci"
state = "blocked"
contains = ["Do You Want To Proceed?"]
[[rules]]
id = "cs"
state = "working"
priority = -1
regex = ['^Exact$']
`)
	if r := c.Evaluate(Input{Screen: "do you want to proceed?"}); r.RuleID != "ci" {
		t.Fatalf("contains did not match case-insensitively: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "exact"}); r.Matched {
		t.Fatalf("regex matched case-insensitively: %+v", r)
	}
}

// Each line_regex must match SOME line, not all the same line.
func TestEachLineRegexMatchesSomeLineIndependently(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "two"
state = "blocked"
line_regex = ['^alpha$', '^beta$']
`)
	if r := c.Evaluate(Input{Screen: "alpha\nbeta"}); !r.Matched {
		t.Fatalf("two line_regex over two lines did not match: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "alpha"}); r.Matched {
		t.Fatalf("matched with only one of the two lines present: %+v", r)
	}
}

func TestAnyIsIgnoredWhenEmptyAndEnforcedWhenPresent(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "r"
state = "blocked"
contains = ["base"]
any = [{ contains = ["one"] }, { contains = ["two"] }]
`)
	if r := c.Evaluate(Input{Screen: "base"}); r.Matched {
		t.Fatalf("any was not enforced: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "base two"}); !r.Matched {
		t.Fatalf("any with a satisfied branch did not match: %+v", r)
	}
}

func TestNotBlocksAMatch(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "r"
state = "idle"
contains = ["prompt"]
not = [{ contains = ["esc to cancel"] }]
`)
	if r := c.Evaluate(Input{Screen: "prompt"}); !r.Matched {
		t.Fatal("the rule did not match without the excluded text")
	}
	if r := c.Evaluate(Input{Screen: "prompt, esc to cancel"}); r.Matched {
		t.Fatal("not did not block the match")
	}
}

// skip_state_update means emit nothing. If it produced "unknown" instead, a
// Claude pane sitting in the transcript viewer would be reset to unknown every
// 500 ms, wiping whatever the gate told us.
func TestSkipStateUpdateEmitsNothing(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "viewer"
state = "unknown"
priority = 100
skip_state_update = true
contains = ["showing detailed transcript"]
`)
	r := c.Evaluate(Input{Screen: "showing detailed transcript"})
	if !r.Matched || !r.Skip {
		t.Fatalf("Evaluate = %+v, want Matched with Skip set", r)
	}
}

// No match means no state. Herdr defaults a known agent to idle here; master
// spec 3.1 forbids that, so the port stops short on purpose.
func TestNoMatchYieldsNoStateNotIdle(t *testing.T) {
	c := compile(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"idle\"\ncontains=[\"never\"]\n")
	r := c.Evaluate(Input{Screen: "something else"})
	if r.Matched {
		t.Fatalf("Evaluate matched nothing but reported %+v", r)
	}
	if r.State == StateIdle {
		t.Fatal("no match produced idle, which master spec 3.1 forbids")
	}
}

func TestEvaluatedCarriesEveryRuleForExplain(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "a"
state = "idle"
contains = ["nope"]
[[rules]]
id = "b"
state = "working"
contains = ["yes"]
`)
	r := c.Evaluate(Input{Screen: "yes"})
	if len(r.Evaluated) != 2 {
		t.Fatalf("Evaluated has %d entries, want 2", len(r.Evaluated))
	}
	byID := map[string]Evaluated{}
	for _, e := range r.Evaluated {
		byID[e.ID] = e
	}
	if byID["a"].Matched || !byID["b"].Matched {
		t.Fatalf("Evaluated matched flags are wrong: %+v", r.Evaluated)
	}
	if !strings.Contains(byID["b"].RegionPreview, "yes") {
		t.Fatalf("the preview %q does not show what was matched", byID["b"].RegionPreview)
	}
}

// An accept-path test of nested
// all/any/not composition, two levels deep, exercising every one of the three
// gate kinds nested inside each other.
func TestNestedAllAnyNotCompositionMatchesCorrectly(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "r"
state = "blocked"
contains = ["base"]
all = [
  { any = [
    { contains = ["one"] },
    { contains = ["two"], not = [{ contains = ["exclude"] }] },
  ] },
]
`)
	if r := c.Evaluate(Input{Screen: "base two"}); !r.Matched {
		t.Fatalf("nested all > any > (contains + not) should match: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "base one"}); !r.Matched {
		t.Fatalf("nested all > any > contains (the other any branch) should match: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "base two exclude"}); r.Matched {
		t.Fatalf("the nested not should have blocked this match: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "base"}); r.Matched {
		t.Fatalf("neither any branch is satisfied by base alone: %+v", r)
	}
}

// A no-match Result must report State as StateUnknown,
// never the zero value "". A caller that switches on r.State without also
// checking r.Matched should see "unknown", not an empty string that belongs
// to no declared State constant.
func TestNoMatchResultStateIsUnknownNotEmpty(t *testing.T) {
	c := compile(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"idle\"\ncontains=[\"never\"]\n")
	r := c.Evaluate(Input{Screen: "something else"})
	if r.Matched {
		t.Fatal("expected no match")
	}
	if r.State != StateUnknown {
		t.Fatalf("State = %q, want %q", r.State, StateUnknown)
	}
}
