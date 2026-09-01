package pyjson

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/indent.json holds json.dumps(v, indent=2) output from Python,
// with and without ensure_ascii.
func TestDumpsIndentMatchesPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/indent.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		In, ASCII, UTF8 string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		v, err := Loads(c.In)
		if err != nil {
			t.Fatalf("Loads(%q): %v", c.In, err)
		}
		if got := DumpsIndent(v, 2, true); got != c.ASCII {
			t.Errorf("ascii %q:\n got %s\nwant %s", c.In, got, c.ASCII)
		}
		if got := DumpsIndent(v, 2, false); got != c.UTF8 {
			t.Errorf("utf8 %q:\n got %s\nwant %s", c.In, got, c.UTF8)
		}
	}
}

func TestDeleteKeepsOrder(t *testing.T) {
	o := NewObject().Set("a", 1).Set("b", 2).Set("c", 3)
	o.Delete("b")
	o.Delete("zz")
	if got := Dumps(o, true); got != `{"a": 1, "c": 3}` {
		t.Fatal(got)
	}
	o.Set("b", 4)
	if got := Dumps(o, true); got != `{"a": 1, "c": 3, "b": 4}` {
		t.Fatal(got)
	}
}
