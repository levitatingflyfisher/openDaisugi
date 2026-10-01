package sprig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTree writes a tree command that logs its argv, stdin and the bytes
// on its descriptor 3, then prints reply.
func fakeTree(t *testing.T, reply string, exit int) (cmd string, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := filepath.Join(dir, "tree")
	body := "#!/bin/sh\n" +
		"{ printf 'ARGV'; for a; do printf ' %s' \"$a\"; done; printf '\\n'; " +
		"printf 'STDIN '; cat; printf '\\n'; printf 'FD3 '; cat <&3 2>/dev/null; printf '\\n'; } >> " + log + "\n" +
		"printf '%s' '" + reply + "'\n" +
		"exit " + itoa(exit) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script + " --data-dir D", log
}

func TestSpawnRunsTheTreeCommandPinnedToItsOwnSession(t *testing.T) {
	cmd, log := fakeTree(t, `{"status":"ran","session":"n-1","answer":"child done","tokens_used":40,"turns_used":2,"reasons":[],"ask_id":null}`, 0)
	s := &SpawnTool{Command: strings.Fields(cmd), Session: "parent-1", Keys: []byte(`{"k":1}`),
		OwnUse: func() (int, int) { return 12, 1 }}
	out, err := s.Run(map[string]any{
		"task": "do x", "envelope": map[string]any{"id": "e"}, "profile": "desk",
		"model": "llama3.3:70b", "tokens": 100.0, "turns": 3.0, "parent": "evil",
	})
	if err != nil || out != "child done" {
		t.Fatalf("out %q err %v", out, err)
	}
	b, _ := os.ReadFile(log)
	got := string(b)
	for _, want := range []string{
		"ARGV --data-dir D --parent parent-1 --profile desk --model llama3.3:70b --tokens 100 --turns 3" +
			" --parent-tokens-used 12 --parent-turns-used 1 --keys-fd 3 -",
		`STDIN {"envelope":{"id":"e"},"task":"do x","tools":[]}`,
		`FD3 {"k":1}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log lacks %q:\n%s", want, got)
		}
	}
	if s.BranchTokens() != 40 {
		t.Fatalf("branch tokens %d", s.BranchTokens())
	}
}

func TestSpawnRefusedAndAsked(t *testing.T) {
	cmd, _ := fakeTree(t, `{"status":"refused","reasons":["tokens: too many","x"]}`, 1)
	s := &SpawnTool{Command: strings.Fields(cmd), Session: "p"}
	out, err := s.Run(map[string]any{"task": "t", "envelope": map[string]any{}})
	if err != nil || out != "refused: tokens: too many; x" {
		t.Fatalf("out %q err %v", out, err)
	}
	cmd, _ = fakeTree(t, `{"status":"asked","ask_id":"ask_abc","reasons":["r"]}`, 3)
	s = &SpawnTool{Command: strings.Fields(cmd), Session: "p"}
	out, _ = s.Run(map[string]any{"task": "t", "envelope": map[string]any{}})
	if out != "sent to the operator as ask ask_abc" {
		t.Fatal(out)
	}
}

func TestSpawnBadInputIsAnError(t *testing.T) {
	s := &SpawnTool{Command: []string{"true"}, Session: "p"}
	if _, err := s.Run(map[string]any{"task": "t"}); err == nil {
		t.Fatal("no envelope must be an error")
	}
	if _, err := s.Run(map[string]any{"task": "t", "envelope": map[string]any{}, "tokens": 1.5}); err == nil {
		t.Fatal("a fraction of a token must be an error")
	}
}

func TestCLISpawnNeedsTheTreeCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := newCLI("x").Run([]string{"--tools", "read,spawn", "x"}, strings.NewReader(""), &out, &errb)
	if code != 2 || errb.String() != "sprig: --tools: spawn needs --tree-cmd\n" {
		t.Fatalf("exit %d %q", code, errb.String())
	}
}

func TestGateSeesSpawnAsAnMCPTool(t *testing.T) {
	if gateToolName("spawn") != "mcp__sprig__spawn" {
		t.Fatal(gateToolName("spawn"))
	}
	var tools []map[string]any
	b, _ := json.Marshal(apiToolsFor([]string{"read", "spawn"}))
	json.Unmarshal(b, &tools)
	if len(tools) != 2 || tools[1]["name"] != "spawn" {
		t.Fatalf("%s", b)
	}
}
