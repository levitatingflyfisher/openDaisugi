package sprig

import (
	"strings"
	"testing"
)

// sprig parses its flags with Go's flag package: one or two dashes, a
// value after "=" or in the next word, and the last value wins. The
// adapter reads and strips every such spelling, so the session it names
// is the one sprig uses.
func TestSessionFlagsReadEverySpellingSprigTakes(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		dir  string
		res  string
	}{
		{[]string{"-session-dir", "/d", "-resume", "r"}, "/d", "r"},
		{[]string{"-session-dir=/d", "--resume=r"}, "/d", "r"},
		{[]string{"--session-dir", "/mine", "-session-dir", "/victim", "-resume", "x", "--resume", "r"}, "/victim", "r"},
		{[]string{"--session-dir", "/d", "--", "-resume", "r"}, "/d", ""},
	} {
		if v, _ := flagValue(tc.argv, "session-dir"); v != tc.dir {
			t.Fatalf("%v: session-dir %q, want %q", tc.argv, v, tc.dir)
		}
		if v, _ := flagValue(tc.argv, "resume"); v != tc.res {
			t.Fatalf("%v: resume %q, want %q", tc.argv, v, tc.res)
		}
	}
	got := stripSessionFlags([]string{"-session-dir", "/v", "--gate", "-resume=r", "--session", "s",
		"-session-dir=/w", "--", "-resume", "x"})
	want := []string{"--gate", "--", "-resume", "x"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stripped %v, want %v", got, want)
	}
}
