package pyjson

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

type fixture struct {
	Loads []struct {
		Text  string `json:"text"`
		OK    bool   `json:"ok"`
		ASCII string `json:"ascii"`
		UTF8  string `json:"utf8"`
	} `json:"loads"`
	Floats []struct {
		Bits string `json:"bits"`
		Repr string `json:"repr"`
	} `json:"floats"`
	Rounds []struct {
		Bits string `json:"bits"`
		N    int    `json:"n"`
		Repr string `json:"repr"`
	} `json:"rounds"`
	Reprs []struct {
		S    string `json:"s"`
		Repr string `json:"repr"`
	} `json:"reprs"`
	ReprList struct {
		In   []string `json:"in"`
		Repr string   `json:"repr"`
	} `json:"repr_list"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/pyjson.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadsDumpsMatchPython(t *testing.T) {
	for _, c := range loadFixture(t).Loads {
		v, err := Loads(c.Text)
		if (err == nil) != c.OK {
			t.Errorf("Loads(%q) err=%v, python ok=%v", c.Text, err, c.OK)
			continue
		}
		if !c.OK {
			continue
		}
		if got := Dumps(v, true); got != c.ASCII {
			t.Errorf("Dumps ascii %q:\n got %s\nwant %s", c.Text, got, c.ASCII)
		}
		if got := Dumps(v, false); got != c.UTF8 {
			t.Errorf("Dumps utf8 %q:\n got %s\nwant %s", c.Text, got, c.UTF8)
		}
	}
}

func TestFloatReprMatchesPython(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.Floats {
		x, err := strconv.ParseFloat(c.Bits, 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := FloatRepr(x); got != c.Repr {
			t.Errorf("FloatRepr(%s) = %s, want %s", c.Bits, got, c.Repr)
		}
	}
	for _, c := range f.Rounds {
		x, _ := strconv.ParseFloat(c.Bits, 64)
		if got := FloatRepr(Round(x, c.N)); got != c.Repr {
			t.Errorf("round(%s, %d) = %s, want %s", c.Bits, c.N, got, c.Repr)
		}
	}
}

func TestReprMatchesPython(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.Reprs {
		if got := Repr(c.S); got != c.Repr {
			t.Errorf("Repr(%q) = %s, want %s", c.S, got, c.Repr)
		}
	}
	if got := ReprList(f.ReprList.In); got != f.ReprList.Repr {
		t.Errorf("ReprList = %s, want %s", got, f.ReprList.Repr)
	}
}

func TestLoadsStrictRefusesWhatPydanticReadsDifferently(t *testing.T) {
	for _, text := range []string{`{"a": 1, "a": 2}`, `{"a": NaN}`, `[Infinity]`, `[-Infinity]`} {
		if _, err := LoadsStrict(text); err != ErrUnsupported {
			t.Errorf("LoadsStrict(%s) = %v, want ErrUnsupported", text, err)
		}
	}
	if _, err := LoadsStrict(`{"a": [1, -2.5e3, "x"]}`); err != nil {
		t.Errorf("LoadsStrict of plain JSON: %v", err)
	}
}

// A lone surrogate is a valid Python str; it is kept in its generalized
// UTF-8 form and written back as the same escape.
func TestLoneSurrogateIsKept(t *testing.T) {
	v, err := Loads(`"a\ud800b"`)
	if err != nil || v != "a\xed\xa0\x80b" {
		t.Fatalf("got %q, %v", v, err)
	}
	if got := Dumps(v, true); got != `"a\ud800b"` {
		t.Fatalf("Dumps = %s", got)
	}
}

// json.loads raises past 4,300 digits and past its nesting limit.
func TestLimits(t *testing.T) {
	if _, err := Loads(strings.Repeat("9", 4300)); err != nil {
		t.Errorf("4300 digits: %v", err)
	}
	if _, err := Loads(strings.Repeat("9", 4301)); err == nil {
		t.Error("4301 digits parsed")
	}
	if _, err := Loads(strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)); err != nil {
		t.Errorf("depth %d: %v", MaxDepth, err)
	}
	if _, err := Loads(strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1)); err == nil {
		t.Errorf("depth %d parsed", MaxDepth+1)
	}
}

func TestTruthy(t *testing.T) {
	v, _ := Loads(`[0, -0, 0.0, "", [], {}, null, false, 1, "x", [0], {"a": 0}, true]`)
	want := []bool{false, false, false, false, false, false, false, false, true, true, true, true, true}
	for i, e := range v.([]any) {
		if Truthy(e) != want[i] {
			t.Errorf("Truthy(#%d %v) = %v", i, e, !want[i])
		}
	}
}
