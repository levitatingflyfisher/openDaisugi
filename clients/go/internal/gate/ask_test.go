package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An operator answers the posted ask with the ask's own nonce, as
// ask.answer does; the gate honors it and carries the edit to Claude Code.
func TestAnAnsweredAskAllowsWithTheOperatorsEdit(t *testing.T) {
	root, env := setup(t)
	if err := os.WriteFile(filepath.Join(root, "operator.json"), []byte(`{"pid": 1, "at": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		ask := filepath.Join(root, "asks", "tu-1.json")
		for i := 0; i < 200; i++ {
			if raw, err := os.ReadFile(ask); err == nil {
				var body map[string]any
				if json.Unmarshal(raw, &body) == nil {
					answer, _ := json.Marshal(map[string]any{"toolUseId": "tu-1", "decision": "allow", "reason": "ok",
						"updatedInput": map[string]any{"command": "ls"}, "nonce": body["nonce"], "by": "ana", "whoFrom": "token"})
					_ = os.MkdirAll(filepath.Join(root, "answers"), 0o700)
					_ = os.WriteFile(filepath.Join(root, "answers", "tu-1.json"), answer, 0o600)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	res := Run([]string{"--mode", "enforce", "--root", root, "--ask", "--ask-timeout", "5"},
		[]byte(`{"session_id":"s1","tool_name":"Bash","tool_use_id":"tu-1","tool_input":{"command":"rm -rf /work/x"},"cwd":"/work"}`), env)
	want := `{"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "allow", "permissionDecisionReason": "allowed by operator: ok", "updatedInput": {"command": "ls"}}}` + "\n"
	if !res.Native || res.Exit != 0 || res.Stdout != want {
		t.Fatalf("got %+v", res)
	}
	log, _ := os.ReadFile(filepath.Join(root, "shadow", "s1.jsonl"))
	if !strings.Contains(string(log), `"ask": true`) || !strings.Contains(string(log), `"allowed_by": "ana", "who_from": "token"`) {
		t.Fatalf("shadow: %s", log)
	}
	for _, sub := range []string{"asks", "answers"} {
		if ents, _ := os.ReadDir(filepath.Join(root, sub)); len(ents) != 0 {
			t.Errorf("%s left behind: %v", sub, ents)
		}
	}
}

// A Herdr pane reports through the herdr command found on PATH.
func TestAHerdrPaneReportsThroughHerdr(t *testing.T) {
	root, env := setup(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	script := "#!/bin/sh\nprintf '%s ' \"$@\" > " + log + "\n"
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env = append(env[:1], "PATH="+bin+":/usr/bin:/bin", "HERDR_PANE_ID=h1")
	res := run(t, root, env, "enforce", `{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}`)
	if !res.Native || res.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
	got, _ := os.ReadFile(log)
	if string(got) != "pane report-agent --source daisugi --agent claude-code --state working -- h1 " {
		t.Fatalf("herdr got %q", got)
	}
}
