package verify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mcpEnv(t *testing.T, rules string) Envelope {
	t.Helper()
	env := DefaultEnvelope()
	if err := json.Unmarshal([]byte(rules), &env.Permissions.McpRules); err != nil {
		t.Fatal(err)
	}
	env.Permissions.FileRead = []string{"/work/**"}
	return env
}

func mcpPlan(args map[string]interface{}) ActionPlan {
	return ActionPlan{Steps: []Step{stepRaw("s0", "mcp", map[string]interface{}{"server": "github", "tool": "get_issue", "arguments": args})}}
}

const issueRule = `[{"tool":"github/get_issue","args":[{"op":"equals","path":"repo","value":"acme/app"},{"op":"numeric_range","path":"number","min":1,"max":9}]}]`

func TestMCPRuleAdmitsAndDenies(t *testing.T) {
	env := mcpEnv(t, issueRule)
	ok := mcpPlan(map[string]interface{}{"repo": "acme/app", "number": float64(3)})
	if v := CheckPermissions(ok, env, false); len(v) != 0 {
		t.Fatalf("want admitted, got %+v", v)
	}
	bad := mcpPlan(map[string]interface{}{"repo": "evil/app", "number": float64(3)})
	v := CheckPermissions(bad, env, false)
	if len(v) != 1 || v[0].Message != "Step 's0' MCP tool 'github/get_issue': argument rule does not hold: equals on 'repo'" {
		t.Fatalf("got %+v", v)
	}
	extra := mcpPlan(map[string]interface{}{"repo": "acme/app", "number": float64(3), "body": "x"})
	v = CheckPermissions(extra, env, false)
	if len(v) != 1 || !strings.Contains(v[0].Message, "argument 'body' is not named by its rule (known: number, repo)") {
		t.Fatalf("got %+v", v)
	}
}

func TestMCPBoolIsNotANumber(t *testing.T) {
	env := mcpEnv(t, issueRule)
	v := CheckPermissions(mcpPlan(map[string]interface{}{"repo": "acme/app", "number": true}), env, false)
	if len(v) != 1 || !strings.Contains(v[0].Message, "numeric_range on 'number'") {
		t.Fatalf("a bool passed numeric_range: %+v", v)
	}
	if !kindEq(float64(1), float64(1)) || kindEq(true, float64(1)) || kindEq(float64(1), true) {
		t.Fatal("kindEq")
	}
}

func TestMCPPathInTakesAbsolutePathsOnly(t *testing.T) {
	env := mcpEnv(t, `[{"tool":"github/get_issue","args":[{"op":"path_in","path":"file","scope":"file_read"}]}]`)
	abs := mcpPlan(map[string]interface{}{"file": "/work/sub/a.txt"})
	if v := CheckPermissionsAt(abs, env, false, "/work"); len(v) != 0 {
		t.Fatalf("want admitted, got %+v", v)
	}
	// Owner, 2026-11-03: a relative value is refused with one reason.
	in := mcpPlan(map[string]interface{}{"file": "sub/a.txt"})
	v := CheckPermissionsAt(in, env, false, "/work")
	if len(v) != 1 || !strings.Contains(v[0].Message, PathInAbsolute) {
		t.Fatalf("want the absolute-only refusal, got %+v", v)
	}
	neg := mcpEnv(t, `[{"tool":"github/get_issue","args":[{"op":"not","child":{"op":"path_in","path":"file","scope":"file_read"}}]}]`)
	if v := CheckPermissionsAt(in, neg, false, "/work"); len(v) != 1 || !strings.Contains(v[0].Message, PathInAbsolute) {
		t.Fatalf("a relative value under not must be refused, got %+v", v)
	}
	for _, file := range []string{"../etc/passwd", "file:///etc/passwd", "C:/x", "..\\x", " /work/a", "/work/a\n", "a\x00"} {
		out := mcpPlan(map[string]interface{}{"file": file})
		if v := CheckPermissionsAt(out, env, false, "/work"); len(v) != 1 {
			t.Fatalf("%q: want denied, got %+v", file, v)
		}
	}
	if v := CheckPermissionsAt(in, env, false, ""); len(v) != 1 {
		t.Fatalf("a relative path with no cwd cannot be placed: %+v", v)
	}
}

func TestMCPToolsDisjoint(t *testing.T) {
	if !toolsDisjoint("a/b", "a/c") || toolsDisjoint("a/b", "a/b") || toolsDisjoint("a/*", "a/c") {
		t.Fatal("toolsDisjoint")
	}
}

func TestMCPRuleRegexMatchesTheWholeString(t *testing.T) {
	env := mcpEnv(t, `[{"tool":"github/get_issue","args":[{"op":"matches","path":"repo","regex":"acme/.*"}]}]`)
	for repo, want := range map[string]int{"acme/app": 0, "evil/acme/app": 1, "acme/app\n": 1} {
		v := CheckPermissions(mcpPlan(map[string]interface{}{"repo": repo}), env, false)
		if len(v) != want {
			t.Errorf("%q: got %+v", repo, v)
		}
	}
}

func TestAnInvariantRegexStillSearches(t *testing.T) {
	ok, err := evalScalar(Matches{Path: "x", Regex: "b"}, map[string]interface{}{"x": "abc"}, nil)
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}

func TestAHostFlattenedToolIsNotAdmittedByAWildcardWhenARuleNamesIt(t *testing.T) {
	env := mcpEnv(t, issueRule)
	env.Permissions.McpAllowlist = []string{"opencode/*"}
	if got := RuledUnderHostName(env.Permissions, "opencode/github_get_issue"); got != "github/get_issue" {
		t.Fatalf("got %q", got)
	}
	if got := RuledUnderHostName(env.Permissions, "opencode/other"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Two checks at once, each with its own globs: a path_in reads its own
// call's context and never the other's. Run with -race.
func TestTwoPathInChecksAtOnceKeepTheirOwnContext(t *testing.T) {
	rule := `[{"tool":"github/get_issue","args":[{"op":"path_in","path":"file","scope":"file_read"}]}]`
	envA, envB := mcpEnv(t, rule), mcpEnv(t, rule)
	envA.Permissions.FileRead = []string{"/a/**"}
	envB.Permissions.FileRead = []string{"/b/**"}
	run := func(env Envelope, ok, bad string, done chan<- string) {
		for i := 0; i < 300; i++ {
			if v := CheckPermissionsAt(mcpPlan(map[string]interface{}{"file": ok}), env, false, "/"); len(v) != 0 {
				done <- "admitted path denied: " + v[0].Message
				return
			}
			if v := CheckPermissionsAt(mcpPlan(map[string]interface{}{"file": bad}), env, false, "/"); len(v) != 1 {
				done <- "foreign path admitted"
				return
			}
		}
		done <- ""
	}
	done := make(chan string, 3)
	go run(envA, "/a/x", "/b/x", done)
	go run(envB, "/b/x", "/a/x", done)
	// A third goroutine evaluates a path_in outside any rule, as an invariant
	// does. It has no context, and must read none of the others'.
	go func() {
		for i := 0; i < 300; i++ {
			if ok, _ := evalScalar(PathIn{Path: "file", Scope: "file_read"}, map[string]interface{}{"file": "/a/x"}, nil); ok {
				done <- "an invariant path_in read a rule's context"
				return
			}
		}
		done <- ""
	}()
	for i := 0; i < 3; i++ {
		if msg := <-done; msg != "" {
			t.Fatal(msg)
		}
	}
	// With no rule around it, a path_in in an invariant reads no context.
	ok, err := evalScalar(PathIn{Path: "file", Scope: "file_read"}, map[string]interface{}{"file": "/a/x"}, nil)
	if err != nil || ok {
		t.Fatalf("a path_in with no context: %v, %v", ok, err)
	}
}

func ruleMessages(t *testing.T, regex string, value string) []string {
	t.Helper()
	rules, _ := json.Marshal([]map[string]interface{}{{"tool": "github/get_issue",
		"args": []map[string]interface{}{{"op": "matches", "path": "repo", "regex": regex}}}})
	env := mcpEnv(t, string(rules))
	var msgs []string
	for _, v := range CheckPermissions(mcpPlan(map[string]interface{}{"repo": value}), env, false) {
		msgs = append(msgs, v.Message)
	}
	return msgs
}

// A pattern outside the subset denies with one reason, the same for a stray
// ")", a lookahead, a backreference, a repeat over 1000, a flag group and a
// \p name.
func TestAPatternOutsideTheSubsetDeniesWithOneReason(t *testing.T) {
	for _, rx := range []string{"x)|(?:.*", "^a)|(b$", "^(a$", "^(?=a)a$", "^(?!a).*$", "^(?<=a)b$", `^(a)\1$`, "^a{1001}$", "(?x)a|.*", "[!--/]", `\p{Ascii}`, `\p{Letter}`} {
		got := ruleMessages(t, rx, "aEVIL")
		want := "Step 's0' MCP tool 'github/get_issue': argument rule refused: regex '" + rx + "' is outside the supported subset"
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %v", rx, got)
		}
	}
}

func TestBacktrackingShapesAnswerAtOnceOn4096Characters(t *testing.T) {
	start := time.Now()
	for _, rx := range []string{"^(a|aa)+$", "^(a+)+$", "^(.*)*$", "^a*a*a*a*b$"} {
		for _, val := range []string{strings.Repeat("a", 4096), strings.Repeat("a", 4095) + "!"} {
			ruleMessages(t, rx, val)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestALongStringDenies(t *testing.T) {
	if got := ruleMessages(t, "^[a-z]+$", strings.Repeat("a", 4096)); len(got) != 0 {
		t.Fatalf("4096 is the cap: %v", got)
	}
	got := ruleMessages(t, "^[a-z]+$", strings.Repeat("a", 4097))
	want := "Step 's0' MCP tool 'github/get_issue': argument rule refused: argument 'repo' is longer than 4096 characters"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v", got)
	}
}

func TestAWildcardRuleAlsoAppliesTheRuleTheFlatNameFinds(t *testing.T) {
	rules := `[{"tool":"github/get_issue","args":[{"op":"in_set","path":"repo","values":["acme/app"]}]},
	{"tool":"opencode/*","args":[{"op":"not_matches","path":"repo","regex":"^secret$"}]}]`
	env := mcpEnv(t, rules)
	env.Permissions.McpAllowlist = []string{"opencode/*"}
	step := func(repo string) ActionPlan {
		return ActionPlan{Steps: []Step{stepRaw("s0", "mcp", map[string]interface{}{"server": "opencode", "tool": "github_get_issue", "arguments": map[string]interface{}{"repo": repo}})}}
	}
	if v := CheckPermissions(step("evil"), env, false); len(v) != 1 || !strings.Contains(v[0].Message, "in_set on 'repo'") {
		t.Fatalf("got %+v", v)
	}
	if v := CheckPermissions(step("acme/app"), env, false); len(v) != 0 {
		t.Fatalf("got %+v", v)
	}
}

func TestFlatNamesCompareLooselyOnBothSides(t *testing.T) {
	if FlatName("GitHub-get_issue") != "github_get_issue" || FlatName("é") != "_" {
		t.Fatal(FlatName("GitHub-get_issue"))
	}
	if FlatPattern("Cased/Tool") != "cased_tool" || FlatPattern("my-srv/[a-z]*") != "my_srv_[a-z]*" {
		t.Fatal(FlatPattern("my-srv/[a-z]*"))
	}
	env := mcpEnv(t, `[{"tool":"github/get_issue","args":[]}]`)
	env.Permissions.McpAllowlist = []string{"opencode/*", "pi/*"}
	for _, key := range []string{"pi/GitHub_get_issue", "pi/github-get_issue", "opencode/GITHUB_get_issue"} {
		if got := RuledUnderHostName(env.Permissions, key); got != "github/get_issue" {
			t.Errorf("%s: got %q", key, got)
		}
	}
}
