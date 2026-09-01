package registry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/signing"
)

// gitEnv is a scrubbed environment: no system git config, the user
// config a file in the scratch root (the registry drops every GIT_*
// variable, so its identity and branch come from there), a fixed identity
// for the test's own git runs, and no search above the scratch root.
func gitEnv(t *testing.T, root string) []string {
	cfg := filepath.Join(root, ".gitconfig")
	body := "[init]\n\tdefaultBranch = main\n[user]\n\tname = t\n\temail = t@example.invalid\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=/usr/bin:/bin", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CEILING_DIRECTORIES=" + root, "GIT_TERMINAL_PROMPT=0"}
}

func gitOut(t *testing.T, env []string, args ...string) string {
	c := exec.Command("git", args...)
	c.Env = env
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func git(t *testing.T, env []string, args ...string) {
	c := exec.Command("git", args...)
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func pathway(t *testing.T, id string) *pathways.Pathway {
	v, err := pyjson.Loads(`{"id": "` + id + `", "task_description": "deploy", "task_embedding": [0.5, 0.0],
		"envelope": {"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": {}},
		"plan_template": {"id": "plan_00000001", "source": "script", "task": "t",
			"steps": [{"id": "s1", "type": "shell", "command": "make"}]},
		"source_trace_ids": [], "distilled_at": 1.5}`)
	if err != nil {
		t.Fatal(err)
	}
	out, verr := pmodel.Validate("CompiledPathway", pmodel.CompiledPathway, v, pmodel.Python)
	if verr != nil {
		t.Fatal(verr)
	}
	return &pathways.Pathway{Obj: out.(*pyjson.Object)}
}

// A bundle published by one clone is pulled by another only when its
// signer is in that clone's local anchor; a trusted-signers.json that
// arrives from the remote grants nothing.
func TestPublishThenPullTrustsOnlyTheLocalAnchor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	env := gitEnv(t, root)
	remote := filepath.Join(root, "remote.git")
	git(t, env, "init", "-q", "--bare", remote)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	git(t, env, "clone", "-q", remote, a)
	priv, pub, err := signing.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	pubA, err := Open(a, Options{Priv: &priv, Pub: &pub, RequireSigned: true, Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := pubA.Publish(pathway(t, "pw_1"), 1600000000.5, true)
	pubA.Close()
	if err != nil || len(hash) != 64 {
		t.Fatalf("publish: %q %v", hash, err)
	}
	git(t, env, "clone", "-q", remote, b)
	// The remote's own trust file names the signer; it must not count.
	if err := os.WriteFile(filepath.Join(b, TrustedSignersFile), []byte(`{"x": "`+pub+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(b, Options{RequireSigned: true, Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Pull(); err != nil || n != 0 {
		t.Fatalf("pull with no local anchor: %d %v", n, err)
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(b, ".cache", TrustedSignersFile), []byte(`{"a": "`+pub+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(b, Options{RequireSigned: true, Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Value("cached_pathways") != 1 || st.Value("trusted_signers") != 1 || st.Value("bundle_files") != 1 {
		t.Fatalf("status %s", pyjson.Dumps(st, true))
	}
}

// An inherited GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE or GIT_CONFIG_*
// does not reach git: status reads the clone, publish commits to it, and
// the decoy and its hooks are left alone.
func TestGitIgnoresAnInheritedGitEnvironment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	env := gitEnv(t, root)
	remote := filepath.Join(root, "remote.git")
	git(t, env, "init", "-q", "--bare", remote)
	a, decoy := filepath.Join(root, "a"), filepath.Join(root, "decoy")
	git(t, env, "clone", "-q", remote, a)
	git(t, env, "-C", a, "commit", "-q", "--allow-empty", "-m", "first")
	git(t, env, "init", "-q", decoy)
	git(t, env, "-C", decoy, "commit", "-q", "--allow-empty", "-m", "decoy")
	head, decoyHead := gitOut(t, env, "-C", a, "rev-parse", "HEAD"), gitOut(t, env, "-C", decoy, "rev-parse", "HEAD")
	hooks := filepath.Join(root, "hooks")
	marker := filepath.Join(root, "hook-ran")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := append(append([]string{}, env...), "GIT_DIR="+filepath.Join(decoy, ".git"), "GIT_WORK_TREE="+decoy,
		"GIT_INDEX_FILE="+filepath.Join(root, "decoy-index"), "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+hooks)
	priv, pub, err := signing.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(a, Options{Priv: &priv, Pub: &pub, RequireSigned: true, Environ: hostile})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Value("head_commit") != head {
		t.Fatalf("status read %v, the clone is at %s", st.Value("head_commit"), head)
	}
	if _, err := s.Publish(pathway(t, "pw_1"), 1600000000.5, false); err != nil {
		t.Fatal(err)
	}
	if gitOut(t, env, "-C", decoy, "rev-parse", "HEAD") != decoyHead {
		t.Fatal("the decoy moved")
	}
	if gitOut(t, env, "-C", a, "rev-parse", "HEAD") == head {
		t.Fatal("the clone did not get the commit")
	}
	for _, p := range []string{marker, filepath.Join(root, "decoy-index")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists", p)
		}
	}
}

// A repo path with no .git of its own inside another repository is not a
// registry: git does not climb to the repository above it.
func TestGitDoesNotClimbIntoAParentRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	env := gitEnv(t, root)
	outer := filepath.Join(root, "outer")
	git(t, env, "init", "-q", outer)
	git(t, env, "-C", outer, "commit", "-q", "--allow-empty", "-m", "first")
	head := gitOut(t, env, "-C", outer, "rev-parse", "HEAD")
	inner := filepath.Join(outer, "reg")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	// No ceiling of the test's own: only the registry's may stop git.
	var free []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_CEILING_DIRECTORIES=") {
			free = append(free, kv)
		}
	}
	priv, pub, err := signing.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(inner, Options{Priv: &priv, Pub: &pub, RequireSigned: true, Environ: free})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Value("head_commit") != "" {
		t.Fatalf("status climbed to %v", st.Value("head_commit"))
	}
	if _, err := s.Publish(pathway(t, "pw_1"), 1600000000.5, false); err == nil {
		t.Fatal("publish outside a clone succeeded")
	}
	if gitOut(t, env, "-C", outer, "rev-parse", "HEAD") != head {
		t.Fatal("the outer repository moved")
	}
}
