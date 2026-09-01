package pyre

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"

	"daisugi-verify/internal/pyjson"
)

func TestNamedEscapes(t *testing.T) {
	for name, want := range map[string]rune{
		"EM DASH": 0x2014, "em dash": 0x2014, "BYTE ORDER MARK": 0xFEFF, "BOM": 0xFEFF,
		"HANGUL SYLLABLE GA": 0xAC00, "HANGUL SYLLABLE GAG": 0xAC01, "CJK UNIFIED IDEOGRAPH-4E00": 0x4E00,
		"CJK UNIFIED IDEOGRAPH-9FFF": 0x9FFF,
	} {
		p, err := Parse(`\N{`+name+`}`, 0, 1)
		if err != nil || len(p.Data) != 1 || p.Data[0].Lit != int(want) {
			t.Errorf("%s: %v %v", name, p, err)
		}
	}
	for _, name := range []string{"hangul syllable ga", "cjk unified ideograph-9FFF", "CJK UNIFIED IDEOGRAPH-4e00", "CJK UNIFIED IDEOGRAPH-A000",
		"LATIN CAPITAL LETTER A WITH MACRON AND GRAVE", strings.Repeat("x", 300)} {
		if _, err := Parse(`\N{`+name+`}`, 0, 1); err == nil || !strings.Contains(err.Msg, "undefined character name") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestNamesAgainstOracle reads testdata/names_oracle.py's output from
// DAISUGI_PYRE_NAMES_ORACLE.
func TestNamesAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_PYRE_NAMES_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_PYRE_NAMES_ORACLE is not set")
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
		v, err := pyjson.Loads(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		row := v.(*pyjson.Object)
		name := row.Value("name").(string)
		var want string
		if e, ok := row.Get("err"); ok {
			want = e.(string)
		} else {
			want = fmt.Sprint(row.Value("cp").(pyjson.Int).Text)
		}
		var got string
		p, perr := Parse(`\N{`+name+`}`, 0, 1)
		switch {
		case perr == nil:
			got = fmt.Sprint(p.Data[0].Lit)
		case perr.Type == "error":
			got = "error: " + perr.String()
		default:
			got = perr.Type + ": " + perr.Msg
		}
		n++
		if got != want {
			bad++
			if bad < 20 {
				t.Errorf("%q:\n got %s\nwant %s", name, got, want)
			}
		}
	}
	t.Logf("%d names, %d differ", n, bad)
}
