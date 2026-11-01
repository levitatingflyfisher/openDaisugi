package warden

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordCallKeepsPiLowercaseBash(t *testing.T) {
	root := t.TempDir()
	r := &runner{env: map[string]string{}, fmt: "pi"}
	r.recordCall([]byte(`{"tool_name":"bash","tool_input":{"command":"ls"},"session_id":"s"}`), root)
	b, err := os.ReadFile(filepath.Join(root, "s.jsonl"))
	if err != nil {
		t.Fatalf("no capture written: %v", err)
	}
	if !strings.Contains(string(b), `"step_type": "shell"`) {
		t.Fatalf("record = %s", b)
	}
}

func TestClassifyHermesAndOpenClaw(t *testing.T) {
	for _, c := range []struct{ fmt, name, want string }{
		{"hermes", "terminal", "shell"}, {"hermes", "read_file", "file_read"},
		{"hermes", "patch", "file_write"}, {"hermes", "web_extract", "network"},
		{"hermes", "vision_analyze", ""}, {"claude", "terminal", ""},
		{"openclaw", "exec", "shell"}, {"openclaw", "apply_patch", "file_write"},
		{"openclaw", "web_fetch", "network"}, {"openclaw", "message", ""},
	} {
		if got := classifyTool(c.name, c.fmt); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.fmt, c.name, got, c.want)
		}
	}
}

func TestOpenClawApplyPatchNamesItsFirstFile(t *testing.T) {
	r := &runner{env: map[string]string{}, fmt: "openclaw"}
	raw := `{"tool":"apply_patch","args":{"patch":"*** Begin Patch\n*** Add File: /w/new.txt\n+hi\n*** End Patch"},"session_id":"o"}`
	root := t.TempDir()
	r.recordCall([]byte(raw), root)
	b, err := os.ReadFile(filepath.Join(root, "o.jsonl"))
	if err != nil || !strings.Contains(string(b), `"path": "/w/new.txt"`) {
		t.Fatalf("record = %s (%v)", b, err)
	}
}

func TestHermesWebExtractTakesTheFirstOfUrls(t *testing.T) {
	r := &runner{env: map[string]string{}, fmt: "hermes"}
	root := t.TempDir()
	r.recordCall([]byte(`{"tool_name":"web_extract","tool_input":{"urls":["https://c/d"]},"session_id":"h"}`), root)
	b, _ := os.ReadFile(filepath.Join(root, "h.jsonl"))
	if !strings.Contains(string(b), `"url": "https://c/d"`) {
		t.Fatalf("record = %s", b)
	}
}
