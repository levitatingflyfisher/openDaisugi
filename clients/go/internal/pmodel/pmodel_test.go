package pmodel

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"daisugi-verify/internal/pyjson"
)

// dump is testdata/oracle.py's dump, on validated values.
func dump(v any) any {
	switch x := v.(type) {
	case nil, bool, string:
		return x
	case pyjson.Int:
		return map[string]any{"int": x.Text}
	case float64:
		return map[string]any{"float": FloatRepr(x)}
	case pyjson.Float:
		return map[string]any{"float": FloatRepr(float64(x))}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = dump(e)
		}
		return out
	case *pyjson.Object:
		pairs := make([]any, 0, x.Len())
		for _, k := range x.Keys() {
			pairs = append(pairs, []any{k, dump(x.Value(k))})
		}
		return map[string]any{"dict": pairs}
	}
	return Repr(v)
}

func TestEnvelopeErrorsMatchPydantic(t *testing.T) {
	cases := map[string]string{
		`{"generated_by": "x", "permissions": {}}`: "1 validation error for Envelope\ntask\n  Field required [type=missing, input_value={'generated_by': 'x', 'permissions': {}}, input_type=dict]\n    For further information visit https://errors.pydantic.dev/2.13/v/missing",
		`{"generated_by": `:                        "1 validation error for Envelope\n  Invalid JSON: EOF while parsing a value at line 1 column 17 [type=json_invalid, input_value='{\"generated_by\": ', input_type=str]\n    For further information visit https://errors.pydantic.dev/2.13/v/json_invalid",
	}
	for text, want := range cases {
		_, err := ValidateJSON("Envelope", Envelope, text)
		if err == nil || err.String() != want {
			t.Errorf("%s:\n got %v\nwant %s", text, err, want)
		}
	}
}

// TestAgainstOracle reads testdata/oracle.py's output from
// DAISUGI_PMODEL_ORACLE.
func TestAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_PMODEL_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_PMODEL_ORACLE is not set")
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
			Kind  string
			Input json.RawMessage
			OK    json.RawMessage
			Err   *string
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		var got any
		var verr *ValidationError
		if row.Kind == "envelope" {
			var text string
			if err := json.Unmarshal(row.Input, &text); err != nil {
				t.Fatal(err)
			}
			got, verr = ValidateJSON("Envelope", Envelope, text)
			if o, ok := got.(*pyjson.Object); ok && !strings.Contains(text, `"id"`) {
				o2 := pyjson.NewObject()
				for _, k := range o.Keys() {
					if k != "id" {
						o2.Set(k, o.Value(k))
					}
				}
				got = o2
			}
		} else {
			in, err := pyjson.Loads(string(row.Input))
			if err != nil {
				t.Fatal(err)
			}
			got, verr = Validate(ExpressionTitle, Expression, in, Python)
		}
		fail := func(format string, args ...any) {
			bad++
			if bad <= 12 {
				t.Errorf("%s %s: "+format, append([]any{row.Kind, row.Input}, args...)...)
			}
		}
		if row.Err != nil {
			if verr == nil {
				fail("validated, want error %q", *row.Err)
			} else if verr.String() != *row.Err {
				fail("\n got %q\nwant %q", verr.String(), *row.Err)
			}
			continue
		}
		if verr != nil {
			fail("error %q, want ok", verr.String())
			continue
		}
		var want any
		_ = json.Unmarshal(row.OK, &want)
		gotJSON, _ := json.Marshal(dump(got))
		var gotV any
		_ = json.Unmarshal(gotJSON, &gotV)
		if !reflect.DeepEqual(gotV, want) {
			fail("\n got %s\nwant %s", gotJSON, row.OK)
		}
	}
	t.Logf("%d rows, %d differ", n, bad)
	if bad > 0 {
		t.Fail()
	}
}
