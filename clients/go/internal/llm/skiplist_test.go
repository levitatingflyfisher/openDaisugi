package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude is a claude that records its argv and its skip variable.
func fakeClaude(t *testing.T) (bin, rec string) {
	t.Helper()
	d := t.TempDir()
	rec = filepath.Join(d, "rec")
	bin = filepath.Join(d, "claude")
	script := "#!/bin/sh\necho \"$@\" > " + rec + "\necho \"skip=$DAISUGI_PANDO_SKIP\" >> " + rec + "\necho '{\"result\":\"ok\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, rec
}

func clientWith(bin string) *Client {
	return New(Env{Getenv: func(string) (string, bool) { return "", false }, Home: "/h",
		LookPath: func(string) (string, error) { return bin, nil }})
}

// A spawner that names a skip list gets each call's session listed before the
// call runs, and the session id is the one passed to claude.
func TestSyncInEnvListsItsSessionWhenASkipListIsNamed(t *testing.T) {
	bin, rec := fakeClaude(t)
	list := filepath.Join(t.TempDir(), "pando", "skip.sessions")
	env := []string{"PATH=/usr/bin:/bin", "DAISUGI_PANDO_SKIPLIST=" + list}
	for i := 0; i < 2; i++ {
		if _, err := clientWith(bin).SyncInEnv(t.TempDir(), "hi", "haiku", 10, env); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(rec)
	b, _ := os.ReadFile(list)
	ids := strings.Fields(string(b))
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("the list holds %q", b)
	}
	if !strings.Contains(string(got), "--session-id "+ids[1]) || !strings.Contains(string(got), "skip=1") {
		t.Fatalf("claude got %q, the list %q", got, b)
	}
}

// Without a skip list the argv is the oracle's and nothing is written.
func TestSyncInEnvWithNoSkipListKeepsTheOraclesArgv(t *testing.T) {
	bin, rec := fakeClaude(t)
	if _, err := clientWith(bin).SyncInEnv(t.TempDir(), "hi", "haiku", 10, []string{"PATH=/usr/bin:/bin"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(rec)
	if strings.Contains(string(got), "--session-id") {
		t.Fatalf("argv changed: %q", got)
	}
}
