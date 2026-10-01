package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// answerAsk answers the ask for tu-1 once it is posted, as ask.answer
// does, with the given edit.
func answerAsk(t *testing.T, root string, edit any) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "operator.json"), []byte(`{"pid": 1, "at": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		ask := filepath.Join(root, "asks", "tu-1.json")
		for i := 0; i < 300; i++ {
			if raw, err := os.ReadFile(ask); err == nil {
				var body map[string]any
				if json.Unmarshal(raw, &body) == nil {
					answer, _ := json.Marshal(map[string]any{"toolUseId": "tu-1", "decision": "allow", "reason": "ok",
						"updatedInput": edit, "nonce": body["nonce"], "by": "ana", "whoFrom": "token"})
					_ = os.MkdirAll(filepath.Join(root, "answers"), 0o700)
					_ = os.WriteFile(filepath.Join(root, "answers", "tu-1.json"), answer, 0o600)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func askRun(root string, env []string) Result {
	return Run([]string{"--mode", "enforce", "--root", root, "--ask", "--ask-timeout", "5"},
		[]byte(`{"session_id":"s1","tool_name":"Bash","tool_use_id":"tu-1","tool_input":{"command":"rm -rf /work/x"},"cwd":"/work"}`), env)
}

// An operator's edit is a new call: it passes the whole gate again, and
// a deny there stands with no second ask.
func TestAnOperatorEditPassesTheGateAgain(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   any
		reason string
	}{
		{"floor", map[string]any{"command": "rm -rf /home/user/.config/coppice"},
			"the operator's edit does not pass the gate: this is the floor's own config. Edit it yourself."},
		{"outside", map[string]any{"command": "rm -rf /tmp/x"}, "the operator's edit does not pass the gate: "},
		{"not an object", "ls", "the operator's edit does not pass the gate: the edit is not a JSON object"},
	} {
		root, env := setup(t)
		answerAsk(t, root, c.edit)
		res := askRun(root, env)
		if !res.Native || res.Exit != 2 || !strings.Contains(res.Stderr, c.reason) {
			t.Fatalf("%s: got %+v", c.name, res)
		}
		log, _ := os.ReadFile(filepath.Join(root, "audit", "s1.jsonl"))
		if !strings.Contains(string(log), `"ask": true`) || !strings.Contains(string(log), `"denied_by": "ana"`) {
			t.Fatalf("%s: audit %s", c.name, log)
		}
	}
}

// A payload's session id never selects an envelope; a pin with no
// envelope denies and never reads default.
func TestAPayloadSessionIDNeverSelectsAnEnvelope(t *testing.T) {
	root, env := setup(t)
	wide := strings.Replace(testEnvelope, `["ls", "git"]`, `["ls", "git", "rm"]`, 1)
	if err := os.WriteFile(filepath.Join(root, "envelopes", "victim.json"), []byte(wide), 0o600); err != nil {
		t.Fatal(err)
	}
	call := `{"session_id":"victim","tool_name":"Bash","tool_input":{"command":"rm x"},"cwd":"/work"}`
	if res := run(t, root, env, "enforce", call); res.Exit != 2 {
		t.Fatalf("unpinned, the payload's id selected an envelope: %+v", res)
	}
	pinned := func(pin string) Result {
		return Run([]string{"--mode", "enforce", "--root", root, "--session", pin}, []byte(call), env)
	}
	if res := pinned("victim"); res.Exit != 0 {
		t.Fatalf("a pin must select its own envelope: %+v", res)
	}
	ls := strings.Replace(call, "rm x", "ls", 1)
	res := Run([]string{"--mode", "enforce", "--root", root, "--session", "child"}, []byte(ls), env)
	if res.Exit != 2 || !strings.Contains(res.Stderr, "no envelope registered") {
		t.Fatalf("a pin with no envelope must deny: %+v", res)
	}
}

// daisugi rank record is a hard deny, before the envelope.
func TestRankRecordIsAHardDeny(t *testing.T) {
	root, env := setup(t)
	all := strings.Replace(testEnvelope, `["ls", "git"]`, `["ls", "git", "daisugi"]`, 1)
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(all), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"enforce", "audit"} {
		res := run(t, root, env, mode, `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"daisugi rank record --judge owner"},"cwd":"/work"}`)
		if !res.Native || res.Exit != 2 || !strings.Contains(res.Stderr, rankRefusal) {
			t.Fatalf("%s: got %+v", mode, res)
		}
	}
	res := run(t, root, env, "enforce", `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"daisugi rank list"},"cwd":"/work"}`)
	if res.Exit != 0 {
		t.Fatalf("a near miss must pass: %+v", res)
	}
	for _, line := range []string{"/usr/bin/daisugi rank record", "uv run python -m opendaisugi rank record",
		"sh -c 'daisugi rank record'", "dai\\sugi rank rec\\ord", "daisugi --x /d rank --y record"} {
		if !(&runner{}).runsVerb(line, rankVerbs) {
			t.Errorf("miss: %q", line)
		}
	}
	for _, line := range []string{"daisugi rank list", "daisugi-helper rank record", "daisugi rank recorder",
		"grep 'rank record' daisugi.log", "daisugi\u00a0rank record", "daisugi status && rank record"} {
		if (&runner{}).runsVerb(line, rankVerbs) {
			t.Errorf("false hit: %q", line)
		}
	}
}
