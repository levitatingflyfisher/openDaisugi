package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config.yaml that names a verifier client this box does not have gets
// one clear line, and the built-in verifier keeps deciding. A restored
// config from a Python install must not leave a friend guessing.
func TestClientProblemNamesWhatIsMissing(t *testing.T) {
	bin := t.TempDir()
	have := filepath.Join(bin, "daisugi-conform-go")
	if err := os.WriteFile(have, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		want string // a substring; "" means no problem
	}{
		{"python is the built-in verifier", map[string]string{"PATH": "/usr/bin:/bin"}, ""},
		{"empty is the default", map[string]string{"PATH": "/usr/bin:/bin"}, ""},
		{"a client on PATH", map[string]string{"PATH": bin}, ""},
		{"a client that is not installed", map[string]string{"PATH": "/usr/bin:/bin"}, "daisugi-conform-rust"},
		{"a name no client has", map[string]string{"PATH": "/usr/bin:/bin"}, "names no client"},
	} {
		name := map[string]string{
			"python is the built-in verifier": "python", "empty is the default": "",
			"a client on PATH": "go", "a client that is not installed": "rust", "a name no client has": "cobol",
		}[c.name]
		got := ClientProblem(name, c.env)
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: want no problem, got %q", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) || strings.Contains(got, "\n") {
			t.Errorf("%s: want one line with %q, got %q", c.name, c.want, got)
		}
		if !strings.Contains(got, "built-in verifier decides") {
			t.Errorf("%s: the line does not say what decides instead: %q", c.name, got)
		}
	}
}
