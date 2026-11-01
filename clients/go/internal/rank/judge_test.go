package rank

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// The digest is the oracle's (tests/test_rank_judge.py): the three ports
// send one request body.
func TestPromptBytesArePinned(t *testing.T) {
	msgs := Prompt("t", "x", "y")
	var l []map[string]string
	for _, m := range msgs {
		l = append(l, map[string]string{"role": m.Role, "content": m.Content})
	}
	b, _ := json.Marshal(l) // map keys are sorted; no spaces
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != "f90271f4cb148385a65dcf40cf1c6b078777f6b8c0ed1bb4f76108d64328cc65" {
		t.Fatalf("prompt digest %s", got)
	}
}

func TestParseVerdict(t *testing.T) {
	for in, want := range map[string]string{"A": "a", " b.\nreason": "b", "Tie": "tie", "A or B": "", "": "", "\n\nB": "b"} {
		if got := ParseVerdict(in); got != want {
			t.Errorf("ParseVerdict(%q) = %q, want %q", in, got, want)
		}
	}
}
