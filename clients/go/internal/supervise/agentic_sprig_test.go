package supervise

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
)

// fakeSprig is a `sprig` that writes its argv, one word a line, to args,
// its cwd to cwd and its stdin to stdin, then runs body.
func fakeSprig(t *testing.T, dir, body string) string {
	t.Helper()
	bin := filepath.Join(dir, "sprig")
	script := "#!/bin/sh\nfor a; do printf '%s\\n' \"$a\"; done > " + filepath.Join(dir, "args") +
		"\npwd > " + filepath.Join(dir, "cwd") + "\ncat > " + filepath.Join(dir, "stdin") + "\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func sprigAgentic(t *testing.T, ws, tmp, bin string, shell bool) *Agentic {
	parent := agenticEnvelope(t, "env_parent", []string{ws + "/**"}, shell)
	return &Agentic{Envelope: parent, Model: "haiku", Self: "/opt/daisugi", TempDir: tmp, Runtime: "sprig", Sprig: bin}
}

func TestAgenticSprigRunsUnderThePinnedGate(t *testing.T) {
	dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	bin := fakeSprig(t, dir, `printf '%s' '{"answer": "done", "turns": 2}'`)
	step := agenticStep(ws, []string{"Read", "Glob", "Bash"}, nil)
	step.Set("max_turns", nil)
	r, err := sprigAgentic(t, ws, tmp, bin, true).Run(step, 30, 1000)
	if err != nil || r.RC != 0 || r.Stdout != "done" {
		t.Fatalf("got %+v %v", r, err)
	}
	dirs, _ := filepath.Glob(filepath.Join(tmp, "daisugi-agentic-*"))
	if len(dirs) != 1 {
		t.Fatalf("tree dirs %v", dirs)
	}
	root := filepath.Join(dirs[0], "gate")
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	want := []string{"--json", "--gate", "--gate-cmd",
		"/opt/daisugi gate check --mode enforce --root " + root + " --session agentic-g --captures-root " + root + "/captures",
		"--tools", "read,bash", "--model", "haiku", "--session-dir", root + "/sessions", "--session", "agentic-g", "-"}
	if got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	cwd, _ := os.ReadFile(filepath.Join(dir, "cwd"))
	if string(stdin) != "fix it" || strings.TrimSpace(string(cwd)) != ws {
		t.Fatalf("stdin %q cwd %q", stdin, cwd)
	}
	ledger, _ := os.ReadFile(filepath.Join(dirs[0], "tree", "ledger.jsonl"))
	if !strings.Contains(string(ledger), `"event": "end", "session": "agentic-g"`) {
		t.Fatalf("ledger %s", ledger)
	}
}

func TestAgenticSprigProfileKeyAndBundle(t *testing.T) {
	dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	// The fake records the size of what descriptors 3 and 4 hold.
	bin := fakeSprig(t, dir, `for n in 3 4; do if [ -e /proc/$$/fd/$n ]; then wc -c < /proc/$$/fd/$n > `+
		filepath.Join(dir, "fd")+`$n; fi; done; printf '%s' '{"answer": "ok", "usage": {"input_tokens": 9}, "model_turns": 1}'`)
	parent := agenticEnvelope(t, "env_parent", []string{ws + "/**"}, false)
	perms := parent.Value("permissions").(*pyjson.Object)
	perms.Set("mcp_allowlist", []any{"sprig/spawn"})
	key := "sk-SENTINEL-go"
	raw := config.Value{Kind: config.Map, Keys: []string{"oai"}, Map: map[string]config.Value{
		"oai": {Kind: config.Map, Keys: []string{"wire", "base_url", "key_env"}, Map: map[string]config.Value{
			"wire": {Kind: config.Str, Text: "openai"}, "base_url": {Kind: config.Str, Text: "http://127.0.0.1:9/v1"},
			"key_env": {Kind: config.Str, Text: "OAI_KEY"}}}}}
	ps := agentprof.Resolve(&raw, func(k string) (string, bool) { return key, k == "OAI_KEY" })
	a := &Agentic{Envelope: parent, Model: "haiku", Self: "/opt/daisugi", TempDir: tmp, Runtime: "claude",
		Sprig: bin, Profiles: ps, Environ: []string{"PATH=/usr/bin:/bin", "OAI_KEY=" + key, "OPENAI_API_KEY=x"}}
	step := agenticStep(ws, []string{"Read", "Spawn"}, nil)
	step.Set("profile", "oai")
	r, err := a.Run(step, 30, 1000)
	if err != nil || r.RC != 0 || r.Stdout != "ok" {
		t.Fatalf("got %+v %v", r, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	got := string(args)
	for _, want := range []string{"--tools\nread,spawn\n", "--wire\nopenai\n", "--base-url\nhttp://127.0.0.1:9/v1\n",
		"--key-fd\n3\n", "--scrub-env\nOAI_KEY\n", "--spawn-keys-fd\n4\n", "--tree-cmd\n/opt/daisugi tree run --data-dir "} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, key) {
		t.Fatal("the key is in the argv")
	}
	fd3, _ := os.ReadFile(filepath.Join(dir, "fd3"))
	fd4, _ := os.ReadFile(filepath.Join(dir, "fd4"))
	if strings.TrimSpace(string(fd3)) != fmt.Sprint(len(key)) || strings.TrimSpace(string(fd4)) != fmt.Sprint(len(ps.Bundle())) {
		t.Fatalf("fd3 %q fd4 %q", fd3, fd4)
	}
}

func TestAgenticSprigFailures(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`echo 'sprig: one' >&2; echo 'sprig: gave up' >&2; exit 3`, "agentic sub-agent failed: sprig exited with code 3: sprig: gave up"},
		{`exit 1`, "agentic sub-agent failed: sprig exited with code 1"},
		{`printf 'I did it.'`, "agentic sub-agent returned unparseable output: 'I did it.'"},
		{`printf '[1]'`, "agentic sub-agent reply has no answer: '[1]'"},
		{`printf '{"answer": 7}'`, `agentic sub-agent reply has no answer: '{"answer": 7}'`},
		{`kill -9 $$`, "agentic sub-agent failed: sprig was killed by signal 9"},
	} {
		dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
		r, err := sprigAgentic(t, ws, tmp, fakeSprig(t, dir, tc.body), false).Run(agenticStep(ws, []string{"Read"}, nil), 30, 1000)
		if err != nil || r.RC != 1 || r.Stdout != tc.want {
			t.Fatalf("%s: got %+v %v", tc.body, r, err)
		}
	}
}

func TestAgenticSprigMissingBinary(t *testing.T) {
	ws, tmp := t.TempDir(), t.TempDir()
	bin := filepath.Join(t.TempDir(), "nope")
	r, err := sprigAgentic(t, ws, tmp, bin, false).Run(agenticStep(ws, []string{"Read"}, nil), 30, 1000)
	if err != nil || r.RC != 1 || r.Stdout != "agentic sub-agent failed: cannot start sprig ("+bin+")" {
		t.Fatalf("got %+v %v", r, err)
	}
}

func TestAgenticSprigTimeoutKillsTheGroup(t *testing.T) {
	dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	bin := fakeSprig(t, dir, "sleep 60 & echo $! > "+filepath.Join(dir, "child")+"; wait")
	t0 := time.Now()
	r, err := sprigAgentic(t, ws, tmp, bin, false).Run(agenticStep(ws, []string{"Read"}, nil), 1, 1000)
	if err != nil || r.RC != 1 || r.Stdout != "agentic sub-agent failed: sprig ran past 1s and was killed" {
		t.Fatalf("got %+v %v", r, err)
	}
	if time.Since(t0) > 15*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
	// The grandchild is gone, or a zombie waiting for its reaper.
	pid, _ := os.ReadFile(filepath.Join(dir, "child"))
	stat := "/proc/" + strings.TrimSpace(string(pid)) + "/stat"
	for i := 0; ; i++ {
		b, err := os.ReadFile(stat)
		if err != nil || strings.Contains(string(b), ") Z ") {
			break
		}
		if i == 40 {
			t.Fatalf("the grandchild %s outlived the kill: %s", pid, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAgenticSprigOnlyUnbackedToolsDelegatesNothing(t *testing.T) {
	dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	r, err := sprigAgentic(t, ws, tmp, fakeSprig(t, dir, "exit 0"), false).Run(agenticStep(ws, []string{"Grep"}, nil), 30, 1000)
	if err != nil || r.RC != 1 || r.Stdout != "no requested tool is backed by the envelope (requested ['Grep']); nothing to delegate" {
		t.Fatalf("got %+v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); err == nil {
		t.Fatal("sprig must not start")
	}
}

func TestAgenticSprigSpaceInTheGateCommandFails(t *testing.T) {
	dir, ws := t.TempDir(), t.TempDir()
	tmp := filepath.Join(t.TempDir(), "t mp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := sprigAgentic(t, ws, tmp, fakeSprig(t, dir, "exit 0"), false).Run(agenticStep(ws, []string{"Read"}, nil), 30, 1000)
	if err != nil || r.RC != 1 || r.Stdout != SprigSpaceText {
		t.Fatalf("got %+v %v", r, err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("nothing may be made: %v", entries)
	}
}

// TestAgenticSprigLeftoverInTheGroupDies: sprig answers and exits; a
// process it left in its group dies with the group kill on exit (MX-R-8),
// so the run ends with the answer.
func TestAgenticSprigLeftoverInTheGroupDies(t *testing.T) {
	dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	t0 := time.Now()
	body := `printf '{"answer": "done"}'; sleep 60 &`
	r, err := sprigAgentic(t, ws, tmp, fakeSprig(t, dir, body), false).Run(agenticStep(ws, []string{"Read"}, nil), 10, 1000)
	if err != nil || r.RC != 0 || r.Stdout != "done" || time.Since(t0) > 5*time.Second {
		t.Fatalf("got %+v %v", r, err)
	}
}

// TestAgenticSprigLeftoverPipeIsATimeout: sprig answers and exits, but a
// process outside its group still holds its stdout. As in the oracle, the
// run is a timeout, and it ends soon after even though the group kill
// cannot reach that process (setsid).
func TestAgenticSprigLeftoverPipeIsATimeout(t *testing.T) {
	for _, body := range []string{
		`printf '{"answer": "done"}'; setsid sleep 60 & sleep 0.3`,
	} {
		dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
		t0 := time.Now()
		r, err := sprigAgentic(t, ws, tmp, fakeSprig(t, dir, body), false).Run(agenticStep(ws, []string{"Read"}, nil), 1, 1000)
		if err != nil || r.RC != 1 || r.Stdout != "agentic sub-agent failed: sprig ran past 1s and was killed" {
			t.Fatalf("%s: got %+v %v", body, r, err)
		}
		if d := time.Since(t0); d > time.Duration(1+sprigDrainS+3)*time.Second {
			t.Fatalf("%s: took %v", body, d)
		}
	}
}
