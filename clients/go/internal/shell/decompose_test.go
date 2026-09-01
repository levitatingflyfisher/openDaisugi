package shell

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDecomposeShapes(t *testing.T) {
	cases := []struct {
		src    string
		ok     bool
		heads  []string
		reason string
	}{
		{"git status && rm -rf /", true, []string{"git", "rm"}, ""},
		{"git $(echo status)", true, []string{"git", "echo"}, ""},
		{"echo @(a|b) ; ls", false, nil, "malformed shell (parse error)"},
		{"echo hi >out more", false, nil, "ambiguous shell (a redirect with more than one target '>out more')"},
		{"$CMD x", false, nil, "non-literal command head ('$CMD')"},
		{"cat >b <<EOF\nx\nEOF", true, []string{"cat"}, ""},
		{"echo héllo | wc", true, []string{"echo", "wc"}, ""},
		{"", false, nil, "no command heads found"},
	}
	for _, c := range cases {
		d, err := Decompose(c.src, 1)
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if d.OK != c.ok || d.Reason != c.reason || (c.ok && !reflect.DeepEqual(d.Heads, c.heads)) {
			t.Errorf("%q: got ok=%v heads=%q reason=%q", c.src, d.OK, d.Heads, d.Reason)
		}
	}
}

func TestDeepChainRaisesRecursionError(t *testing.T) {
	cmd := "ls" + strings.Repeat(" && ls", 10000)
	_, err := Decompose(cmd, 1)
	if err == nil || err.Type != "RecursionError" {
		t.Fatalf("want RecursionError, got %v", err)
	}
}

func TestSurrogateRaisesUnicodeEncodeError(t *testing.T) {
	_, err := Decompose("ls \xed\xa0\x80", 1)
	want := `'utf-8' codec can't encode character '\ud800' in position 3: surrogates not allowed`
	if err == nil || err.Type != "UnicodeEncodeError" || err.Msg != want {
		t.Fatalf("got %v", err)
	}
}

// The grammar's scanner splits words at iswspace, which reads LC_CTYPE.
// CPython runs with a UTF-8 LC_CTYPE (it coerces the C locale), where
// U+2029 is a space after a non-ASCII letter; the port sets the same.
func TestScannerSeesUnicodeSpaceAsCPythonDoes(t *testing.T) {
	d := DecomposeCommand("rm > -中 r")
	if d.OK || !strings.Contains(d.Reason, "more than one target") {
		t.Fatalf("got %+v", d)
	}
}

// TestAgainstOracle reads testdata/oracle.py's output from
// DAISUGI_DECOMPOSE_ORACLE.
func TestAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_DECOMPOSE_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_DECOMPOSE_ORACLE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	n, bad := 0, 0
	for sc.Scan() {
		var row struct {
			Src                            string
			OK                             bool
			Heads, Commands, Reads, Writes []string
			Reason, Error                  string
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		d, perr := Decompose(row.Src, 1)
		got := ""
		if perr != nil {
			got = perr.Error()
		}
		norm := func(s []string) []string {
			if len(s) == 0 {
				return nil
			}
			return s
		}
		same := got == row.Error && (perr != nil || (d.OK == row.OK && d.Reason == row.Reason &&
			reflect.DeepEqual(norm(d.Heads), norm(row.Heads)) && reflect.DeepEqual(norm(d.Commands), norm(row.Commands)) &&
			reflect.DeepEqual(norm(d.Reads), norm(row.Reads)) && reflect.DeepEqual(norm(d.Writes), norm(row.Writes))))
		if !same {
			bad++
			if bad <= 8 {
				t.Errorf("%q:\n got %+v %s\nwant %+v", row.Src, d, got, row)
			}
		}
	}
	t.Logf("%d commands, %d differ", n, bad)
}
