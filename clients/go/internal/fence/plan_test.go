package fence

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type goldenCase struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Input  Input  `json:"input"`
	Expect Output `json:"expect"`
}

// TestGoldenRuleList is the oracle's rule list, case by case.
func TestGoldenRuleList(t *testing.T) {
	f, err := os.Open("../../../fixtures/fence/cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		var c goldenCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		n++
		got := Plan(c.Input)
		if !reflect.DeepEqual(got, c.Expect) {
			gj, _ := json.Marshal(got)
			ej, _ := json.Marshal(c.Expect)
			t.Errorf("%s (%s):\n got %s\nwant %s", c.Name, c.ID, gj, ej)
		}
	}
	if n < 40 {
		t.Fatalf("only %d golden cases", n)
	}
}
