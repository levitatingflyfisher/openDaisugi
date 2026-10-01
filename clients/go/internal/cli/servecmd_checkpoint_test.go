package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/fenceruns"
)

// The resident checkpoint snapshots the bound workspace and nothing else,
// and reads only a transcript under ~/.claude/projects (FN-R-27, FN-R-29;
// tests/test_gate_resident_checkpoint.py is the oracle's copy).

const cpSecret = "the-secret-the-fence-denies\n"

type cpEnv struct {
	t          *testing.T
	sock, root string
	dir        string // the temp dir above the gate root
	projects   string
}

func newCpEnv(t *testing.T) *cpEnv {
	sock := startServer(t)
	root := filepath.Dir(sock)
	dir := filepath.Dir(root)
	projects := filepath.Join(dir, "home", ".claude", "projects")
	if err := os.MkdirAll(filepath.Join(projects, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := fenceruns.TranscriptRoot
	fenceruns.TranscriptRoot = func() string { return projects }
	t.Cleanup(func() { fenceruns.TranscriptRoot = old })
	return &cpEnv{t: t, sock: sock, root: root, dir: dir, projects: projects}
}

func cpGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func cpRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cpGit(t, dir, "init", "-q")
	cpGit(t, dir, "config", "user.email", "t@t")
	cpGit(t, dir, "config", "user.name", "t")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600)
	cpGit(t, dir, "add", "a.txt")
	cpGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func (c *cpEnv) prompt(path string) string {
	os.MkdirAll(filepath.Dir(path), 0o700)
	row := `{"type": "user", "uuid": "u1", "parentUuid": null, "sessionId": "s1", ` +
		`"timestamp": "2026-10-23T10:00:00Z", "message": {"role": "user", "content": "hi"}}` + "\n"
	if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
		c.t.Fatal(err)
	}
	return path
}

func (c *cpEnv) bind(ws string) {
	if err := fenceruns.Register(c.root, os.Getpid(), ws); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cpEnv) call(cwd, transcript string) string {
	pl := `{"session_id": "s1", "tool_name": "Read", "tool_input": {"file_path": "/x"}, "cwd": "` + cwd +
		`", "transcript_path": "` + transcript + `"}`
	return ask(c.t, c.sock, `{"v": 1, "argv": ["--mode", "audit", "--checkpoints"], "stdin_b64": "`+b64(pl)+`"}`+"\n", false)
}

func cpRefs(t *testing.T, repo string) []string {
	return strings.Fields(cpGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/daisugi"))
}

func cpHashed(t *testing.T, repo string) bool {
	out := cpGit(t, repo, "cat-file", "--batch-all-objects", "--batch")
	return strings.Contains(out, cpSecret)
}

func cpNames(t *testing.T, repo, ref string) []string {
	return strings.Fields(cpGit(t, repo, "ls-tree", "-r", "--name-only", ref))
}

func cpHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Checkpoints are off inside the fence (FN-R-36): the unfenced server would
// run git on config the fenced agent writes. The bound snapshot itself is
// tested in internal/gate.

func TestResidentCheckpointAFencedCallIsDenied(t *testing.T) {
	c := newCpEnv(t)
	ws := cpRepo(t, filepath.Join(c.dir, "ws"))
	c.bind(ws)
	got := c.call(ws, c.prompt(filepath.Join(c.projects, "p", "t.jsonl")))
	if !strings.Contains(got, `"exit_code": 2`) || !strings.Contains(got, "checkpoints are off inside the fence") {
		t.Fatalf("call: %q", got)
	}
	if refs := cpRefs(t, ws); len(refs) != 0 {
		t.Errorf("the unfenced server took a checkpoint for a fenced session: %v", refs)
	}
}

func TestResidentCheckpointAFencedCallWithNoCwdIsDenied(t *testing.T) {
	c := newCpEnv(t)
	ws := cpRepo(t, filepath.Join(c.dir, "ws"))
	c.bind(ws)
	if got := c.call("", ""); !strings.Contains(got, "checkpoints are off inside the fence") {
		t.Fatalf("call: %q", got)
	}
}

func TestResidentCheckpointAnUnfencedCallIsUnchanged(t *testing.T) {
	c := newCpEnv(t)
	ws := cpRepo(t, filepath.Join(c.dir, "ws"))
	got := c.call(ws, c.prompt(filepath.Join(c.projects, "p", "t.jsonl")))
	if !strings.Contains(got, `"exit_code": 2`) || !strings.Contains(got, "runs under no fence run") {
		t.Fatalf("call: %q", got)
	}
}
