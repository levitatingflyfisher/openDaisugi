package gate

import (
	"reflect"
	"testing"
)

// One heredoc around a patch is unwrapped only when its closing word
// matches its opening word, as the oracle's backreference requires.
func TestApplyPatchUnwrapsOneHeredoc(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: src/a.txt\n*** End Patch"
	for text, want := range map[string][]string{
		"<<'EOF'\n" + patch + "\nEOF":     {"src/a.txt"},
		"cat <<EOF\n" + patch + "\nEOF  ": {"src/a.txt"},
		"<<EOF\n" + patch + "\nEND":       {"src/a.txt"},
		patch:                             {"src/a.txt"},
	} {
		got, ok := parseApplyPatch(text)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v %v", text, got, ok)
		}
	}
	if _, ok := parseApplyPatch("<<EOF\n*** Begin Patch\nEOF"); ok {
		t.Error("a patch with no End Patch was read")
	}
}
