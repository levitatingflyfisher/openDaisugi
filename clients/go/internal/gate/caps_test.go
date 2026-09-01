package gate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChildDeadline(t *testing.T) {
	for argv, want := range map[string]float64{
		"":                         13,
		"--verify-timeout 5":       8,
		"--verify-timeout 5 --ask": 98,
		"--verify-timeout 5 --ask --ask-timeout 20": 28,
		"--verify-timeout 0.1":                      4,
		"--verify-timeout nan":                      3603,
		"--nope":                                    13,
	} {
		if got := ChildDeadline(strings.Fields(argv)).Seconds(); got != want {
			t.Errorf("%q: %v, want %v", argv, got, want)
		}
	}
}

// The caps of tests/test_gate_size_caps.py, with the oracle's reasons.
func TestSizeCaps(t *testing.T) {
	root, env := setup(t)
	envelope := `{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["echo"],"file_read":["/work/**"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(envelope), 0o600); err != nil {
		t.Fatal(err)
	}
	big, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Read", "cwd": "/work",
		"tool_input": map[string]any{"file_path": "/work/a", "pad": strings.Repeat("x", MaxPayloadBytes)}})
	res := run(t, root, env, "enforce", string(big))
	want := fmt.Sprintf("openDaisugi gate: DENIED — hook payload is larger than %d bytes; the gate does not read it\n", MaxPayloadBytes)
	if res.Exit != 2 || res.Stderr != want {
		t.Errorf("payload: %d %q", res.Exit, res.Stderr)
	}
	for _, c := range []struct {
		n      int
		denied bool
	}{{MaxShellCommandChars, false}, {MaxShellCommandChars + 1, true}} {
		p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
			"tool_input": map[string]any{"command": "echo " + strings.Repeat("a", c.n-5)}})
		res := run(t, root, env, "enforce", string(p))
		want := fmt.Sprintf("openDaisugi gate: DENIED — shell command is longer than %d characters; the gate does not read it\n",
			MaxShellCommandChars)
		if (res.Stderr == want) != c.denied || (res.Exit == 2) != c.denied {
			t.Errorf("%d: %d %.200q", c.n, res.Exit, res.Stderr)
		}
	}
}
