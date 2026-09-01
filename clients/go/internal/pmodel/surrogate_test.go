package pmodel

import "testing"

// jiter's errors for surrogate escapes, as pydantic_core.from_json gives
// them (probed on pydantic-core 2.13.5's jiter).
func TestSurrogateEscapesFailAsJiterDoes(t *testing.T) {
	const b = `\`
	for text, want := range map[string]string{
		`"` + b + `ud800"`:                      "unexpected end of hex escape at line 1 column 8",
		`"` + b + `ud800x"`:                     "unexpected end of hex escape at line 1 column 8",
		`"` + b + `ud800` + b + `n"`:            "unexpected end of hex escape at line 1 column 9",
		`"` + b + `ud800` + b + `u0041"`:        "lone leading surrogate in hex escape at line 1 column 13",
		`"` + b + `ud800` + b + `ud800"`:        "lone leading surrogate in hex escape at line 1 column 13",
		`"` + b + `ud800` + b + `ue000"`:        "lone leading surrogate in hex escape at line 1 column 13",
		`"` + b + `ud800` + b + `uZZZZ"`:        "invalid escape at line 1 column 10",
		`"` + b + `ud800` + b + `u"`:            "EOF while parsing a string at line 1 column 10",
		`"` + b + `ud800` + b + `"`:             "unexpected end of hex escape at line 1 column 9",
		`"` + b + `ud800` + b + `u12"`:          "EOF while parsing a string at line 1 column 12",
		`"` + b + `udfff"`:                      "lone leading surrogate in hex escape at line 1 column 7",
		`"` + b + `udc00` + b + `ud800"`:        "lone leading surrogate in hex escape at line 1 column 7",
		`"` + b + `ud800`:                       "EOF while parsing a string at line 1 column 7",
		`"` + b + `ud800` + b + `u00`:           "EOF while parsing a string at line 1 column 11",
		`{"a": "` + b + `ud800` + b + `u0041"}`: "lone leading surrogate in hex escape at line 1 column 19",
		`"` + b + `uZZ"`:                        "EOF while parsing a string at line 1 column 6",
		`"` + b + `uZZZZ`:                       "invalid escape at line 1 column 4",
		`"` + b + `u12G4"`:                      "invalid escape at line 1 column 6",
	} {
		_, err := ParseJSON(text)
		if err == nil {
			t.Errorf("%s: parsed", text)
			continue
		}
		if got := err.Msg + " at " + Position(text, err.At); got != want {
			t.Errorf("%s: got %q, want %q", text, got, want)
		}
	}
	if v, err := ParseJSON(`"` + b + `ud800` + b + `udc00"`); err != nil || v != "\U00010000" {
		t.Errorf("a pair: %v %v", v, err)
	}
}
