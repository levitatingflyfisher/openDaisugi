package envdefault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/warden"
	"daisugi-verify/internal/wardenroot"
)

// check runs the real warden, in enforce mode, on one Bash call in dir
// under the envelope env, and returns its exit code and stderr.
func check(t *testing.T, env []byte, dir, command string) (int, string) {
	t.Helper()
	root := t.TempDir()
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, string(env))
	if verr != nil {
		t.Fatalf("the envelope does not validate: %s", verr.String())
	}
	if _, err := wardenroot.Register(v.(*pyjson.Object), "s1", root); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command},
		"cwd": dir, "session_id": "s1"})
	res := warden.Run([]string{"--mode", "enforce", "--root", root, "--session", "s1"}, payload,
		[]string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"})
	return res.Exit, res.Stderr
}

func TestTheDefaultDeniesPublishAndAllowsWork(t *testing.T) {
	dir := t.TempDir()
	env := Envelope(dir)
	for _, c := range []string{
		"git push", "git push origin main", "cd sub && git push", "bash -c 'git push'", "git -C . push",
		"env GIT_TRACE=1 git push", "echo x; git push", "(git push)",
		"gh release create v1", "gh -R a/b release upload v1 x", "gh pr merge 3",
		"flutter build appbundle", "fastlane deploy", "bundle exec fastlane beta", "stripe listen",
		"npm publish", "cargo publish --dry-run",
	} {
		if code, errs := check(t, env, dir, c); code != 2 || !strings.Contains(errs, "deny_") {
			t.Errorf("%q: exit %d, %s", c, code, errs)
		}
	}
	// sudo is denied before the invariants: the warden cannot place what a
	// runner head reads or writes.
	if code, _ := check(t, env, dir, "sudo git push"); code != 2 {
		t.Errorf("sudo git push: exit %d", code)
	}
	for _, c := range []string{
		"git status", "git commit -m 'push the fix'", "git log --grep push", "make test",
		"./gradlew assembleDebug", "flutter build apk", "flutter test", "cat src/stripe/x.dart",
		"npm test", "cargo build --release", "echo push",
	} {
		if code, errs := check(t, env, dir, c); code != 0 {
			t.Errorf("%q: exit %d, %s", c, code, errs)
		}
	}
}

// The envelope names each denied verb in its deny line, so the worker and
// the owner see which rule stopped it.
func TestADenyNamesTheVerb(t *testing.T) {
	dir := t.TempDir()
	_, errs := check(t, Envelope(dir), dir, "git push")
	if !strings.Contains(errs, "deny_git_push") {
		t.Fatalf("stderr = %s", errs)
	}
}

func TestWithDenyKeepsTheProposalAndSkipsAllowedVerbs(t *testing.T) {
	dir := t.TempDir()
	in := []byte(`{"generated_by":"steward","task":"t","permissions":{"file_read":["` + dir + `"],"shell":true,"shell_allowlist":["git"]},` +
		`"invariants":[{"type":"deny_stripe","description":"mine","expr":null,"enforce":false}]}`)
	out, err := WithDeny(in, []string{"git push"})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Invariants []struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"invariants"`
		Permissions map[string]any `json:"permissions"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, inv := range env.Invariants {
		types[inv.Type]++
	}
	if types["deny_git_push"] != 0 || types["deny_stripe"] != 1 || types["deny_gh_release"] != 1 {
		t.Fatalf("invariants = %+v", env.Invariants)
	}
	if env.Permissions["shell_allowlist"].([]any)[0] != "git" {
		t.Fatalf("permissions changed: %v", env.Permissions)
	}
	if code, _ := check(t, out, dir, "git push"); code != 0 {
		t.Fatal("an allowed verb was denied")
	}
}

func TestWiderSaysWhatIsWider(t *testing.T) {
	dir := "/home/o/code/trellis"
	fits := []byte(`{"permissions":{"file_read":["` + dir + `","` + dir + `/**","` + dir + `/src/*.dart"],"file_write":["` + dir + `/build"],` +
		`"shell":true,"shell_allowlist":["git","flutter","./gradlew"],"max_execution_time_s":30}}`)
	if w := Wider(fits, 0, 0, nil, dir); len(w) != 0 {
		t.Fatalf("a proposal inside the default is wider: %v", w)
	}
	if w := Wider(Envelope(dir), 0, 0, nil, dir); len(w) != 0 {
		t.Fatalf("the default is wider than itself: %v", w)
	}
	wide := []byte(`{"permissions":{"file_read":["/home/o/code/trellis2","../x"],"file_write":["/tmp/out"],` +
		`"network":true,"network_hosts":["pub.dev"],"shell_allowlist":["/usr/bin/make","stripe"],"mcp_allowlist":["a/b"],` +
		`"max_execution_time_s":3600},"shell_interpreter_policy":"allow"}`)
	if w, _ := WithDeny(fits, nil); len(Wider(w, 0, 0, nil, dir)) != 0 {
		t.Fatalf("the canonical denies make a proposal wider: %v", Wider(w, 0, 0, nil, dir))
	}
	got := strings.Join(Wider(wide, 3000000, 600, []string{"git push"}, dir), "\n")
	for _, want := range []string{
		"reads outside the project: /home/o/code/trellis2, ../x",
		"writes outside the project: /tmp/out",
		"a budget: 3000000 tokens, 600 turns",
		"network: pub.dev",
		"runs denied verbs: git push",
		"shell commands outside the default: /usr/bin/make, stripe",
		"MCP tools: a/b",
		"steps up to 3600 s (the default is 30)",
		"shell interpreters without a warning",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wider lacks %q:\n%s", want, got)
		}
	}
}

func TestUnderRootIsAPathPrefixAndFollowsLinks(t *testing.T) {
	base := t.TempDir()
	code := filepath.Join(base, "code")
	for _, d := range []string{filepath.Join(code, "trellis"), filepath.Join(base, "code2", "x"), filepath.Join(base, "else")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(code, "out")
	if err := os.Symlink(filepath.Join(base, "else"), link); err != nil {
		t.Fatal(err)
	}
	roots := []string{code}
	for dir, want := range map[string]bool{
		filepath.Join(code, "trellis"):    true,
		code:                              true,
		filepath.Join(base, "code2", "x"): false,
		link:                              false,
		filepath.Join(base, "else"):       false,
	} {
		if got := UnderRoot(dir, roots); got != want {
			t.Errorf("UnderRoot(%s) = %v", dir, got)
		}
	}
	if UnderRoot(filepath.Join(base, "else"), []string{"/"}) {
		t.Error("/ is a trusted root")
	}
}

func TestCheckAllowRefusesAnUnknownVerb(t *testing.T) {
	if err := CheckAllow([]string{"git push"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckAllow([]string{"rm -rf"}); err == nil || !strings.Contains(err.Error(), "git push") {
		t.Fatalf("err = %v", err)
	}
}

// Review r9 finding 1: a relative pattern, a bare glob, a .. and a link that
// leaves the project are all wider than the default. The warden reads a
// bare ** as everything, so Within must never read it as inside.
func TestAGlobRelativeOrEscapingPathIsWider(t *testing.T) {
	dir := t.TempDir()
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(dir, "away")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"**", "**/x", "../x/**", "*", "src/x", "", dir + "/../x", dir + "/src/../../x",
		dir + "/away", dir + "/away/**", "/**", dir + "*"} {
		if Within(p, dir) {
			t.Errorf("%q reads as inside %s", p, dir)
		}
		env := []byte(`{"permissions":{"file_read":[` + strconvQuote(p) + `]}}`)
		if len(Wider(env, 0, 0, nil, dir)) == 0 {
			t.Errorf("a proposal reading %q fits the default", p)
		}
	}
	for _, p := range []string{dir, dir + "/**", dir + "/src/*.dart", dir + "/src"} {
		if !Within(p, dir) {
			t.Errorf("%q reads as outside %s", p, dir)
		}
	}
	// The reviewer's probe: under the envelope a fitting grant writes (the
	// default itself), /etc/passwd is out of reach; under a ** proposal it
	// was not.
	if code, _ := check(t, Envelope(dir), dir, "cat /etc/passwd"); code == 0 {
		t.Fatal("cat /etc/passwd ran under the default")
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// Review r9 finding 2: a proposal cannot ship its own deny_* invariant. It
// is wider (it cards), and WithDeny replaces it with the canonical one, so
// git push stays denied even if the owner approves the card.
func TestAProposalCannotShipItsOwnDeny(t *testing.T) {
	dir := t.TempDir()
	neutered := `{"generated_by":"s","task":"t","permissions":{"file_read":["` + dir + `/**"],"file_write":["` + dir + `/**"],` +
		`"shell":true,"shell_allowlist":["*"]},"invariants":[` +
		`{"type":"deny_git_push","description":"x","expr":{"op":"forall_steps","pred":{"op":"implies","a":{"op":"equals","path":"type","value":"shell"},"b":{"op":"not_matches","path":"command","regex":"zzz-never-zzz"}}}},` +
		`{"type":"deny_gh_release","description":"x","enforce":false,"expr":{"op":"forall_steps","pred":{"op":"implies","a":{"op":"equals","path":"type","value":"shell"},"b":{"op":"not_matches","path":"command","regex":"gh"}}}}]}`
	if w := Wider([]byte(neutered), 0, 0, nil, dir); len(w) == 0 || !strings.Contains(strings.Join(w, "\n"), "invariants") {
		t.Fatalf("a proposal with its own deny invariants fits: %v", w)
	}
	out, err := WithDeny([]byte(neutered), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "zzz-never-zzz") || strings.Contains(string(out), `"enforce":false`) {
		t.Fatalf("the proposal's own deny survived:\n%s", out)
	}
	for _, c := range []string{"git push origin main", "gh release create v1"} {
		if code, _ := check(t, out, dir, c); code != 2 {
			t.Errorf("%q: exit %d", c, code)
		}
	}
}

// Review r9 finding 5b: the reviewer's evasions. Wrappers, quotes inside a
// word, a backslash-newline, gh api on release and merge endpoints, and
// the other publishers all deny.
func TestTheDenyListSeesThroughWrappersAndQuotes(t *testing.T) {
	dir := t.TempDir()
	env := Envelope(dir)
	for _, c := range []string{
		"xargs git push", "timeout 60 git push", "nice git push", "ionice -c3 git push", "flock /tmp/l git push",
		"stdbuf -o0 git push", "watch git push", "env A=1 git push",
		`git "push"`, `git pu''sh`, "git \\\npush", `g"i"t push`, `git p\ush`,
		"gh api -X POST repos/o/r/releases -f tag_name=v1", "gh api -X PUT repos/o/r/pulls/3/merge",
		"gh api -X DELETE repos/o/r", "gh repo delete o/r --yes", "gh workflow run deploy.yml",
		"pnpm publish", "yarn publish", "yarn npm publish",
		"gi\\\nt push", "git pu\\\nsh",
		"gh api -f tag_name=v1 repos/o/r/releases", "gh api --method PATCH repos/o/r/releases/1",
		"gh api --input m.json repos/o/r/pulls/3/merge", "gh api -X DELETE repos/o/r/issues/1",
		"gh api --method=delete repos/o/r/git/refs/heads/x",
	} {
		// flock and watch are runners the placer cannot read: denied before
		// the invariants, which is a deny all the same.
		if code, errs := check(t, env, dir, c); code != 2 || !strings.Contains(errs, "deny_") && !strings.Contains(errs, "cannot be placed") {
			t.Errorf("%q: exit %d, %s", c, code, errs)
		}
	}
	for _, c := range []string{"gh api repos/o/r/releases", "gh api repos/o/r/releases/latest --jq .tag_name",
		"gh api repos/o/r/issues/3/comments -f body=merged", "gh api -X GET repos/o/r/releases", "gh api repos/o/r/pulls", "gh pr list", "git pull", "yarn install", "pnpm test", "cat src/stripe/x.dart"} {
		if code, errs := check(t, env, dir, c); code != 0 {
			t.Errorf("%q: exit %d, %s", c, code, errs)
		}
	}
}

// Review r9 fix 2, finding 1: gh api with an attached flag value, a method
// in any case, more write endpoints and a GraphQL mutation all deny; reads
// stay allowed.
func TestGhAPIWritesDenyInEverySpelling(t *testing.T) {
	dir := t.TempDir()
	env := Envelope(dir)
	cases := []struct {
		cmd  string
		deny bool
	}{
		{"gh api -XPOST repos/o/r/releases", true},
		{"gh api -XDELETE repos/o/r", true},
		{"gh api -ftag_name=v1 repos/o/r/releases", true},
		{"gh api -Ftag_name=v1 repos/o/r/releases", true},
		{"gh api --method Post repos/o/r/releases", true},
		{"gh api --method=POST repos/o/r/releases", true},
		{"gh api -X=POST repos/o/r/releases", true},
		{"gh api --field tag_name=v1 repos/o/r/releases", true},
		{"gh api --raw-field=tag_name=v1 repos/o/r/releases", true},
		{"gh api -X Delete repos/o/r/issues/1", true},
		{"gh api -X PATCH repos/o/r/git/refs/heads/main -f sha=abc", true},
		{"gh api -X PUT repos/o/r/contents/x -f message=m", true},
		{"gh api repos/o/r/merges -f base=main -f head=x", true},
		{"gh api repos/o/r/actions/workflows/d.yml/dispatches -f ref=main", true},
		{"gh api graphql -f query='mutation { mergePullRequest(input:{}) { clientMutationId } }'", true},
		{"gh api repos/o/r/releases", false},
		{"gh api -X GET repos/o/r/releases", false},
		{"gh api -XGET repos/o/r/releases", false},
		{"gh api repos/o/r/releases/latest --jq .tag_name", false},
		{"gh api repos/o/r/contents/README.md", false},
		{"gh api repos/o/r/git/refs/heads/main", false},
		{"gh api graphql -f query='query { viewer { login } }'", false},
	}
	for _, c := range cases {
		code, errs := check(t, env, dir, c.cmd)
		if c.deny && (code != 2 || !strings.Contains(errs, "deny_gh_api")) {
			t.Errorf("%q: exit %d, %s", c.cmd, code, errs)
		}
		if !c.deny && code != 0 {
			t.Errorf("%q: exit %d, %s", c.cmd, code, errs)
		}
	}
}
