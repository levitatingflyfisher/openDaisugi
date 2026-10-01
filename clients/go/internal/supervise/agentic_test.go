package supervise

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pyjson"
)

func agenticEnvelope(t *testing.T, id string, read []string, shell bool) *pyjson.Object {
	t.Helper()
	reads := []any{}
	for _, r := range read {
		reads = append(reads, r)
	}
	env, err := gateroot.Validate(pyjson.NewObject().Set("id", id).Set("generated_by", "test").Set("task", "t").
		Set("permissions", pyjson.NewObject().Set("file_read", reads).Set("shell", shell)))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// fakeAgent is a `claude` that writes its argv, one word a line, to args
// and prints out.
func fakeAgent(t *testing.T, dir, out string) *llm.Client {
	t.Helper()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nfor a; do printf '%s\\n' \"$a\"; done > " + filepath.Join(dir, "args") +
		"\ncat > /dev/null\nprintf '%s' '" + out + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(string) (string, bool) { return "", false }
	return llm.New(llm.Env{Getenv: getenv, Home: dir, LookPath: func(string) (string, error) { return bin, nil }})
}

func agenticStep(ws string, tools []string, child *pyjson.Object) *pyjson.Object {
	ts := []any{}
	for _, x := range tools {
		ts = append(ts, x)
	}
	s := pyjson.NewObject().Set("id", "g").Set("depends_on", []any{}).Set("type", "agentic").
		Set("prompt", "fix it").Set("workspace", ws).Set("tools", ts).Set("max_turns", nil)
	if child != nil {
		s.Set("child_envelope", child)
	}
	return s
}

func TestAgenticRunsUnderTheGate(t *testing.T) {
	dir, ws := t.TempDir(), t.TempDir()
	tmp := t.TempDir()
	parent := agenticEnvelope(t, "env_parent", []string{ws + "/**"}, true)
	child := agenticEnvelope(t, "env_child", []string{ws + "/**"}, false)
	a := &Agentic{Envelope: parent, Model: "haiku", Self: "/opt/daisugi", TempDir: tmp,
		Claude: fakeAgent(t, dir, `{"type": "result", "is_error": false, "result": "done"}`)}
	r, err := a.Run(agenticStep(ws, []string{"Read", "Bash"}, child), 30, 1000)
	if err != nil || r.RC != 0 || r.Stdout != "done" {
		t.Fatalf("got %+v %v", r, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	words := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
	if words[0] != "-p" || words[1] != "--model=haiku" || words[len(words)-2] != "--allowedTools" ||
		words[len(words)-1] != "Read" {
		t.Fatalf("argv %q", words)
	}
	dirs, _ := filepath.Glob(filepath.Join(tmp, "daisugi-agentic-*"))
	if len(dirs) != 1 {
		t.Fatalf("tree dirs %v", dirs)
	}
	root := filepath.Join(dirs[0], "gate")
	if !strings.Contains(string(args), "/opt/daisugi gate check --mode enforce --root "+root) ||
		!strings.Contains(string(args), "--session agentic-g") {
		t.Fatalf("settings %s", args)
	}
	for _, d := range []string{root, filepath.Join(dirs[0], "tree")} {
		if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, st, err)
		}
	}
	// The tree: the caller's envelope as the root, the child proved under
	// it, both ended; the child's envelope is unregistered at its end.
	ledger, _ := os.ReadFile(filepath.Join(dirs[0], "tree", "ledger.jsonl"))
	for _, want := range []string{`"event": "root", "session": "root", "envelope_id": "env_parent"`,
		`"event": "spawn", "parent": "root", "session": "agentic-g", "envelope_id": "env_child"`,
		`"model": "haiku"`, `"event": "end", "session": "agentic-g"`, `"event": "end", "session": "root"`} {
		if !strings.Contains(string(ledger), want) {
			t.Fatalf("ledger lacks %s:\n%s", want, ledger)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "envelopes", "agentic-g.json")); err == nil {
		t.Fatal("an ended child keeps no envelope")
	}
}

func TestAgenticFailsClosed(t *testing.T) {
	dir, ws, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	parent := agenticEnvelope(t, "env_parent", []string{ws + "/**"}, true)
	a := &Agentic{Envelope: parent, Model: "haiku", Self: "/opt/daisugi", TempDir: tmp,
		Claude: fakeAgent(t, dir, `{"type": "result", "is_error": true, "result": "no"}`)}
	cases := []struct {
		step *pyjson.Object
		want string
	}{
		{agenticStep(ws+"/missing", []string{"Read"}, nil),
			"agentic workspace '" + ws + "/missing' does not exist or is not a directory"},
		{agenticStep(ws, []string{"Read"}, agenticEnvelope(t, "env_c", []string{"/**"}, false)),
			"the child envelope is refused: "},
		{agenticStep(ws, []string{"Bash"}, agenticEnvelope(t, "env_c", []string{ws + "/**"}, false)),
			"no requested tool is backed by the envelope (requested ['Bash']); nothing to delegate"},
		{agenticStep(ws, []string{"Read"}, nil), "agentic sub-agent reported is_error: no"},
	}
	for _, c := range cases {
		r, err := a.Run(c.step, 30, 1000)
		if err != nil || r.RC != 1 || !strings.HasPrefix(r.Stdout, c.want) {
			t.Fatalf("got %+v %v, want %q", r, err, c.want)
		}
	}
}
