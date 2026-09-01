package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// clients/fixtures/dialect/globs.json is written by clients/dialect_cases.py
// from the oracle: dialect.glob_regex for each glob, and which normalized
// paths the file-scope matcher matches with it.
const globsFixture = "../../../fixtures/dialect/globs.json"

func TestDialectHashIsTheHashOfTheDefinitions(t *testing.T) {
	sum := sha256.Sum256([]byte(DialectJSON))
	if got := hex.EncodeToString(sum[:])[:16]; got != DialectHash {
		t.Fatalf("sha256(DialectJSON)[:16] = %s, want %s", got, DialectHash)
	}
}

func TestGlobRegexMatchesTheOracle(t *testing.T) {
	raw, err := os.ReadFile(globsFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Paths []string `json:"paths"`
		Globs []struct {
			Glob    string    `json:"glob"`
			Regex   *string   `json:"regex"`
			Error   *string   `json:"error"`
			Matches *[]string `json:"matches"`
		} `json:"globs"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for _, g := range fx.Globs {
		regex, err := GlobRegex(g.Glob)
		if g.Error != nil {
			if err == nil || err.Error() != *g.Error {
				t.Errorf("GlobRegex(%q) err = %v, want %q", g.Glob, err, *g.Error)
			}
			continue
		}
		if err != nil || regex != *g.Regex {
			t.Errorf("GlobRegex(%q) = %q, %v; want %q", g.Glob, regex, err, *g.Regex)
			continue
		}
		re, err := compilePyRegex(regex)
		if err != nil {
			t.Errorf("compile %q: %v", regex, err)
			continue
		}
		want := map[string]bool{}
		for _, p := range *g.Matches {
			want[p] = true
		}
		for _, p := range fx.Paths {
			if re.MatchString(p) != want[p] {
				t.Errorf("glob %q regex %q on %q: got %v, want %v", g.Glob, regex, p, !want[p], want[p])
			}
		}
	}
}

func TestSynonymsNameOneWord(t *testing.T) {
	for _, name := range []string{"file_unchanged", "read_only", "no_modifications", "file_immutable", "file_preservation", "keep_unchanged"} {
		if w, ok := WordFor(name); !ok || w != "keep_unchanged" {
			t.Errorf("WordFor(%q) = %q, %v", name, w, ok)
		}
	}
	if _, ok := WordFor("no_force_push"); ok {
		t.Error("no_force_push is not a word")
	}
}

func TestTranslatePyRegexBridgesEscapes(t *testing.T) {
	for in, want := range map[string]string{
		`^a\Z`:        `^a\z`,
		`\\Z`:         `\\Z`,
		`\u2014`:      `\x{2014}`,
		`\\u2014`:     `\\u2014`,
		`\u20`:        `\u20`,
		`a\.b`:        `a\.b`,
		`trailing\`:   `trailing\`,
		`\\\u00e9\Z`:  `\\\x{00e9}\z`,
		`[^/]*\.py\Z`: `[^/]*\.py\z`,
	} {
		if got := translatePyRegex(in); got != want {
			t.Errorf("translatePyRegex(%q) = %q, want %q", in, got, want)
		}
	}
}

// clients/fixtures/dialect/writes.json is write_paths.step_write_paths for
// a fixed set of shell commands, with no base and with one.
func TestStepWritePathsMatchTheOracle(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/dialect/writes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Base     string `json:"base"`
		Commands []struct {
			Command string    `json:"command"`
			Writes  *[]string `json:"writes"`
			Placed  *[]string `json:"placed"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	same := func(got []string, ok bool, want *[]string) bool {
		if want == nil || !ok {
			return want == nil && !ok
		}
		if len(got) != len(*want) {
			return false
		}
		for i := range got {
			if got[i] != (*want)[i] {
				return false
			}
		}
		return true
	}
	for _, c := range fx.Commands {
		step := map[string]any{"id": "s1", "type": "shell", "command": c.Command}
		if got, ok := StepWritePaths(step, ""); !same(got, ok, c.Writes) {
			t.Errorf("%q: got %q %v, want %v", c.Command, got, ok, c.Writes)
		}
		if got, ok := StepWritePaths(step, fx.Base); !same(got, ok, c.Placed) {
			t.Errorf("%q placed: got %q %v, want %v", c.Command, got, ok, c.Placed)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	for _, c := range []struct{ target, base, want, err string }{
		{"src/**", "/repo", "/repo/src/**", ""},
		{"**", "/repo", "**", ""},
		{"/abs/**", "/repo", "/abs/**", ""},
		{"src/**", "", "src/**", ""},
		{"src/**", "/", "/src/**", ""},
		{"src/**", "/re*po", "", "the working directory holds a glob character"},
		{"~/x/**", "/repo", "", "a target that starts with ~ cannot be placed from the cwd"},
		{"./a.py", "/repo", "", "a relative target with a . or .. segment cannot be placed"},
	} {
		got, err := ResolveTarget(c.target, c.base)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("ResolveTarget(%q, %q) err = %v, want %q", c.target, c.base, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ResolveTarget(%q, %q) = %q, %v; want %q", c.target, c.base, got, err, c.want)
		}
	}
}
