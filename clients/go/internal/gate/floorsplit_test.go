package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const floorAllowAll = `{"generated_by":"t","task":"t","permissions":{"shell":true,
 "shell_allowlist":["cd","echo","ls","cp"],"shell_allow_decomposition":true,
 "file_read":["/**"],"file_write":["/**"]}}`

func floorCall(t *testing.T, root string, env []string, cmd string) Result {
	t.Helper()
	p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
		"tool_input": map[string]string{"command": cmd}})
	return run(t, root, env, "enforce", string(p))
}

// 1234e6db: a command whose words cannot be split leaves the cwd unknown,
// and the commands after it are still checked.
func TestFloorWordSplitErrorDoesNotEndTheCheck(t *testing.T) {
	root, env := setup(t)
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(floorAllowAll), 0o600); err != nil {
		t.Fatal(err)
	}
	res := floorCall(t, root, env, `cd /home/user/.config ; echo $'a\'b' ; cd coppice && echo x > coppice.toml`)
	if !res.Native || !strings.Contains(res.Stderr, floorRefusal) {
		t.Fatalf("got %+v", res)
	}
}

// 1234e6db: a line deep enough to reach the recursion limit inside the
// floor rule is a hit. Every chain length near the limit denies, whether
// the floor rule or the verifier is the one that cannot split it.
func TestFloorChainNearTheRecursionLimitDenies(t *testing.T) {
	root, env := setup(t)
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(floorAllowAll), 0o600); err != nil {
		t.Fatal(err)
	}
	for n := 940; n <= 1010; n++ {
		cmd := "cd /home/user/.config" + strings.Repeat(" && ls", n) + " && cd coppice && echo x > coppice.toml"
		res := floorCall(t, root, env, cmd)
		if !res.Native || res.Exit != 2 {
			t.Errorf("%d links: %+v", n, res)
		}
	}
}
