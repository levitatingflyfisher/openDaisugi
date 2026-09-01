package pyjson

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"testing"

	"daisugi-verify/internal/pystr"
)

func TestLoadsPyErrors(t *testing.T) {
	for text, want := range map[string]string{
		"":                  "Expecting value: line 1 column 1 (char 0)",
		`{"a" 1}`:           "Expecting ':' delimiter: line 1 column 6 (char 5)",
		`{"a": 1,}`:         "Expecting property name enclosed in double quotes: line 1 column 9 (char 8)",
		`{"a": 1`:           "Expecting ',' delimiter: line 1 column 8 (char 7)",
		`{"a": "\x"}`:       `Invalid \escape: line 1 column 8 (char 7)`,
		"{\"a\": \"b\nc\"}": "Invalid control character at: line 1 column 9 (char 8)",
		`[1] x`:             "Extra data: line 1 column 5 (char 4)",
		"\ufeff{}":          "Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)",
	} {
		_, err := LoadsPy(text, 0)
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
}

func pyDump(v any) any {
	switch x := v.(type) {
	case nil, bool:
		return x
	case string:
		// encoding/json reads a lone surrogate escape as U+FFFD.
		rs := pystr.Runes(x)
		for i, r := range rs {
			if pystr.IsSurrogate(r) {
				rs[i] = 0xFFFD
			}
		}
		return string(rs)
	case Int:
		return map[string]any{"int": x.Text}
	case Float:
		f := float64(x)
		if math.IsNaN(f) {
			return map[string]any{"float": "nan"}
		}
		s := FloatRepr(f)
		switch s {
		case "Infinity":
			s = "inf"
		case "-Infinity":
			s = "-inf"
		}
		return map[string]any{"float": s}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = pyDump(e)
		}
		return out
	case *Object:
		pairs := []any{}
		for _, k := range x.Keys() {
			pairs = append(pairs, []any{k, pyDump(x.Value(k))})
		}
		return map[string]any{"dict": pairs}
	}
	return "?"
}

// TestLoadsPyAgainstOracle reads testdata/decode_oracle.py's output from
// DAISUGI_PYJSON_DECODE_ORACLE.
func TestLoadsPyAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_PYJSON_DECODE_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_PYJSON_DECODE_ORACLE is not set")
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
		row, err := Loads(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		o := row.(*Object)
		text := o.Value("t").(string)
		var want any
		if e, ok := o.Get("err"); ok {
			want = "err: " + e.(string)
		} else {
			var raw map[string]any
			_ = json.Unmarshal(sc.Bytes(), &raw)
			want = raw["ok"]
		}
		v, derr := LoadsPy(text, 0)
		var got any
		if derr != nil {
			got = "err: " + derr.Error()
		} else {
			b, _ := json.Marshal(pyDump(v))
			_ = json.Unmarshal(b, &got)
		}
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		n++
		if string(gj) != string(wj) {
			bad++
			if bad < 20 {
				t.Errorf("%q:\n got %s\nwant %s", text, gj, wj)
			}
		}
	}
	t.Logf("%d texts, %d differ", n, bad)
}
