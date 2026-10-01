package smolvla

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// modelDir is the directory of the pinned graphs and assets, from
// DAISUGI_SMOLVLA_DIR. Tests that need it skip when it is not set.
func modelDir(t *testing.T) string {
	t.Helper()
	d := os.Getenv("DAISUGI_SMOLVLA_DIR")
	if d == "" {
		t.Skip("DAISUGI_SMOLVLA_DIR is not set (clients/vla_export.py --part assets fills it)")
	}
	return d
}

type tokenCases struct {
	Width int `json:"width"`
	Cases []struct {
		Text string  `json:"text"`
		IDs  []int64 `json:"ids"`
		Mask []int64 `json:"mask"`
	} `json:"cases"`
}

func TestTokenIDsMatchTheOracle(t *testing.T) {
	tok, err := LoadTokenizer(filepath.Join(modelDir(t), "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../../fixtures/vla/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var tc tokenCases
	if err := json.Unmarshal(b, &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Width != Width || len(tc.Cases) != 30 {
		t.Fatalf("fixture: width %d, %d cases", tc.Width, len(tc.Cases))
	}
	for _, c := range tc.Cases {
		ids, mask, err := tok.Task(c.Text)
		if err != nil {
			t.Fatalf("%q: %v", c.Text, err)
		}
		if !equal(ids[:], c.IDs) || !equal(mask[:], c.Mask) {
			t.Errorf("%q:\n got %v %v\nwant %v %v", c.Text, ids, mask, c.IDs, c.Mask)
		}
	}
}

func TestPreTokenizerSplits(t *testing.T) {
	cases := map[string][]string{
		"it's 12ab":     {"it", "'s", " 12", "ab"},
		"a   b":         {"a", "  ", " b"},
		"end  ":         {"end", "  "},
		"x\t\ny":        {"x", "\t", "\n", "y"},
		"!! ok ??":      {"!!", " ok", " ??"},
		"'re 'x":        {"'re", " '", "x"},
		"\u00e9t\u00e9": {"\u00e9t\u00e9"},
	}
	for in, want := range cases {
		got := preTokenize(in)
		if len(got) != len(want) {
			t.Errorf("%q: got %q, want %q", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: got %q, want %q", in, got, want)
				break
			}
		}
	}
}

func TestBadTokenizerFilesAreErrors(t *testing.T) {
	for _, s := range []string{
		``,
		`{}`,
		`{"model": {"type": "WordPiece", "vocab": {}, "merges": []}}`,
		`{"model": {"type": "BPE", "vocab": {"a": 0}, "merges": [["a"]]}}`,
	} {
		if _, err := ParseTokenizer([]byte(s)); err == nil {
			t.Errorf("%q: no error", s)
		}
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
