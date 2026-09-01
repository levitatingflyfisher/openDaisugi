package pathways

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"daisugi-verify/internal/verify"
)

const bundleJSON = `{"opendaisugi_version": "x", "schema_version": 1, "pathway": {"id": "pw_1",
 "task_description": "build", "task_embedding": [0.5, 0.5], "embedding_model": "lexical-hash-v1",
 "embedding_model_version": "3", "envelope": {"id": "env_1", "generated_by": "t", "task": "t",
 "permissions": {"shell": true, "shell_allowlist": ["make"], "max_execution_time_s": 0}},
 "plan_template": {"id": "plan_1", "source": "s", "task": "t", "steps": [
 {"id": "s1", "type": "shell", "command": "make test"}]}, "source_trace_ids": ["t1"],
 "distilled_at": 1.5}}`

var marker = regexp.MustCompile(`\(echo "(DAISUGI-MARK-\d+)"\)`)

// standIn answers every Z3 check with verdict, as `z3 -in` would print it.
func standIn(verdict string, err error) func(string) (string, error) {
	return func(cmds string) (string, error) {
		m := marker.FindStringSubmatch(cmds)
		if m == nil {
			return "", nil // (reset)
		}
		return verdict + "\n" + m[1] + "\n", err
	}
}

func TestZ3UnknownRefusesTheImport(t *testing.T) {
	old := verify.InProcessZ3
	defer func() { verify.InProcessZ3 = old }()
	verify.InProcessZ3 = standIn("unknown", nil)
	p, err := ParseBundle(bundleJSON, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := Verify(p, 7)
	if err != nil || refusal == nil || refusal.Code != "VERIFICATION_TIMEOUT" ||
		!strings.Contains(refusal.Msg, "exceeded 7ms") || !strings.Contains(refusal.Msg, "raise --z3-timeout-ms") {
		t.Fatalf("got %v, %v", refusal, err)
	}
}

func TestZ3ErrorIsAViolationNotAPass(t *testing.T) {
	old := verify.InProcessZ3
	defer func() { verify.InProcessZ3 = old }()
	verify.InProcessZ3 = standIn("", errors.New("invalid timeout"))
	p, err := ParseBundle(strings.Replace(bundleJSON, `"max_execution_time_s": 0`, `"max_execution_time_s": 30`, 1), "b.json")
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := Verify(p, 500)
	if err != nil || refusal == nil || refusal.Code != "VERIFICATION_FAILED" || !strings.Contains(refusal.Msg, "[z3]") {
		t.Fatalf("a Z3 error passed the import: %v, %v", refusal, err)
	}
}

func TestUnstorableRowsAreRefusedBeforeWriting(t *testing.T) {
	cases := map[string]string{
		"surrogate task":    strings.Replace(bundleJSON, `"build"`, `"b\ud800"`, 1),
		"surrogate command": strings.Replace(bundleJSON, `"make test"`, `"make \udfff"`, 1),
		"metadata depth":    strings.Replace(bundleJSON, `"command": "make test"`, `"command": "make test", "metadata": {"n": `+strings.Repeat("[", 254)+strings.Repeat("]", 254)+`}`, 1),
	}
	for name, text := range cases {
		p, err := ParseBundle(text, "b.json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var ie *ImportError
		if err := Storable(p); !errors.As(err, &ie) || ie.Code != "UNSTORABLE" {
			t.Errorf("%s: Storable() = %v, want UNSTORABLE", name, err)
		}
		if _, err := p.Row(); err == nil {
			t.Errorf("%s: Row() made a row", name)
		}
	}
	// The JSON reader takes 196 levels of metadata and not 197, as the
	// oracle's does.
	for d, want := range map[int]bool{196: true, 197: false} {
		text := strings.Replace(bundleJSON, `"command": "make test"`, `"command": "make test", "metadata": {"n": `+
			strings.Repeat("[", d)+"1"+strings.Repeat("]", d)+`}`, 1)
		p, err := ParseBundle(text, "b.json")
		if err != nil {
			t.Fatal(err)
		}
		if got := Storable(p) == nil; got != want {
			t.Errorf("depth %d: storable %v, want %v", d, got, want)
		}
	}
}

func TestOverwriteKeepsTheOldRowWhenTheNewOneCannotBeWritten(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	good, err := ParseBundle(bundleJSON, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutPathway(good); err != nil {
		t.Fatal(err)
	}
	bad, err := ParseBundle(bundleJSON, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	bad.Obj.Set("distilled_at", math.NaN())
	if _, err := s.ReplacePathway(bad); err == nil {
		t.Fatal("a NaN distilled_at was written")
	}
	rows, _ := s.All()
	if len(rows) != 1 || rows[0]["distilled_at"] != 1.5 {
		t.Fatalf("the old row went: %v", rows)
	}
	existed, err := s.ReplacePathway(good)
	if err != nil || !existed {
		t.Fatalf("replace: %v %v", existed, err)
	}
}

func TestOpenTakesAFileColonPathAsAName(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd) //nolint:errcheck
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open("file:zz/pathways.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, "file:zz", "pathways.db")); err != nil {
		t.Fatalf("the database is not at ./file:zz/pathways.db: %v", err)
	}
}

// A skill delegation whose subsumption check answers unknown is a timeout,
// as the oracle's check_skill_delegations has it: the import refuses with
// VERIFICATION_TIMEOUT, not VERIFICATION_FAILED. The contract declares no
// file or MCP globs, so the unknown reaches the final subsumption check.
func TestZ3UnknownInSkillSubsumptionIsATimeout(t *testing.T) {
	old := verify.InProcessZ3
	defer func() { verify.InProcessZ3 = old }()
	verify.InProcessZ3 = standIn("unknown", nil)
	skill := `{"id": "k1", "type": "skill", "skill_id": "pdf", "contract_envelope": {"generated_by": "g", "task": "t",
 "permissions": {"shell": true, "shell_allowlist": ["make"]}}}`
	for _, stakes := range []string{``, `"stakes": "high", `} {
		b := strings.Replace(bundleJSON, `{"id": "s1", "type": "shell", "command": "make test"}`, skill, 1)
		b = strings.Replace(b, `"max_execution_time_s": 0`, `"max_execution_time_s": 30`, 1)
		b = strings.Replace(b, `"permissions":`, stakes+`"permissions":`, 1)
		p, err := ParseBundle(b, "b.json")
		if err != nil {
			t.Fatal(err)
		}
		refusal, err := Verify(p, 7)
		if err != nil || refusal == nil || refusal.Code != "VERIFICATION_TIMEOUT" ||
			!strings.HasPrefix(refusal.Msg, "verifier timed out (Z3 subsumption check exceeded 7ms") {
			t.Fatalf("stakes %q: got %v, %v", stakes, refusal, err)
		}
		if stakes != `` && refusal.Msg != "verifier timed out (Z3 subsumption check exceeded 7ms); raise --z3-timeout-ms" {
			t.Fatalf("a strict timeout ran on: %v", refusal)
		}
	}
}
