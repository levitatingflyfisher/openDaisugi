package verify

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The subset fixtures are shared with the Python and Rust unit tests.
const fixtureDir = "../../../fixtures/"

type subsetRow struct {
	Pattern string `json:"pattern"`
	Ok      bool   `json:"ok"`
}

type matchRow struct {
	Pattern string   `json:"pattern"`
	Probes  []string `json:"probes"`
	Match   string   `json:"match"`
}

func readFixture(t *testing.T, name string, into interface{}) {
	t.Helper()
	raw, err := os.ReadFile(fixtureDir + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func TestTheValidatorGivesTheFixtureVerdict(t *testing.T) {
	var rows []subsetRow
	readFixture(t, "rule_regex_subset.json", &rows)
	if len(rows) < 300 {
		t.Fatalf("only %d rows", len(rows))
	}
	for _, r := range rows {
		if got := RuleRegexSupported(r.Pattern); got != r.Ok {
			t.Errorf("%q: supported = %v, want %v", r.Pattern, got, r.Ok)
		}
		if got := RuleRegexValid(r.Pattern); got != r.Ok {
			t.Errorf("%q: valid = %v, want %v", r.Pattern, got, r.Ok)
		}
	}
}

func TestTheEngineAnswersTheRecordedMatches(t *testing.T) {
	var rows []matchRow
	readFixture(t, "rule_regex_match.json", &rows)
	for _, r := range rows {
		var got strings.Builder
		for _, p := range r.Probes {
			ok, err := RuleRegexFull(r.Pattern, p)
			if err != nil {
				t.Fatalf("%q: %v", r.Pattern, err)
			}
			if ok {
				got.WriteByte('1')
			} else {
				got.WriteByte('0')
			}
		}
		if got.String() != r.Match {
			t.Errorf("%q: the engines disagree on a probe", r.Pattern)
		}
	}
}

func TestAPatternWithALoneSurrogateIsOutsideTheSubset(t *testing.T) {
	if RuleRegexSupported("a\xed\xa0\x80") || RuleRegexSupported("[\xed\xb0\x80]") {
		t.Fatal("a surrogate in a pattern")
	}
}

func TestAValueThatIsNotValidUTF8DeniesBeforeMatching(t *testing.T) {
	want := "argument rule refused: argument 'a' is not valid UTF-8 text"
	for _, v := range []string{"\xed\xa0\x80", "x\xed\xbf\xbfy", "\xff"} {
		if got := RuleTextRefusal("^evil$", v, "a"); got != want {
			t.Errorf("%q: got %q", v, got)
		}
		_, err := evalScalar(NotMatches{Path: "a", Regex: "^evil$"}, map[string]interface{}{"a": v}, &pathContext{})
		if r, ok := err.(ruleRefused); !ok || r.msg != want {
			t.Errorf("not_matches on %q: got %v", v, err)
		}
		_, err = evalScalar(Matches{Path: "a", Regex: ".*"}, map[string]interface{}{"a": v}, &pathContext{})
		if r, ok := err.(ruleRefused); !ok || r.msg != want {
			t.Errorf("matches on %q: got %v", v, err)
		}
	}
	// A neighbour: the replacement character itself is valid text.
	if got := RuleTextRefusal("^evil$", "good�", "a"); got != "" {
		t.Errorf("a valid value: got %q", got)
	}
	// A pattern outside the subset is named first.
	if got := RuleTextRefusal("(?x)a", "\xff", "a"); got != "argument rule refused: regex '(?x)a' is outside the supported subset" {
		t.Errorf("got %q", got)
	}
}

func TestBacktrackingShapesAreInsideTheSubsetAndAnswerAtOnce(t *testing.T) {
	start := time.Now()
	for _, rx := range []string{"^(a|aa)+$", "^(a+)+$", "^(.*)*$", "^a*a*a*a*b$"} {
		if !RuleRegexSupported(rx) {
			t.Fatal(rx)
		}
		for _, v := range []string{strings.Repeat("a", 4096), strings.Repeat("a", 4095) + "!"} {
			if _, err := RuleRegexFull(rx, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("slow")
	}
}
