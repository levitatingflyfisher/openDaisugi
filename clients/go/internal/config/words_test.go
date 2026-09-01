package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The same rows as FORMS in tests/test_gate_hook_words.py: hand-written
// gate hooks the words must find, and commands that only look like one.
var forms = []struct{ cmd, kind, mode string }{
	{"/usr/bin/python3 -m opendaisugi.gate --mode enforce --root /x || exit 2", KindGate, "enforce"},
	{"'/opt/my env/bin/python' -m opendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"uv run --project /p python -m opendaisugi.gate --mode enforce || exit 2", KindGate, "enforce"},
	{"uv run python -m opendaisugi.gate_client --mode shadow", KindGate, "shadow"},
	{"FOO=1 python3 -m opendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"env FOO=1 python3 -m opendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"env -u X -i python3 -m opendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"python3 -I -m opendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"python3 -Im opendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"python3 -W ignore -m opendaisugi.gate --mode shadow", KindGate, "shadow"},
	{"python3 -mopendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"python3.12 -m opendaisugi.gate --mode enforce||exit 2", KindGate, "enforce"},
	{"python3 -m opendaisugi.gate --mode enforce; true", KindGate, "enforce"},
	{"cd /w && python3 -m opendaisugi.gate --mode enforce", KindGate, "enforce"},
	{"sh -c 'python3 -m opendaisugi.gate --mode enforce'", KindGate, "enforce"},
	{`bash -lc "uv run python -m opendaisugi.gate --mode enforce"`, KindGate, "enforce"},
	{"daisugi gate check --mode enforce --root /x || exit 2", KindGate, "enforce"},
	{"DAISUGI_GATE_HOOK=opendaisugi.gate FOO=1 daisugi gate check --mode enforce", KindGate, "enforce"},
	{"python3 -m opendaisugi.gate --mode 'enforce||x'", KindGate, ""},
	{"python3 -m opendaisugi.gateway-watch --mode enforce", KindNone, ""},
	{"'/opt/x --mode enforce/python' -m opendaisugi.gatex --root /r", KindNone, ""},
	{"DAISUGI_GATE_HOOK=opendaisugi.gate /bin/true gate check --mode enforce", KindNone, ""},
	{"echo -m opendaisugi.gate --mode enforce", KindUnknown, ""},
	{"python3 -c 'import x' -m opendaisugi.gate --mode enforce", KindUnknown, ""},
	{"nice python3 -m opendaisugi.gate --mode enforce", KindUnknown, ""},
	{"python3 -m opendaisugi.gate --mode enforce 'unclosed", KindUnknown, ""},
}

func TestHandWrittenForms(t *testing.T) {
	for _, f := range forms {
		if kind, mode := GateHookKind(f.cmd), GateHookMode(f.cmd); kind != f.kind || mode != f.mode {
			t.Errorf("%s: got %q %q, want %q %q", f.cmd, kind, mode, f.kind, f.mode)
		}
	}
}

func TestStatusNeverReadsAnUnknownHookAsShadow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	write := func(cmds ...string) {
		body := `{"hooks": {"PreToolUse": [{"hooks": [`
		for i, c := range cmds {
			if i > 0 {
				body += ", "
			}
			body += `{"type": "command", "command": "` + c + `"}`
		}
		if err := os.WriteFile(p, []byte(body+`]}]}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unknown := "nice python3 -m opendaisugi.gate --mode enforce"
	for _, c := range []struct {
		cmds []string
		want string
	}{
		{[]string{unknown}, KindUnknown},
		{[]string{"python3 -m opendaisugi.gate --mode shadow", unknown}, KindUnknown},
		{[]string{unknown, "daisugi gate check --mode enforce"}, "enforce"},
	} {
		write(c.cmds...)
		if got, err := InstalledHookMode(p); err != nil || got != c.want {
			t.Errorf("%v: %q %v", c.cmds, got, err)
		}
	}
}
