package distill

import "testing"

func TestCapabilityHead(t *testing.T) {
	for _, c := range []struct {
		typ, in, want string
		ok            bool
	}{
		{"file_read", "/work/a.txt", "/work", true},
		{"file_read", "a.txt", "", false},
		{"file_read", "/a.txt", "/", true},
		{"file_read", "//x//y", "//x", true},
		{"network", "https://Example.com:8/x?y", "https://Example.com:8", true},
		{"network", "HTTPS://h", "https://h", true},
		{"network", "notaurl", "", false},
		{"network", "http://[::1", "", false},
		{"network", " \thttps://a\n.b/c", "https://a.b", true},
		{"shell", "ls", "", false},
	} {
		got, ok := capabilityHead(c.typ, c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%s %q: %q %v", c.typ, c.in, got, ok)
		}
	}
}

func TestNormalizeTask(t *testing.T) {
	in := "Base directory for this skill: /x\n  ### Skill: y\nPath: plugin:z\n<command-name>/b</command-name>do it  "
	if got := NormalizeTask(in); got != "do it" {
		t.Fatalf("%q", got)
	}
	if got := NormalizeTask("<command-args>x</command-args>"); got != "<command-args>x</command-args>" {
		t.Fatalf("%q", got)
	}
}

func TestSalvageWorkspaceIsTheFixedPrefixTheGlobsAdmit(t *testing.T) {
	for _, c := range []struct {
		globs []string
		want  string
		ok    bool
	}{
		{[]string{"/work/**"}, "/work", true},
		{[]string{"**"}, ".", true},
		{[]string{"./**"}, ".", true},
		{[]string{"/**"}, "/", true},
		{[]string{"/work/a/../b/**"}, "/work/b", true},
		{[]string{"/work/out.txt"}, "", false},
		{[]string{"/work/src/*.py"}, "", false},
		{[]string{"/work/src/*.py", "/work/**"}, "/work/src", true},
		{[]string{"/work/src/*.py", "/other/**"}, "/other", true},
		{nil, "", false},
	} {
		got, ok := SalvageWorkspace(c.globs)
		if got != c.want || ok != c.ok {
			t.Errorf("SalvageWorkspace(%q) = %q, %v; want %q, %v", c.globs, got, ok, c.want, c.ok)
		}
	}
}
