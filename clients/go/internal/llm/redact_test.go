package llm

import (
	"strings"
	"testing"
)

const redactKey = "sk-ABCD" + "MIDDLEPART1234" + "WXYZ"

// A key that straddles the 500-character cut of a failed reply's body is
// cleaned before the cut, not after: the cut used to leave a prefix too
// short for the key pattern.
func TestReadReplyCleansBeforeTheCut(t *testing.T) {
	w, _ := ResolveWire("ollama/m", "", "", env(nil))
	_, e := ReadReply(w, 500, strings.Repeat("x", 492)+redactKey+"tail")
	if e == nil || strings.Contains(e.Error(), "sk-ABCDM") || strings.Contains(e.Error(), "MIDDLE") {
		t.Fatalf("%v", e)
	}
	_, e = ReadReply(w, 200, strings.Repeat("y", 192)+redactKey)
	if e == nil || strings.Contains(e.Error(), "sk-ABCDM") || strings.Contains(e.Error(), "MIDDLE") {
		t.Fatalf("%v", e)
	}
}

func TestCleanHidesBearerAndKeyParameters(t *testing.T) {
	got := Clean("Authorization: Bearer abcdef0123456789tokenvalue and " +
		"http://h/v1/models?api_key=SECRETVALUE123&x=1 and ?key=OTHERSECRET99 and token=zzzzzzzz")
	for _, leak := range []string{"abcdef0123456789tokenvalue", "SECRETVALUE123", "OTHERSECRET99", "zzzzzzzz"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q leaked in %q", leak, got)
		}
	}
	if !strings.Contains(got, "Bearer ...") || !strings.Contains(got, "x=1") {
		t.Fatal(got)
	}
}
