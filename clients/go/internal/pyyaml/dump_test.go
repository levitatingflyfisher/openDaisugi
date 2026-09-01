package pyyaml

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"testing"

	"daisugi-verify/internal/pyjson"
)

// fromTyped reads testdata/dump_oracle.py's typed value.
func fromTyped(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = fromTyped(e)
		}
		return out
	case *pyjson.Object:
		if t, ok := x.Get("int"); ok {
			return pyjson.Int{Text: t.(string)}
		}
		if t, ok := x.Get("float"); ok {
			switch t.(string) {
			case "nan":
				return math.NaN()
			case "inf":
				return math.Inf(1)
			case "-inf":
				return math.Inf(-1)
			}
			f, _ := strconv.ParseFloat(t.(string), 64)
			return f
		}
		o := pyjson.NewObject()
		for _, p := range x.Value("dict").([]any) {
			kv := p.([]any)
			o.Set(kv[0].(string), fromTyped(kv[1]))
		}
		return o
	}
	return v
}

func TestSafeDumpSmall(t *testing.T) {
	o := pyjson.NewObject().Set("name", "build-the-release").Set("n", pyjson.Int{Text: "3"}).
		Set("v", "3").Set("xs", []any{1.5, 1e16, nil, true}).Set("empty", []any{}).
		Set("m", pyjson.NewObject().Set("a", "é").Set("b", pyjson.NewObject()))
	want := "name: build-the-release\nn: 3\nv: '3'\nxs:\n- 1.5\n- 1.0e+16\n- null\n- true\nempty: []\nm:\n  a: \"\\xE9\"\n  b: {}\n"
	got, why := SafeDump(o)
	if why != nil || got != want {
		t.Fatalf("got %q (%v)\nwant %q", got, why, want)
	}
}

// TestSafeDumpAgainstOracle reads testdata/dump_oracle.py's output from
// DAISUGI_PYYAML_DUMP_ORACLE.
func TestSafeDumpAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_PYYAML_DUMP_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_PYYAML_DUMP_ORACLE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	n, bad, loaded := 0, 0, 0
	refusals := map[string]int{}
	for sc.Scan() {
		row, err := pyjson.Loads(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		o := row.(*pyjson.Object)
		want, ok := o.Value("yaml").(string)
		if !ok {
			continue
		}
		n++
		got, why := SafeDump(fromTyped(o.Value("value")))
		if why != nil || got != want {
			bad++
			if bad <= 10 {
				t.Errorf("value %s\n got %q (%v)\nwant %q", pyjson.Dumps(o.Value("value"), true), got, why, want)
			}
		}
		back, why := LoadDumped(want)
		if why != nil {
			refusals[why.Why]++
			if os.Getenv("DAISUGI_PYYAML_SHOW") != "" && refusals[why.Why] <= 2 {
				t.Logf("refused (%s): %q", why.Why, want)
			}
			continue
		}
		loaded++
		if again, _ := SafeDump(back); again != want {
			t.Errorf("LoadDumped(%q) dumps again as %q", want, again)
		}
	}
	t.Logf("%d dumps, %d disagreements; LoadDumped read %d back, refused %v", n, bad, loaded, refusals)
}
