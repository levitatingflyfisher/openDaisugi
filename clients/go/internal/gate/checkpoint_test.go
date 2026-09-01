package gate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A call allowed at a new prompt commits the work tree to a private ref,
// once: the same prompt again takes no second checkpoint.
func TestCheckpointsTakeOneSnapshotPerPrompt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root, env := setup(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type": "user", "uuid": "u1", "message": {"content": "hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": "/work",
		"transcript_path": tr, "tool_input": map[string]any{"file_path": "/work/a"}})
	payload = []byte(strings.Replace(string(payload), `"cwd":"/work"`, `"cwd":"`+repo+`"`, 1))
	for i := 0; i < 2; i++ {
		res := Run([]string{"--mode", "enforce", "--root", root, "--checkpoints"}, payload, env)
		if !res.Native || res.Exit != 0 {
			t.Fatalf("got %+v", res)
		}
	}
	refs, _ := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/daisugi").Output()
	if n := len(strings.Fields(string(refs))); n != 1 {
		t.Fatalf("want one checkpoint ref, got %q", refs)
	}
	tree, _ := os.ReadFile(filepath.Join(filepath.Dir(root), "sessions", "s1.jsonl"))
	if strings.Count(string(tree), `"type": "checkpoint"`) != 1 || !strings.Contains(string(tree), `"promptUuid": "u1"`) {
		t.Fatalf("tree: %s", tree)
	}
}

// The gate calls git in the AGENT's own workspace repo under --checkpoints.
// That repo's local .git/config is agent-controlled, so
// `git config core.fsmonitor "..."` would make the next checkpoint run a
// program the agent chose, not git's own, unless every git call this
// port makes overrides it back on the command line. Mirrors
// tests/test_checkpoints.py::test_snapshot_does_not_run_the_repos_own_fsmonitor.
func TestCheckpointsDoNotRunTheReposOwnFsmonitor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root, env := setup(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	if out, err := exec.Command("git", "-C", repo, "config", "core.fsmonitor",
		"sh -c 'touch "+marker+"'").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type": "user", "uuid": "u1", "message": {"content": "hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "cwd": "/work",
		"transcript_path": tr, "tool_input": map[string]any{"file_path": "/work/a"}})
	payload = []byte(strings.Replace(string(payload), `"cwd":"/work"`, `"cwd":"`+repo+`"`, 1))
	res := Run([]string{"--mode", "enforce", "--root", root, "--checkpoints"}, payload, env)
	if !res.Native || res.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repo's own core.fsmonitor ran during the checkpoint")
	}
	refs, _ := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/daisugi").Output()
	if n := len(strings.Fields(string(refs))); n != 1 {
		t.Fatalf("want one checkpoint ref (the checkpoint must still have worked), got %q", refs)
	}
}
