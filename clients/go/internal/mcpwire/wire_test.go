package mcpwire

import (
	"math"
	"strings"
	"testing"

	"daisugi-verify/internal/pyjson"
)

// Lines end at \n, \r\n and a lone \r; bad UTF-8 is replaced run by run;
// a last line with no end is kept.
func TestLinesAreUniversalNewlines(t *testing.T) {
	r := NewLineReader(strings.NewReader("a\nb\r\nc\rd\xff\xfe\ne"))
	var got []string
	for {
		l, ok := r.Next()
		if !ok {
			break
		}
		got = append(got, l)
	}
	want := []string{"a\n", "b\n", "c\n", "d\ufffd\ufffd\n", "e"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
}

// Only a JSON integer or a string is an id; anything else makes the
// message a notification, which gets no reply.
func TestClassifyIDs(t *testing.T) {
	for line, kind := range map[string]Kind{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`:         Request,
		`{"jsonrpc":"2.0","id":"x","method":"ping"}`:       Request,
		`{"jsonrpc":"2.0","id":1.0,"method":"ping"}`:       Notification,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`:      Notification,
		`{"jsonrpc":"2.0","method":"ping"}`:                Notification,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`:         Invalid,
		`{"jsonrpc":"2.0","id":1,"result":{}}`:             Invalid,
		`{"jsonrpc":"2.0","id":1,"method":"x","params":1}`: Invalid,
		`[1]`: Invalid,
	} {
		if got := Classify(line).Kind; got != kind {
			t.Errorf("%s: %v, want %v", line, got, kind)
		}
	}
}

// pydantic's float text, and NaN as null (a line) or NaN (a text block).
func TestFloatsAsPydanticWritesThem(t *testing.T) {
	for f, want := range map[float64]string{1e16: "1e+16", 1e-5: "0.00001", 1.5e-7: "1.5e-7", 100: "100.0",
		1e-6: "1e-6", 0.1: "0.1"} {
		if got := Compact(f); got != want {
			t.Errorf("%v: %s, want %s", f, got, want)
		}
	}
	o := pyjson.NewObject().Set("a", math.NaN()).Set("b", "é\x00\u2028")
	if got := Compact(o); got != `{"a":null,"b":"é\u0000`+"\u2028"+`"}` {
		t.Error(got)
	}
	if got := Indent(o); got != "{\n  \"a\": NaN,\n  \"b\": \"é\\u0000\u2028\"\n}" {
		t.Error(got)
	}
}
