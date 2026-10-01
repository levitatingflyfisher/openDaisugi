package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortHop(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "daisugi")
	for _, n := range []string{"daisugi", "daisugi-rs"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		env    map[string]string
		target string
		err    string
	}{
		{map[string]string{}, "", ""},
		{map[string]string{"DAISUGI_PORT": "go"}, "", ""},
		{map[string]string{"DAISUGI_PORT": "rust"}, filepath.Join(dir, "daisugi-rs"), ""},
		{map[string]string{"DAISUGI_PORT": "python"}, "",
			"DAISUGI_PORT is python, but there is no daisugi-py beside " + self + ". Install it, or unset DAISUGI_PORT. Nothing was changed."},
		{map[string]string{"DAISUGI_PORT": "zig"}, "", "DAISUGI_PORT must be go, rust or python, not zig. Nothing was changed."},
		{map[string]string{"DAISUGI_PORT": "rust", "DAISUGI_PORT_HOP": "1"}, "",
			"DAISUGI_PORT is rust, but the daisugi it ran is the go port. Nothing was changed."},
		// The port the variable names is this one: a hand-over ends here.
		{map[string]string{"DAISUGI_PORT": "go", "DAISUGI_PORT_HOP": "1"}, "", ""},
	}
	for _, c := range cases {
		got, err := PortHop("go", self, c.env)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if got != c.target || msg != c.err {
			t.Errorf("%v: got (%q, %q), want (%q, %q)", c.env, got, msg, c.target, c.err)
		}
	}
	// A directory or a file that is not executable is not a port.
	if err := os.WriteFile(filepath.Join(dir, "daisugi-py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PortHop("go", self, map[string]string{"DAISUGI_PORT": "python"}); err == nil || !strings.Contains(err.Error(), "no daisugi-py beside") {
		t.Errorf("non-executable sibling: %v", err)
	}
}
