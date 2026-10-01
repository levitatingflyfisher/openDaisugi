package sprig

import (
	"os"
	"path/filepath"
	"testing"
)

// Tree command output past 16 MiB fails the call with one line.
func TestSpawnCapsTheTreeCommandOutput(t *testing.T) {
	script := filepath.Join(t.TempDir(), "tree")
	body := "#!/bin/sh\ncat >/dev/null\nhead -c 17000000 /dev/zero\necho '{\"status\":\"ran\",\"answer\":\"x\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &SpawnTool{Command: []string{script}, Session: "p"}
	_, err := s.Run(map[string]any{"task": "t", "envelope": map[string]any{}})
	if err == nil || err.Error() != "spawn: the tree command wrote more than 16 MiB" {
		t.Fatalf("err %v", err)
	}
}
