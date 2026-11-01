package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

var typeNames = map[fieldType]string{
	tStr:     "<class 'str'>",
	tOptStr:  "str | None",
	tInt:     "<class 'int'>",
	tOptInt:  "int | None",
	tBool:    "<class 'bool'>",
	tOptBool: "bool | None",
	tPath:    "<class 'pathlib.Path'>",
	tFloor:   "<class 'opendaisugi.config.FloorConfig'>",
	tAny:     "typing.Any",
}

// testdata/config_fields.json is written by clients/cli_cases.py from
// Config.model_fields. A field Python validates and this table lacks
// would let a bad file read as valid here, so the two must be equal.
func TestFieldsMatchPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/config_fields.json")
	if err != nil {
		t.Fatal(err)
	}
	var py struct{ Config, Floor [][2]string }
	if err := json.Unmarshal(raw, &py); err != nil {
		t.Fatal(err)
	}
	if len(py.Config) != len(Fields) {
		t.Fatalf("Python has %d Config fields, Go %d", len(py.Config), len(Fields))
	}
	for i, f := range Fields {
		if py.Config[i][0] != f.Name || py.Config[i][1] != typeNames[f.Type] {
			t.Errorf("field %d: Python %v, Go %s %s", i, py.Config[i], f.Name, typeNames[f.Type])
		}
	}
	if len(py.Floor) != len(FloorFields) {
		t.Fatalf("Python has %d FloorConfig fields, Go %d", len(py.Floor), len(FloorFields))
	}
	for i, f := range FloorFields {
		if py.Floor[i][0] != f.Name || py.Floor[i][1] != typeNames[f.Type] {
			t.Errorf("floor field %d: Python %v, Go %s %s", i, py.Floor[i], f.Name, typeNames[f.Type])
		}
	}
}

// The expected modes are what Python's resolve_warden_mode printed for the
// same files (clients/fixtures/cli, the "status cfg" cases); "?" is a
// file this package hands to Python.
func TestWardenMode(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"enforce", "warden_mode: enforce\n", "enforce"},
		{"audit", "warden_mode: audit\n", "audit"},
		{"quoted", "warden_mode: 'enforce'\n", "enforce"},
		{"dquoted", "warden_mode: \"enforce\"  # on\n", "enforce"},
		{"comment", "# my config\n\nwarden_mode: enforce # yes\nmodel: x\n", "enforce"},
		{"bogus mode", "warden_mode: strict\n", "audit"},
		{"bool mode", "warden_mode: yes\n", "audit"},
		{"int mode", "warden_mode: 5\n", "audit"},
		{"empty", "", "audit"},
		{"comments only", "# x\n\n", "audit"},
		{"list", "- a\n- b\n", "audit"},
		{"scalar", "hello\n", "audit"},
		{"bad yaml", "warden_mode: [enforce\n", "audit"},
		{"bad other field", "warden_mode: enforce\nz3_timeout_ms: lots\n", "audit"},
		{"float for int", "warden_mode: enforce\nz3_timeout_ms: 1.5\n", "audit"},
		{"null for int", "warden_mode: enforce\nz3_timeout_ms: null\n", "audit"},
		{"str for bool", "warden_mode: enforce\nauto_tend: maybe\n", "audit"},
		{"int 2 for bool", "warden_mode: enforce\nwarden_ask: 2\n", "audit"},
		{"yes bool", "warden_mode: enforce\nauto_tend: yes\nshell_allow_decomposition: off\n", "enforce"},
		{"null opt", "warden_mode: enforce\nllm_backend: null\nrouter_local_model: ~\n", "enforce"},
		{"unknown flow", "warden_mode: enforce\nnot_a_key: [1, 2]\n", "enforce"},
		{"unknown map", "warden_mode: enforce\nfoo:\n  bar: 1\n", "enforce"},
		{"floor", "warden_mode: enforce\nfloor:\n  backend: coppice\n  notify_cmd: null\n", "enforce"},
		{"floor null", "warden_mode: enforce\nfloor: null\n", "audit"},
		{"floor empty", "warden_mode: enforce\nfloor:\n", "audit"},
		{"floor int backend", "warden_mode: enforce\nfloor:\n  backend: 3\n", "audit"},
		{"dup key", "warden_mode: enforce\nwarden_mode: audit\n", "audit"},
		{"anchor", "warden_mode: &m enforce\n", "enforce"},
		{"tab", "warden_mode:\tenforce\n", "audit"},
		{"crlf", "warden_mode: enforce\r\n", "enforce"},
		{"data dir", "warden_mode: enforce\ndata_dir: /somewhere/else\n", "enforce"},
		{"key on", "on: 1\nwarden_mode: enforce\n", "enforce"},
		{"str for str", "warden_mode: enforce\nmodel: 12\n", "audit"},
		{"timestamp", "warden_mode: enforce\nmodel: 2001-12-14\n", "audit"},
		{"url", "warden_mode: enforce\nvoice_server_url: http://127.0.0.1:7477\n", "enforce"},
		{"doc marker", "---\nwarden_mode: enforce\n", "enforce"},
		{"nested under scalar", "warden_mode: enforce\n  x: 1\n", "audit"},
		{"indented top", "  warden_mode: enforce\n", "enforce"},
		{"multi plain", "warden_mode: enforce\nnotify: aaa\n  bbb\nmodel: x\n", "enforce"},
		{"floor seq", "warden_mode: enforce\nfloor:\n- a\n", "audit"},
		{"seq same indent", "warden_mode: enforce\nextra:\n- a\n- b: 1\n  c: 2\n", "enforce"},
		{"float int ok", "warden_mode: enforce\nz3_timeout_ms: 800.0\n", "enforce"},
		{"str int ok", "warden_mode: enforce\nz3_timeout_ms: ' 1_000 '\n", "enforce"},
		{"str bool ok", "warden_mode: enforce\nwarden_ask: 'T'\n", "enforce"},
		{"dquote esc", "warden_mode: \"enf\\x6frce\"\n", "enforce"},
		{"falsy top", "0\n", "audit"},
		{"false top", "false\n", "audit"},
		{"flow map", "warden_mode: enforce\nfloor: {backend: tmux, notify_cmd: null}\n", "enforce"},
		{"flow map bad", "warden_mode: enforce\nfloor: {backend: [1]}\n", "audit"},
		{"empty flow", "{}\n", "audit"},
	}
	dir := t.TempDir()
	for _, c := range cases {
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(c.text), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := WardenMode(p)
		if err != nil {
			got = "?"
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got, err := WardenMode(filepath.Join(dir, "absent.yaml")); err != nil || got != "audit" {
		t.Errorf("absent: %q %v", got, err)
	}
}

func TestInstalledHookMode(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"enforce", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.warden_client --mode enforce --root /x"}]}]}}`, "enforce"},
		{"go form", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "DAISUGI_WARDEN_HOOK=opendaisugi.warden '/o p/daisugi' warden check --mode enforce || exit 2"}]}]}}`, "enforce"},
		{"record hook", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "daisugi hook record --mode enforce"}]}]}}`, ""},
		{"foreign", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "/opt/opendaisugi.router-watch --mode enforce"}]}]}}`, ""},
		{"bad value", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.warden --mode enforced"}]}]}}`, ""},
		{"last wins", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.warden --mode enforce --mode\taudit"}]}]}}`, "audit"},
		{"quoted path", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "'/x --mode enforce/py' -m opendaisugi.warden --root /r"}]}]}}`, ""},
		{"first hook wins", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.warden --mode audit"}]}, {"hooks": [{"command": "python3 -m opendaisugi.warden --mode enforce"}]}]}}`, "audit"},
		{"no mode then mode", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.warden"}, {"command": "python3 -m opendaisugi.warden --mode=enforce"}]}]}}`, "enforce"},
		{"hooks null", `{"hooks": null}`, ""},
		{"pre empty dict", `{"hooks": {"PreToolUse": {}}}`, ""},
		{"bad json", `{nope`, ""},
		{"bom", "\xef\xbb\xbf{}", ""},
		{"list top", `[1]`, "?"},
		{"hooks list", `{"hooks": [1]}`, "?"},
		{"pre dict", `{"hooks": {"PreToolUse": {"a": 1}}}`, "?"},
		{"entry str", `{"hooks": {"PreToolUse": ["x"]}}`, "?"},
		{"command int", `{"hooks": {"PreToolUse": [{"hooks": [{"command": 5}]}]}}`, ""},
		{"command list", `{"hooks": {"PreToolUse": [{"hooks": [{"command": ["python3 -m opendaisugi.warden --mode enforce"]}]}]}}`, ""},
		{"unclosed quote", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.warden --mode enforce 'x"}]}]}}`, "unknown"},
	}
	dir := t.TempDir()
	for _, c := range cases {
		p := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(p, []byte(c.text), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := InstalledHookMode(p)
		if err != nil {
			got = "?"
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bad"), []byte{0xff, '{', '}'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := InstalledHookMode(filepath.Join(dir, "bad")); got != "" || err != nil {
		t.Errorf("invalid utf-8: %q %v", got, err)
	}
}

func TestEffectiveHookMode(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	write := func(dir, mode string) {
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.warden --mode ` + mode + `"}]}]}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(home, "audit")
	write(cwd, "enforce")
	eff, err := EffectiveHookMode(home, cwd)
	if err != nil || eff.Mode != "enforce" || SourceLabel(eff) != "project+global" {
		t.Fatalf("%+v %v", eff, err)
	}
	eff, _ = EffectiveHookMode(home, home)
	if eff.Mode != "audit" || SourceLabel(eff) != "global" || eff.CwdMode != "" {
		t.Fatalf("same file counted twice: %+v", eff)
	}
}

func TestRecordHook(t *testing.T) {
	for _, c := range []struct {
		cmd, event string
		want       bool
	}{
		{"daisugi hook record --format claude", "", true},
		{"/usr/local/bin/daisugi hook record --event stop", "stop", true},
		{"daisugi hook record --event=stop", "stop", true},
		{"daisugi hook record --event stop", "notification", false},
		{"mydaisugi hook record", "", false},
		{"echo 'daisugi hook record'", "", false},
	} {
		if got := IsRecordHook(c.cmd, c.event); got != c.want {
			t.Errorf("IsRecordHook(%q, %q) = %v", c.cmd, c.event, got)
		}
	}
}
