package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The client refuses every case, so a found client turns an allow into a
// deny.
const refuser = `#!/usr/bin/python3
import json, sys
for line in sys.stdin:
    print(json.dumps({"id": json.loads(line)["id"], "ok": False, "violations": [{"stage": "z3", "step": None}]}), flush=True)
`

// The gate finds a compiled client through OPENDAISUGI_<NAME>_CLIENT or
// as daisugi-conform-<name> on PATH, and never beside its own binary, as
// bench.options.gate_client_argv does.
func TestGateFindsClientByVariableOrPath(t *testing.T) {
	payload := `{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a.txt"},"cwd":"/work"}`
	bin := t.TempDir()
	client := filepath.Join(bin, "daisugi-conform-go")
	if err := os.WriteFile(client, []byte(refuser), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		extra  []string
		denied bool
	}{
		{"nothing", nil, false},
		{"path", []string{"PATH=" + bin + ":/usr/bin:/bin"}, true},
		{"variable", []string{"OPENDAISUGI_GO_CLIENT=" + client}, true},
		{"variable names no file", []string{"OPENDAISUGI_GO_CLIENT=" + client + ".none"}, false},
	} {
		root, env := setup(t)
		if err := os.WriteFile(filepath.Join(filepath.Dir(root), "config.yaml"), []byte("verifier_client: go\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env = append(env, c.extra...)
		res := run(t, root, env, "enforce", payload)
		if !res.Native || (res.Exit == 2) != c.denied {
			t.Errorf("%s: %+v", c.name, res)
		}
		log, _ := os.ReadFile(filepath.Join(root, "verifier", "last_dispatch.json"))
		if !strings.Contains(string(log), map[bool]string{false: "not built", true: `"ok": true`}[c.denied]) {
			t.Errorf("%s: dispatch log %s", c.name, log)
		}
	}
}
