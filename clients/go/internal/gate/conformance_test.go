package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With recording on, a verified call leaves its verify case, and the
// case id is the content address of the rest of the body.
func TestRecordingWritesTheVerifyCase(t *testing.T) {
	root, env := setup(t)
	rec := t.TempDir()
	env = append(env, recordEnv+"="+rec)
	res := run(t, root, env, "enforce", `{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}`)
	if !res.Native || res.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
	files, _ := filepath.Glob(filepath.Join(rec, "cases-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, `{"envelope":{`) || !strings.Contains(line, `"kind":"verify"`) ||
		!strings.Contains(line, `"plan":{"id":"plan_case","source":"call-time-gate"`) {
		t.Fatalf("line: %s", line)
	}
}
