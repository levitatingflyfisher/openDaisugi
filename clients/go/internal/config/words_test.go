package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The same rows as FORMS in tests/test_warden_hook_words.py: hand-written
// warden hooks the words must find, and commands that only look like one.
var forms = []struct{ cmd, kind, mode string }{
	{"/usr/bin/python3 -m opendaisugi.warden --mode enforce --root /x || exit 2", KindWarden, "enforce"},
	{"'/opt/my env/bin/python' -m opendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"uv run --project /p python -m opendaisugi.warden --mode enforce || exit 2", KindWarden, "enforce"},
	{"uv run python -m opendaisugi.warden_client --mode audit", KindWarden, "audit"},
	{"FOO=1 python3 -m opendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"env FOO=1 python3 -m opendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"env -u X -i python3 -m opendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"python3 -I -m opendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"python3 -Im opendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"python3 -W ignore -m opendaisugi.warden --mode audit", KindWarden, "audit"},
	{"python3 -mopendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"python3.12 -m opendaisugi.warden --mode enforce||exit 2", KindWarden, "enforce"},
	{"python3 -m opendaisugi.warden --mode enforce; true", KindWarden, "enforce"},
	{"cd /w && python3 -m opendaisugi.warden --mode enforce", KindWarden, "enforce"},
	{"sh -c 'python3 -m opendaisugi.warden --mode enforce'", KindWarden, "enforce"},
	{`bash -lc "uv run python -m opendaisugi.warden --mode enforce"`, KindWarden, "enforce"},
	{"daisugi warden check --mode enforce --root /x || exit 2", KindWarden, "enforce"},
	{"uv run --project /p daisugi warden check --mode enforce", KindWarden, "enforce"},
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden FOO=1 daisugi warden check --mode enforce", KindWarden, "enforce"},
	{"python3 -m opendaisugi.warden --mode 'enforce||x'", KindWarden, ""},
	{"python3 -m opendaisugi.router-watch --mode enforce", KindNone, ""},
	{"'/opt/x --mode enforce/python' -m opendaisugi.gatex --root /r", KindNone, ""},
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden /bin/true warden check --mode enforce", KindNone, ""},
	{"echo -m opendaisugi.warden --mode enforce", KindUnknown, ""},
	{"python3 -c 'import x' -m opendaisugi.warden --mode enforce", KindUnknown, ""},
	{"nice python3 -m opendaisugi.warden --mode enforce", KindUnknown, ""},
	{"python3 -m opendaisugi.warden --mode enforce 'unclosed", KindUnknown, ""},
}

func TestHandWrittenForms(t *testing.T) {
	for _, f := range forms {
		if kind, mode := WardenHookKind(f.cmd), WardenHookMode(f.cmd); kind != f.kind || mode != f.mode {
			t.Errorf("%s: got %q %q, want %q %q", f.cmd, kind, mode, f.kind, f.mode)
		}
	}
}

func TestStatusNeverReadsAnUnknownHookAsAudit(t *testing.T) {
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
	unknown := "nice python3 -m opendaisugi.warden --mode enforce"
	for _, c := range []struct {
		cmds []string
		want string
	}{
		{[]string{unknown}, KindUnknown},
		{[]string{"python3 -m opendaisugi.warden --mode audit", unknown}, KindUnknown},
		{[]string{unknown, "daisugi warden check --mode enforce"}, "enforce"},
	} {
		write(c.cmds...)
		if got, err := InstalledHookMode(p); err != nil || got != c.want {
			t.Errorf("%v: %q %v", c.cmds, got, err)
		}
	}
}

// The same rows as RENAMED in tests/test_warden_hook_words.py: a binary
// installed under another file name writes a hook the reader knows by its
// shape, not by the program's name.
var renamed = []struct{ cmd, kind, mode string }{
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/x/rs-daisugi warden check --mode enforce --root /x --format claude --verify-timeout 10.0 || exit 2", KindWarden, "enforce"},
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden '/opt/my tools/dsg' warden check --mode audit --root /x --format codex --verify-timeout 10.0 --session s1", KindWarden, "audit"},
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden FOO=1 /opt/x/go-daisugi warden check --mode enforce --root /x --format claude --verify-timeout 5.0", KindWarden, "enforce"},
	{"/opt/x/rs-daisugi warden check --mode enforce --root /x --format claude --verify-timeout 10.0", KindNone, ""},
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/x/rs-daisugi warden check --mode enforce", KindNone, ""},
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/x/rs-daisugi warden check --mode enforce --root /x --format claude", KindNone, ""},
	{"DAISUGI_WARDEN_HOOK=opendaisugi.warden /opt/x/rs-daisugi warden status --mode enforce --root /x --format claude --verify-timeout 10.0", KindNone, ""},
	{"/opt/x/rs-daisugi DAISUGI_WARDEN_HOOK=opendaisugi.warden warden check --mode enforce --root /x --format claude --verify-timeout 10.0", KindNone, ""},
	{"DAISUGI_WARDEN_HOOK=other /opt/x/rs-daisugi warden check --mode enforce --root /x --format claude --verify-timeout 10.0", KindNone, ""},
}

func TestRenamedBinaryHookIsTheWarden(t *testing.T) {
	for _, f := range renamed {
		if kind, mode := WardenHookKind(f.cmd), WardenHookMode(f.cmd); kind != f.kind || mode != f.mode {
			t.Errorf("%s: got %q %q, want %q %q", f.cmd, kind, mode, f.kind, f.mode)
		}
	}
	if got := WardenHookProgram(renamed[0].cmd); got != "/opt/x/rs-daisugi" {
		t.Errorf("program %q", got)
	}
	if got := WardenHookProgram("/opt/x/rs-daisugi warden check --mode enforce"); got != "" {
		t.Errorf("program %q", got)
	}
}
