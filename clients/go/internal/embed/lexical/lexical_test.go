package lexical

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
)

// units.json is written by clients/pathway_cases.py from the oracle.
const unitsPath = "../../../../fixtures/pathways/units.json"

type units struct {
	Blake2b [][2]string `json:"blake2b"`
	Lexical []struct {
		Text   string       `json:"text"`
		Tokens []string     `json:"tokens"`
		NZ     [][2]float64 `json:"nz"`
	} `json:"lexical"`
}

func load(t *testing.T) units {
	t.Helper()
	raw, err := os.ReadFile(unitsPath)
	if err != nil {
		t.Fatal(err)
	}
	var u units
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBlake2bMatchesHashlib(t *testing.T) {
	u := load(t)
	if len(u.Blake2b) < 50 {
		t.Fatalf("only %d digests in the fixture", len(u.Blake2b))
	}
	for _, p := range u.Blake2b {
		if got := hex.EncodeToString(blake2b([]byte(p[0]), 8)); got != p[1] {
			t.Errorf("blake2b(%q, 8) = %s, hashlib says %s", p[0], got, p[1])
		}
	}
}

// RFC 7693 appendix A: BLAKE2b-512("abc").
func TestBlake2bRFCVector(t *testing.T) {
	want := "ba80a53f981c4d0d6a2797b69f12f6e94c212f14685ac4b74b12bb6fdbffa2d1" +
		"7d87c5392aab792dc252d5de4533cc9518d38aa8dbf1925ab92386edd4009923"
	if got := hex.EncodeToString(blake2b([]byte("abc"), 64)); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestTokensAndVectorsMatchOracle(t *testing.T) {
	u := load(t)
	for _, c := range u.Lexical {
		toks := Tokens(c.Text)
		if len(toks) == 0 && len(c.Tokens) == 0 {
			toks = c.Tokens
		}
		if !reflect.DeepEqual(toks, c.Tokens) {
			t.Errorf("Tokens(%q) = %q, oracle %q", c.Text, toks, c.Tokens)
			continue
		}
		v := Encode(c.Text)
		want := make([]float64, Dim)
		for _, nz := range c.NZ {
			want[int(nz[0])] = nz[1]
		}
		for i := range v {
			if math.Abs(v[i]-want[i]) > 1e-12 {
				t.Errorf("Encode(%q)[%d] = %v, oracle %v", c.Text, i, v[i], want[i])
				break
			}
		}
	}
}
