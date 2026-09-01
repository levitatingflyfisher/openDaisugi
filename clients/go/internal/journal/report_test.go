package journal

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Expected values are Python 3.12's str.splitlines and repr.
func TestSplitlines(t *testing.T) {
	got := splitlines("a\nb\r\nc\rd\x0be\x1cf\u2028g\n")
	want := []string{"a", "b", "c", "d", "e", "f", "g"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestReprStr(t *testing.T) {
	for in, want := range map[string]string{
		"it's \"q\"":          `'it\'s "q"'`,
		"it's":                `"it's"`,
		"\u00e9\u200b\u3000x": "'\u00e9" + `\u200b\u3000x'`,
		"\x00\x7f\u00a0":      `'\x00\x7f\xa0'`,
		"\U0001F600":          `'` + "\U0001F600" + `'`,
	} {
		if got := reprStr(in); got != want {
			t.Errorf("reprStr(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestReport(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "shadow")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	log := `{"tool_name": "Bash", "detail": "a && b", "would_deny": true, "reason": "shell command contains metacharacters"}
{"tool_name": "Read", "would_deny": false}

not json
`
	if err := os.WriteFile(filepath.Join(d, "s1.jsonl"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := Files(root, "", false)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Build(files)
	if err != nil {
		t.Fatal(err)
	}
	text, err := rep.Text()
	if err != nil {
		t.Fatal(err)
	}
	want := "calls=2 allowed=1 would_deny=1 false_positive_candidates=1\n" +
		"  DENY [FP-candidate] Bash 'a && b': shell command contains metacharacters\n"
	if text != want {
		t.Fatalf("%q", text)
	}
	if !strings.Contains(rep.JSON(), "\"reasons\": {\n    \"shell command contains metacharacters\": 1\n  }") {
		t.Fatal(rep.JSON())
	}
}
