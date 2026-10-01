package verify

import (
	"encoding/json"
	"testing"
)

// pyEqual compares ints exactly, and an int with a float exactly, as
// Python's == does; numbers come in as a step decodes them.
func TestPyEqualExactNumbers(t *testing.T) {
	num := func(s string) interface{} { return exactNumbers(json.Number(s)) }
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"9007199254740993", "9007199254740992", false},
		{"-9007199254740993", "-9007199254740992", false},
		{"18446744073709551621", "18446744073709551616", false},
		{"9007199254740993", "9007199254740992.0", false},
		{"9007199254740992", "9007199254740992.0", true},
		{"18446744073709551616", "18446744073709551616.0", true},
		{"18446744073709551621", "1.8446744073709552e19", false},
		{"1", "1.0", true},
		{"3.5", "3", false},
	} {
		if got := pyEqual(num(c.a), num(c.b)); got != c.want {
			t.Errorf("%s == %s: %v", c.a, c.b, got)
		}
	}
	if !pyEqual(true, num("1")) || pyEqual(true, num("2")) {
		t.Error("bool is not the int 0 or 1")
	}
}

// A Z3 numeral is Python's: an int exactly (2^60 is not its float's
// shortest digits), a float as str(f) gives it.
func TestSMTNumeralsArePythons(t *testing.T) {
	for in, want := range map[string]string{
		"1152921504606846976":  "1152921504606846976.0",
		"9007199254740993":     "9007199254740993.0",
		"-9223372036854775808": "(- 9223372036854775808.0)",
		"9007199254740992":     "9007199254740992.0",
		"1e30":                 "1000000000000000000000000000000.0",
		"0.1":                  "0.1",
	} {
		got, err := smtNumLit(exactNumbers(json.Number(in)))
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
}
