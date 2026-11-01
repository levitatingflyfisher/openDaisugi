package warden

import (
	"strings"
	"testing"

	"daisugi-verify/internal/pyjson"
)

func TestPlaceArgPath(t *testing.T) {
	for _, c := range []struct {
		value, cwd, want string
		ok               bool
	}{
		{"/work/a", "/work", "/work/a", true},
		{"src/a.py", "/work", "", false},
		{"../etc/passwd", "/work/sub", "", false},
		{"a/b/../c", "/work", "", false},
		{"a/..", "/work", "", false},
		{"a..b/c", "/work", "", false},
		{"file:///etc/passwd", "/work", "", false},
		{"C:/x", "/work", "", false},
		{"c:x", "/work", "", false},
		{"a\\b", "/work", "", false},
		{"..\\..\\x", "/work", "", false},
		{" /a", "/work", "", false},
		{"/a\n", "/work", "", false},
		{"/a ", "/work", "", false},
		{"a\x00b", "/work", "", false},
		{"a:b/c"[2:], "/work", "", false},
		{"/work/../etc/passwd", "/work", "/etc/passwd", true},
		{"~/.ssh/id", "/work", "", false},
		{"$HOME/x", "/work", "", false},
		{"rel", "", "", false},
		{"rel", "relative-cwd", "", false},
		{"", "/work", "", false},
	} {
		got, ok := placeArgPath(c.value, c.cwd)
		if got != c.want || ok != c.ok {
			t.Errorf("placeArgPath(%q, %q) = %q, %v; want %q, %v", c.value, c.cwd, got, ok, c.want, c.ok)
		}
	}
}

func TestABoolIsNotANumber(t *testing.T) {
	one := pyjson.Int{Text: "1"}
	if kindEq(true, one) || kindEq(one, true) {
		t.Fatal("true equals 1")
	}
	if !kindEq(one, one) || !kindEq(true, true) {
		t.Fatal("equal values differ")
	}
	if kindIn(true, []any{one}) || !kindIn(one, []any{one}) {
		t.Fatal("kindIn")
	}
}

func TestMCPCaps(t *testing.T) {
	big := pyjson.NewObject().Set("a", strings.Repeat("x", maxMCPArgumentsChars))
	if got := checkMCPCaps(big); !strings.Contains(got, "longer than 262144 characters") {
		t.Fatalf("size cap: %q", got)
	}
	var deep any = "end"
	for i := 0; i < 65; i++ {
		deep = pyjson.NewObject().Set("a", deep)
	}
	if got := checkMCPCaps(deep); !strings.Contains(got, "deeper than 64 levels") {
		t.Fatalf("depth cap: %q", got)
	}
	var ok any = "end"
	for i := 0; i < 63; i++ {
		ok = pyjson.NewObject().Set("a", ok)
	}
	if got := checkMCPCaps(ok); got != "" {
		t.Fatalf("within caps: %q", got)
	}
}

func TestPathShapedStringsWalkInKeyOrderAndListIndex(t *testing.T) {
	args := pyjson.NewObject().
		Set("opts", pyjson.NewObject().Set("paths", []any{"/work/ok", "~/x", "rel/a"})).
		Set("path", "/etc/passwd")
	got := pathShapedStrings(args, "/home/u/", true)
	want := []pathArg{{"opts.paths[0]", "/work/ok"}, {"opts.paths[1]", "/home/u/x"}, {"path", "/etc/passwd"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(pathShapedStrings(pyjson.NewObject().Set("p", "~/x"), "", false)) != 0 {
		t.Fatal("a ~/ string with no HOME is a path")
	}
}

func TestAHostFlattenedToolIsNotAdmittedByAWildcardWhenARuleNamesIt(t *testing.T) {
	env := &envelope{McpAllowlist: []string{"opencode/*", "pi/*"}, McpRules: []mcpRule{{Tool: "github/get_issue"}, {Tool: "gh/*"}}}
	for _, c := range []struct{ key, want string }{
		{"opencode/github_get_issue", "github/get_issue"},
		{"pi/github_get_issue", "github/get_issue"},
		{"opencode/gh_anything", "gh/*"},
		{"opencode/slack_post", ""},
		{"github/get_issue", ""},
	} {
		if got := ruledUnderHostName(env, c.key); got != c.want {
			t.Errorf("%s: got %q, want %q", c.key, got, c.want)
		}
	}
	env.McpAllowlist = append(env.McpAllowlist, "opencode/github_get_issue")
	if got := ruledUnderHostName(env, "opencode/github_get_issue"); got != "" {
		t.Errorf("a literal allowlist entry names the tool itself: %q", got)
	}
}
