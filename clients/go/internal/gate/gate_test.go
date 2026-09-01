package gate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testEnvelope = `{"id": "env_t", "generated_by": "t", "task": "t", "permissions": {
 "file_read": ["/work/**"], "file_write": ["/work/**"], "shell": true,
 "shell_allowlist": ["ls", "git"], "network": false}}`

func setup(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "data", "gate")
	if err := os.MkdirAll(filepath.Join(root, "envelopes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(testEnvelope), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=/home/user", "PATH=/usr/bin:/bin"}
	return root, env
}

func run(t *testing.T, root string, env []string, mode, payload string) Result {
	t.Helper()
	return Run([]string{"--mode", mode, "--root", root, "--format", "claude"}, []byte(payload), env)
}

func TestReadInsideEnvelopeIsAllowed(t *testing.T) {
	root, env := setup(t)
	res := run(t, root, env, "enforce", `{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a.txt"},"cwd":"/work"}`)
	if !res.Native || res.Exit != 0 || res.Stdout != "{\"continue\": true}\n" {
		t.Fatalf("got %+v", res)
	}
	log, err := os.ReadFile(filepath.Join(root, "shadow", "s1.jsonl"))
	if err != nil || !strings.Contains(string(log), `"reason": "verified in envelope"`) {
		t.Fatalf("shadow log: %v %s", err, log)
	}
}

func TestUnknownToolIsDeniedInEnforce(t *testing.T) {
	root, env := setup(t)
	res := run(t, root, env, "enforce", `{"session_id":"s1","tool_name":"TodoWrite","tool_input":{}}`)
	want := "openDaisugi gate: DENIED \u2014 unrecognized tool 'TodoWrite' \u2014 not in the gate's classification map, denied by default\n"
	if !res.Native || res.Exit != 2 || res.Stderr != want {
		t.Fatalf("got %+v", res)
	}
}

func TestShadowAllowsWhatEnforceDenies(t *testing.T) {
	root, env := setup(t)
	res := run(t, root, env, "shadow", `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"rm -rf /work"},"cwd":"/work"}`)
	if !res.Native || res.Exit != 0 || res.Stdout != "{\"continue\": true}\n" {
		t.Fatalf("got %+v", res)
	}
}

// A reviewer's fail-open: an extglob hid $(rm x) from the decomposition.
// On tree-sitter-bash, the oracle's grammar, each of these is refused, so
// the gate denies it natively, as the oracle does.
func TestCompoundThatHidesAHeadIsDeniedNatively(t *testing.T) {
	root, env := setup(t)
	envelope := `{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["echo","ls"],
	 "shell_allow_decomposition":true,"file_read":["/work/**"],"file_write":["/work/**"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"echo @(a|$(rm x)) ; ls", "echo !(a|$(rm x)) ; ls", "echo +($(rm x)) ; ls", "echo *(`rm x`) ; ls",
		"echo ?(<(rm x)) ; ls", "ls @($(rm x)) | ls", "[ a '<' b ] && ls", "{a,b} ; ls", "x#y 2>&1",
	} {
		p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": cmd}})
		res := run(t, root, env, "enforce", string(p))
		if !res.Native || res.Exit != 2 {
			t.Errorf("%q: native=%v exit=%d, want a native deny", cmd, res.Native, res.Exit)
		}
	}
	p := `{"session_id":"s","tool_name":"Bash","cwd":"/work","tool_input":{"command":"ls && echo hi"}}`
	if res := run(t, root, env, "enforce", p); !res.Native || res.Exit != 0 {
		t.Errorf("a plain compound command: %+v", res)
	}
}

// A glob with many ** segments is exponential to match. Past the fixed
// step limit both gates deny with the same reason, whatever the box.
func TestManyDoubleStarGlobDeniesAtTheStepLimit(t *testing.T) {
	root, env := setup(t)
	glob := "/**/a/**/a/**/a/**/a/**/a/**/a/**/a/**/a/**/a/b"
	envelope := `{"generated_by":"t","task":"t","permissions":{"file_read":["` + glob + `"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	path := "/" + strings.Repeat("a/", 40) + "c"
	p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Read", "cwd": "/work",
		"tool_input": map[string]string{"file_path": path}})
	start := time.Now()
	res := run(t, root, env, "enforce", string(p))
	want := "openDaisugi gate: DENIED — gate internal error (denied fail-closed): file glob '" + glob +
		"' is too complex to match: more than 100000 steps.\n"
	if !res.Native || res.Exit != 2 || res.Stderr != want || time.Since(start) > 5*time.Second {
		t.Fatalf("got %+v after %s", res, time.Since(start))
	}
}

// The native verifier runs under --verify-timeout and denies, in the
// oracle's words, when the budget runs out.
func TestVerifyDeadlineDenies(t *testing.T) {
	r := &runner{mode: "enforce", t0: time.Now(), verifyTimeoutS: 0.05}
	r.verify = func(*record, *envelope) []violation {
		time.Sleep(2 * time.Second)
		return nil
	}
	rec := &record{ToolName: "Read", StepType: "file_read", Path: "/work/a"}
	d := r.evaluateRecord(rec, &envelope{ID: "env_x"}, nil, nil)
	want := "gate internal error (denied fail-closed): verifier exceeded the gate's inner time budget (0.05s)."
	if d.Allow || !d.WouldDeny || d.Reason != want || d.Tier != tierPermanent || d.PlanID != nil {
		t.Fatalf("got %+v", d)
	}
}

// ebd4dcc: after a cd the gate cannot follow, a relative word that names
// a floor directory by its last part is a floor hit, as in the oracle.
func TestFloorWordAfterUnfollowedCdIsDenied(t *testing.T) {
	root, env := setup(t)
	envelope := `{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["cd","cp","ls"],
	 "shell_allow_decomposition":true,"file_read":["/**"],"file_write":["/**"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	for cmd, deny := range map[string]bool{
		"cd .. && cp a .config/coppice/x": true,
		"cd .. && cp a opencode/x":        false, // the floor guard's names only; opencode's is checked below
		"cd .. && cp a b/c":               false,
		"cp a .config/coppice/x":          false, // cwd /work is followed: /work/.config/coppice is not the floor
	} {
		p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": cmd}})
		res := run(t, root, env, "enforce", string(p))
		if !res.Native {
			t.Errorf("%q: handed to Python (%s)", cmd, res.Why)
			continue
		}
		got := strings.Contains(res.Stderr, floorRefusal)
		if got != deny {
			t.Errorf("%q: floor refusal %v, want %v (%q)", cmd, got, deny, res.Stderr)
		}
	}
}

// A line of 400 redirects made the floor rule place every word against
// every cwd, once per command (the oracle took 257 s). Each cwd is now
// kept once, as in 84d1999, so the line answers at once.
func TestFourHundredSegmentLineAnswersQuickly(t *testing.T) {
	root, env := setup(t)
	envelope := `{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["echo","ls"],
	 "shell_allow_decomposition":true,"file_read":["/work/**"],"file_write":["/work/**"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	parts := make([]string, 400)
	for i := range parts {
		parts[i] = fmt.Sprintf("echo %d > /work/o%d", i, i)
	}
	p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
		"tool_input": map[string]string{"command": strings.Join(parts, " ; ")}})
	start := time.Now()
	res := run(t, root, env, "enforce", string(p))
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("took %s", took)
	}
	if !res.Native || res.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
	// The same line writing outside the envelope is denied, and its tier
	// is worked out too, still quickly.
	for i := range parts {
		parts[i] = fmt.Sprintf("echo %d > /etc/o%d", i, i)
	}
	p, _ = json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
		"tool_input": map[string]string{"command": strings.Join(parts, " ; ")}})
	start = time.Now()
	res = run(t, root, env, "enforce", string(p))
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("deny took %s", took)
	}
	if !res.Native || res.Exit != 2 {
		t.Fatalf("got %+v", res)
	}
}

// 50684bb: past 32 distinct directories the gate stops following cd. A
// line of 400 or 1,000 cds into different directories answers at once,
// and a relative floor word after the cap is refused as after any cd the
// gate cannot follow.
func TestManyDistinctCdsAnswerQuicklyAndStopBeingFollowed(t *testing.T) {
	root, env := setup(t)
	envelope := `{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["cd","echo","cp"],
	 "shell_allow_decomposition":true,"file_read":["/**"],"file_write":["/**"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{400, 1000} {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("cd /work/d%d ; echo %d > o%d", i, i, i)
		}
		p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]string{"command": strings.Join(parts, " ; ")}})
		start := time.Now()
		res := run(t, root, env, "enforce", string(p))
		if took := time.Since(start); took > 900*time.Millisecond {
			t.Fatalf("%d cds took %s", n, took)
		}
		if !res.Native {
			t.Fatalf("%d cds handed to Python: %s", n, res.Why)
		}
	}
	// 33 distinct cds, then a relative floor word: the cwd is no longer
	// followed, so the word names the floor by its last part.
	parts := make([]string, 33)
	for i := range parts {
		parts[i] = fmt.Sprintf("cd /work/d%d", i)
	}
	cmd := strings.Join(parts, " ; ") + " ; cp a .config/coppice/x"
	p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
		"tool_input": map[string]string{"command": cmd}})
	if res := run(t, root, env, "enforce", string(p)); !strings.Contains(res.Stderr, floorRefusal) {
		t.Errorf("a floor word after the cd cap: %+v", res)
	}
}

// The hard-deny rules run under the same budget as the verifier and
// deny when it runs out.
func TestHardDenyRulesDeadlineDenies(t *testing.T) {
	r := &runner{mode: "enforce", t0: time.Now(), verifyTimeoutS: 0.05}
	late := r.runBounded(func() { time.Sleep(2 * time.Second) })
	if !late {
		t.Fatal("a slow rule stage did not run out of budget")
	}
}

// fb7acaa: the state report carries the transcript path (absolute only),
// the verdict {decision, tool, clause} and the floor's mode word.
func TestStateReportCarriesTranscriptVerdictAndMode(t *testing.T) {
	root, env := setup(t)
	cases := []struct {
		mode, payload, want string
	}{
		{"enforce", `{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work","transcript_path":"/t/x.jsonl"}`,
			`"detail": "verdict=allow", "transcript_path": "/t/x.jsonl", "verdict": {"decision": "allow", "tool": "Read", "clause": ""}, "mode": "enforcing"}`},
		{"shadow", `{"session_id":"s2","tool_name":"Bash","tool_input":{"command":"rm x"},"cwd":"/work","transcript_path":"rel/x.jsonl"}`,
			`"verdict": {"decision": "allow", "tool": "Bash", "clause": "permissions: Step 's0' shell command 'rm' not in allowlist ['ls', 'git']"}, "mode": "watching"}`},
	}
	for _, c := range cases {
		res := run(t, root, env, c.mode, c.payload)
		if !res.Native {
			t.Fatalf("handed to Python: %s", res.Why)
		}
		var sid struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal([]byte(c.payload), &sid)
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(root), "sessions", sid.SessionID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if last := lines[len(lines)-1]; !strings.HasSuffix(last, c.want) {
			t.Errorf("state entry %s\nwant suffix %s", last, c.want)
		}
	}
}

func TestPathHelpersMatchPython(t *testing.T) {
	cases := map[string]string{"": ".", "//a//b/": "//a/b", "///a/../b": "/b", "a/./b/..": "a", "../x": "../x"}
	for in, want := range cases {
		if got := normpath(in); got != want {
			t.Errorf("normpath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := pathParent("gate"); got != "." {
		t.Errorf("pathParent(gate) = %q", got)
	}
	if got := pathParent("/a/b/"); got != "/a" {
		t.Errorf("pathParent(/a/b/) = %q", got)
	}
	if got := pathParent("/a"); got != "/" {
		t.Errorf("pathParent(/a) = %q", got)
	}
	if c, ok := commonpath("/a/b/c", "/a/b"); !ok || c != "/a/b" {
		t.Errorf("commonpath = %q %v", c, ok)
	}
	if _, ok := commonpath("/a", "b"); ok {
		t.Error("commonpath of mixed paths must fail")
	}
	if got := splitlines("a\nb\r\nc\u2028d\re"); strings.Join(got, "|") != "a|b|c|d|e" {
		t.Errorf("splitlines = %q", got)
	}
}

func TestRealpathFollowsSymlinks(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "real", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	r := &runner{home: "/home/user", cwd: "/"}
	if got := r.realpath(dir + "/link/sub/missing"); got != dir+"/real/sub/missing" {
		t.Errorf("realpath through a link = %q", got)
	}
	if got := r.realpath(dir + "/link/../x"); got != dir+"/x" {
		t.Errorf("realpath with .. after a link = %q", got)
	}
	if got := r.realpath(dir + "/loop/x"); got != dir+"/loop/x" {
		t.Errorf("realpath of a loop = %q", got)
	}
}

func TestURLSchemeHost(t *testing.T) {
	cases := [][3]string{
		{"https://Example.COM:8080/x", "https", "example.com"},
		{"HTTP://user:pw@Host/p", "http", "host"},
		{"file:///etc/passwd", "file", ""},
		{"example.com/x", "", ""},
		{" \thttps://a.b", "https", "a.b"},
		{"h%x://a", "", ""},
		{"https://[::1]:8/x", "https", "::1"},
		{"https://[fe80::1%Eth0]/x", "https", "fe80::1%Eth0"},
		{"https://ΑΣ.gr/", "https", "ασ.gr"},
		{"https://ΑΣ/", "https", "ας"},
	}
	for _, c := range cases {
		s, n := urlSplit(c[0])
		if h := urlHostname(n); s != c[1] || h != c[2] {
			t.Errorf("urlsplit(%q) = %q %q, want %q %q", c[0], s, h, c[1], c[2])
		}
	}
	for u, want := range map[string]string{
		"https://[x]/":          "'x' does not appear to be an IPv4 or IPv6 address",
		"https://[1.2.3.4]/":    "An IPv4 address cannot be in brackets",
		"https://a[::1]/":       "Invalid IPv6 URL",
		"https://[::1/":         "Invalid IPv6 URL",
		"https://[vz.1]/":       "IPvFuture address is invalid",
		"https://ex\u2100.com/": "netloc 'ex\u2100.com' contains invalid characters under NFKC normalization",
	} {
		exc := catch(func() { urlSplit(u) })
		if exc == nil || exc.Msg != want {
			t.Errorf("urlsplit(%q) raised %v, want %q", u, exc, want)
		}
	}
}

// sys.stderr escapes a lone surrogate, so a reason that carries one is
// written as ASCII, never as bytes that are not UTF-8.
func TestStderrEscapesALoneSurrogate(t *testing.T) {
	res := outcome(&decision{Reason: "tool 'zz\xed\xa0\x80'"}, "claude")
	if want := "openDaisugi gate: DENIED — tool 'zz\\ud800'\n"; res.Stderr != want {
		t.Fatalf("got %q, want %q", res.Stderr, want)
	}
}
