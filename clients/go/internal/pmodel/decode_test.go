package pmodel

import (
	"bufio"
	"os"
	"testing"

	"daisugi-verify/internal/pyjson"
)

// typedEqual compares a decoded value to clients/literal_cases.py's typed
// dump. A tuple there is a list here; sawTuple records one.
func typedEqual(got, want any, sawTuple *bool) bool {
	switch w := want.(type) {
	case nil, bool:
		return got == w
	case *pyjson.Object:
		keys := w.Keys()
		if len(keys) != 1 {
			return false
		}
		inner := w.Value(keys[0])
		switch keys[0] {
		case "int":
			g, ok := got.(pyjson.Int)
			return ok && g.Text == inner
		case "float":
			g, ok := got.(pyjson.Float)
			r := pyjson.FloatRepr(float64(g))
			r = map[string]string{"Infinity": "inf", "-Infinity": "-inf", "NaN": "nan"}[r] + r
			if len(r) > 3 && (r[:3] == "inf" || r[:4] == "-inf" || r[:3] == "nan") {
				r = r[:len(r)-len(pyjson.FloatRepr(float64(g)))]
			}
			return ok && r == inner
		case "str":
			return got == inner
		case "list", "tuple":
			if keys[0] == "tuple" {
				*sawTuple = true
			}
			g, ok := got.([]any)
			ws := inner.([]any)
			if !ok || len(g) != len(ws) {
				return false
			}
			for i := range ws {
				if !typedEqual(g[i], ws[i], sawTuple) {
					return false
				}
			}
			return true
		case "dict":
			g, ok := got.(*pyjson.Object)
			ws := inner.([]any)
			if !ok || len(g.Keys()) != len(ws) {
				return false
			}
			for i, kv := range ws {
				pair := kv.([]any)
				if g.Keys()[i] != pair[0] || !typedEqual(g.Value(pair[0].(string)), pair[1], sawTuple) {
					return false
				}
			}
			return true
		}
	}
	return false
}

// TestDecodeDictTextAgainstPython reads clients/fixtures/literal
// (clients/literal_cases.py): decode_dict_text's answer for thousands of
// texts. The port gives the same dict or None, and refuses where Python
// prints a SyntaxWarning or returns what the reader does not model.
func TestDecodeDictTextAgainstPython(t *testing.T) {
	f, err := os.Open("../../../fixtures/literal/cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n, over := 0, 0
	for sc.Scan() {
		v, err := pyjson.Loads(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		c := v.(*pyjson.Object)
		text := c.Value("text").(string)
		warn := c.Value("warn").(bool)
		want := c.Value("value").(*pyjson.Object)
		got := DecodeDictText(text)
		n++
		switch {
		case warn:
			if got.Refuse == "" || got.Dict != nil {
				t.Errorf("%q: Python warns; got %v %q", text, got.Dict, got.Refuse)
			}
		case want.Keys()[0] == "unmodeled":
			if got.Refuse == "" {
				t.Errorf("%q: Python returns %v, not modelled; got no refusal", text, want.Value("unmodeled"))
			}
		case want.Keys()[0] == "none":
			if got.Refuse == "" && got.Dict != nil {
				t.Errorf("%q: Python returns None; got %v", text, got.Dict)
			} else if got.Refuse != "" {
				// A refusal where Python gives None and prints nothing:
				// allowed, but counted.
				over++
				t.Logf("%q: refused (%s) where Python returns None", text, got.Refuse)
			}
		default:
			saw := false
			if got.Refuse != "" || got.Dict == nil || !typedEqual(got.Dict, want, &saw) || saw != got.Tuple {
				t.Errorf("%q: want %s; got %v %q tuple=%v", text, pyjson.Dumps(want, true), got.Dict, got.Refuse, got.Tuple)
			}
		}
	}
	if n < 3000 {
		t.Fatalf("only %d cases", n)
	}
	if over >= 100 {
		t.Errorf("%d refusals where Python returns None", over)
	}
}
