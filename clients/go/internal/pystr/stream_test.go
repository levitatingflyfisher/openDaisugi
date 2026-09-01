package pystr

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDecodeStreamMatchesPython reads testdata/stream.json (gen_stream.py
// writes it from the oracle's Python): for each input, the lines Python
// hands out before its UnicodeDecodeError, and the error's text.
func TestDecodeStreamMatchesPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Parts       [][2]any `json:"parts"`
		LinesSHA256 string   `json:"lines_sha256"`
		LinesLen    int      `json:"lines_len"`
		Error       *string  `json:"error"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 300 {
		t.Fatalf("only %d cases", len(cases))
	}
	for i, c := range cases {
		var data []byte
		for _, p := range c.Parts {
			b, err := hex.DecodeString(p[0].(string))
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, bytes.Repeat(b, int(p[1].(float64)))...)
		}
		text, exc := DecodeStream(data)
		// Python hands the lines out with universal newlines.
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		sum := sha256.Sum256([]byte(text))
		if hex.EncodeToString(sum[:]) != c.LinesSHA256 {
			t.Errorf("case %d: %d bytes of lines, Python %d", i, len(text), c.LinesLen)
		}
		switch {
		case c.Error == nil && exc != nil:
			t.Errorf("case %d: raised %q, Python did not", i, exc.Msg)
		case c.Error != nil && exc == nil:
			t.Errorf("case %d: no error, Python raised %q", i, *c.Error)
		case c.Error != nil && exc.Msg != *c.Error:
			t.Errorf("case %d: %q, Python %q", i, exc.Msg, *c.Error)
		case exc != nil && exc.Type != "UnicodeDecodeError":
			t.Errorf("case %d: type %s", i, exc.Type)
		}
	}
}
