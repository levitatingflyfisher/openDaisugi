package tsbash

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Dump renders a tree the way clients/go/internal/tsbash/testdata/dump.py
// renders py-tree-sitter's tree, so the two can be compared line by line.
func Dump(t *Tree) string {
	var b strings.Builder
	fmt.Fprintf(&b, "E%v", t.HasError)
	var walk func(i int)
	walk = func(i int) {
		n := t.Nodes[i]
		fmt.Fprintf(&b, "(%s %d %d", n.Type, n.Start, n.End)
		if n.Missing {
			b.WriteString(" M")
		}
		if n.Name >= 0 {
			fmt.Fprintf(&b, " N%d:%d", t.Nodes[n.Name].Start, t.Nodes[n.Name].End)
		}
		for _, c := range n.Children {
			walk(c)
		}
		b.WriteString(")")
	}
	walk(0)
	return b.String()
}

func TestParseShapes(t *testing.T) {
	cases := map[string]string{
		"ls":         "Efalse(program 0 2(command 0 2 N0:2(command_name 0 2(word 0 2))))",
		"a && b":     "Efalse(program 0 6(list 0 6(command 0 1 N0:1(command_name 0 1(word 0 1)))(&& 2 4)(command 5 6 N5:6(command_name 5 6(word 5 6)))))",
		"echo 'x":    "",
		"":           "Efalse(program 0 0)",
		"echo héllo": "Efalse(program 0 11(command 0 11 N0:4(command_name 0 4(word 0 4))(word 5 11)))",
	}
	for src, want := range cases {
		tree, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		got := Dump(tree)
		if want == "" {
			if !tree.HasError {
				t.Errorf("%q: want a parse error, got %s", src, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%q:\n got %s\nwant %s", src, got, want)
		}
	}
}

// TestDumpAgainstOracle compares against dumps written by testdata/dump.py
// when DAISUGI_TSBASH_ORACLE names its output (JSON lines of
// {"src": ..., "dump": ...}).
func TestDumpAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_TSBASH_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_TSBASH_ORACLE is not set")
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
		var row struct{ Src, Dump string }
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		tree, err := Parse([]byte(row.Src))
		if err != nil {
			t.Fatal(err)
		}
		n++
		if got := Dump(tree); got != row.Dump {
			bad++
			if bad <= 5 {
				t.Errorf("%q:\n got %s\nwant %s", row.Src, got, row.Dump)
			}
		}
	}
	t.Logf("%d trees, %d differ", n, bad)
	if bad > 0 {
		t.Fail()
	}
}
