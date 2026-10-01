package cli

import (
	"strings"
	"testing"
)

func TestUserWarningShown(t *testing.T) {
	for _, c := range []struct {
		pw       string
		show, ok bool
	}{
		{"", true, true},
		{"ignore::UserWarning", false, true},
		{"ignore", false, true},
		{"i::DeprecationWarning", true, true},
		{"ignore::UserWarning,default::UserWarning", true, true},
		{"ignore:3 PATH", false, true},
		{"ignore:4", true, true},
		{"default", false, false},
		{"always::DeprecationWarning", false, false},
		{"error", false, false},
		{"bogus", false, false},
		{"ignore::NoSuchWarning", false, false},
		{"ignore:::m", false, false},
	} {
		show, ok := userWarningShown(c.pw, "3 pathway(s)")
		if show != c.show || ok != c.ok {
			t.Errorf("%q: %v %v", c.pw, show, ok)
		}
	}
}

// The MCP server prints the stale-embeddings warning once per server, and
// nothing under a filter that ignores it or one this binary does not model.
func TestMCPStaleWarnOnce(t *testing.T) {
	for _, c := range []struct{ pw, want string }{
		{"", "UserWarning: a\n"},
		{"ignore", ""},
		{"error", ""},
	} {
		var b strings.Builder
		e := &Env{Stderr: &b, env: map[string]string{}}
		if c.pw != "" {
			e.env["PYTHONWARNINGS"] = c.pw
		}
		s := &mcpServer{e: e}
		s.staleWarn("")
		s.staleWarn("a")
		s.staleWarn("b")
		if b.String() != c.want {
			t.Fatalf("%q: %q", c.pw, b.String())
		}
	}
}

// staleOnce prints the warning, or refuses a filter not modelled.
func TestStaleOnce(t *testing.T) {
	var b strings.Builder
	e := &Env{Stderr: &b, env: map[string]string{}}
	if err := e.staleOnce("w"); err != nil || b.String() != "UserWarning: w\n" {
		t.Fatalf("%v %q", err, b.String())
	}
	e.env["PYTHONWARNINGS"] = "error"
	if err := e.staleOnce("w"); err == nil {
		t.Fatal("an error filter was not refused")
	}
}
