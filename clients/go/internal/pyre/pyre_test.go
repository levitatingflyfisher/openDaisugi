package pyre

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"daisugi-verify/internal/pystr"
)

func search(t *testing.T, p, s string) bool {
	t.Helper()
	re, err := Compile(p, 0)
	if err != nil {
		t.Fatalf("%q: %v", p, err)
	}
	return re.Search(s)
}

func TestSearchBasics(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{`\bcoppice\b[^\n;&|]*?\bagent\b[\s'"]+(allow)\b`, `coppice agent allow`, true},
		{`\bcoppice\b`, `xcoppice`, false},
		{`\bé\b`, `é`, true},
		{`\w+`, `ß`, true},
		{`^a$`, "a\n", true},
		{`(?i)k`, "K", true},
		{`(a)\1`, "aa", true},
		{`(?<=a)b`, "ab", true},
		{`(?<!a)b`, "ab", false},
		{`(?>a+)a`, "aaa", false},
		{`a++a`, "aaa", false},
		{`(a)?(?(1)b|c)`, "c", true},
		{`\s`, " ", true},
		{`\d`, "٣", true},
		{`(?a)\d`, "٣", false},
	}
	for _, c := range cases {
		if got := search(t, c.p, c.s); got != c.want {
			t.Errorf("search(%q, %q) = %v, want %v", c.p, c.s, got, c.want)
		}
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		`x{2,1}`:   "min repeat greater than max repeat at position 2",
		`a**`:      "multiple repeat at position 2",
		`(?<=a+)b`: "look-behind requires fixed-width pattern",
		`(`:        "missing ), unterminated subpattern at position 0",
		"a\n(":     "missing ), unterminated subpattern at position 2 (line 2, column 1)",
		`\q`:       `bad escape \q at position 0`,
		`[z-a]`:    "bad character range z-a at position 1",
	}
	for p, want := range cases {
		_, err := Compile(p, 0)
		if err == nil || err.String() != want {
			t.Errorf("Compile(%q) error %v, want %q", p, err, want)
		}
	}
}

// TestAgainstOracle reads testdata/oracle.py's output from
// DAISUGI_PYRE_ORACLE.
func TestAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_PYRE_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_PYRE_ORACLE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	n, bad := 0, 0
	for sc.Scan() {
		var row struct {
			P, S string
			Err  string
			Span []int
			Regs [][]int
			Warn int
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		re, perr := Compile(row.P, 0)
		got := ""
		if perr != nil {
			if perr.Type == "unported" {
				continue
			}
			got = perr.Type + ": " + perr.String()
			if perr.Type == "error" {
				got = "error: " + perr.String()
			}
		}
		fail := func(msg string, args ...any) {
			bad++
			if bad <= 15 {
				t.Errorf("%q on %q: "+msg, append([]any{row.P, row.S}, args...)...)
			}
		}
		if row.Err != "" || got != "" {
			if got != row.Err {
				fail("error %q, want %q", got, row.Err)
			}
			continue
		}
		if len(re.Warnings()) != row.Warn {
			fail("%d warnings, want %d", len(re.Warnings()), row.Warn)
		}
		s, e, marks, ok := re.SearchGroups(pystr.Runes(row.S), 0, false)
		if ok != (row.Span != nil) {
			fail("match %v, want %v", ok, row.Span)
			continue
		}
		if !ok {
			continue
		}
		if s != row.Span[0] || e != row.Span[1] {
			fail("span %d-%d, want %v", s, e, row.Span)
			continue
		}
		for g := 1; g < len(row.Regs); g++ {
			if marks[2*g] != row.Regs[g][0] || marks[2*g+1] != row.Regs[g][1] {
				fail("group %d %d-%d, want %v", g, marks[2*g], marks[2*g+1], row.Regs[g])
				break
			}
		}
	}
	t.Logf("%d rows, %d differ", n, bad)
}
