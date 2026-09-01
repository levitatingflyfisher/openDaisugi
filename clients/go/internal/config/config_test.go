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

// The expected modes are what Python's resolve_gate_mode printed for the
// same files (clients/fixtures/cli, the "status cfg" cases); "?" is a
// file this package hands to Python.
func TestGateMode(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"enforce", "gate_mode: enforce\n", "enforce"},
		{"shadow", "gate_mode: shadow\n", "shadow"},
		{"quoted", "gate_mode: 'enforce'\n", "enforce"},
		{"dquoted", "gate_mode: \"enforce\"  # on\n", "enforce"},
		{"comment", "# my config\n\ngate_mode: enforce # yes\nmodel: x\n", "enforce"},
		{"bogus mode", "gate_mode: strict\n", "shadow"},
		{"bool mode", "gate_mode: yes\n", "shadow"},
		{"int mode", "gate_mode: 5\n", "shadow"},
		{"empty", "", "shadow"},
		{"comments only", "# x\n\n", "shadow"},
		{"list", "- a\n- b\n", "shadow"},
		{"scalar", "hello\n", "shadow"},
		{"bad yaml", "gate_mode: [enforce\n", "?"},
		{"bad other field", "gate_mode: enforce\nz3_timeout_ms: lots\n", "shadow"},
		{"float for int", "gate_mode: enforce\nz3_timeout_ms: 1.5\n", "shadow"},
		{"null for int", "gate_mode: enforce\nz3_timeout_ms: null\n", "shadow"},
		{"str for bool", "gate_mode: enforce\nauto_tend: maybe\n", "shadow"},
		{"int 2 for bool", "gate_mode: enforce\ngate_ask: 2\n", "shadow"},
		{"yes bool", "gate_mode: enforce\nauto_tend: yes\nshell_allow_decomposition: off\n", "enforce"},
		{"null opt", "gate_mode: enforce\nllm_backend: null\ngateway_local_model: ~\n", "enforce"},
		{"unknown flow", "gate_mode: enforce\nnot_a_key: [1, 2]\n", "enforce"},
		{"unknown map", "gate_mode: enforce\nfoo:\n  bar: 1\n", "enforce"},
		{"floor", "gate_mode: enforce\nfloor:\n  backend: coppice\n  notify_cmd: null\n", "enforce"},
		{"floor null", "gate_mode: enforce\nfloor: null\n", "shadow"},
		{"floor empty", "gate_mode: enforce\nfloor:\n", "shadow"},
		{"floor int backend", "gate_mode: enforce\nfloor:\n  backend: 3\n", "shadow"},
		{"dup key", "gate_mode: enforce\ngate_mode: shadow\n", "shadow"},
		{"anchor", "gate_mode: &m enforce\n", "?"},
		{"tab", "gate_mode:\tenforce\n", "?"},
		{"crlf", "gate_mode: enforce\r\n", "?"},
		{"data dir", "gate_mode: enforce\ndata_dir: /somewhere/else\n", "enforce"},
		{"key on", "on: 1\ngate_mode: enforce\n", "enforce"},
		{"str for str", "gate_mode: enforce\nmodel: 12\n", "shadow"},
		{"timestamp", "gate_mode: enforce\nmodel: 2001-12-14\n", "?"},
		{"url", "gate_mode: enforce\nvoice_server_url: http://127.0.0.1:7477\n", "enforce"},
		{"doc marker", "---\ngate_mode: enforce\n", "enforce"},
		{"nested under scalar", "gate_mode: enforce\n  x: 1\n", "?"},
		{"indented top", "  gate_mode: enforce\n", "enforce"},
		{"multi plain", "gate_mode: enforce\nnotify: aaa\n  bbb\nmodel: x\n", "enforce"},
		{"floor seq", "gate_mode: enforce\nfloor:\n- a\n", "shadow"},
		{"seq same indent", "gate_mode: enforce\nextra:\n- a\n- b: 1\n  c: 2\n", "enforce"},
		{"float int ok", "gate_mode: enforce\nz3_timeout_ms: 800.0\n", "enforce"},
		{"str int ok", "gate_mode: enforce\nz3_timeout_ms: ' 1_000 '\n", "enforce"},
		{"str bool ok", "gate_mode: enforce\ngate_ask: 'T'\n", "enforce"},
		{"dquote esc", "gate_mode: \"enf\\x6frce\"\n", "enforce"},
		{"falsy top", "0\n", "shadow"},
		{"false top", "false\n", "shadow"},
		{"flow map", "gate_mode: enforce\nfloor: {backend: tmux, notify_cmd: null}\n", "enforce"},
		{"flow map bad", "gate_mode: enforce\nfloor: {backend: [1]}\n", "shadow"},
		{"empty flow", "{}\n", "shadow"},
	}
	dir := t.TempDir()
	for _, c := range cases {
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(c.text), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := GateMode(p)
		if err != nil {
			got = "?"
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got, err := GateMode(filepath.Join(dir, "absent.yaml")); err != nil || got != "shadow" {
		t.Errorf("absent: %q %v", got, err)
	}
}

func TestInstalledHookMode(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"enforce", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.gate_client --mode enforce --root /x"}]}]}}`, "enforce"},
		{"go form", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "DAISUGI_GATE_HOOK=opendaisugi.gate '/o p/daisugi' gate check --mode enforce || exit 2"}]}]}}`, "enforce"},
		{"record hook", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "daisugi hook record --mode enforce"}]}]}}`, ""},
		{"foreign", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "/opt/opendaisugi.gateway-watch --mode enforce"}]}]}}`, ""},
		{"bad value", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.gate --mode enforced"}]}]}}`, ""},
		{"last wins", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.gate --mode enforce --mode\tshadow"}]}]}}`, "shadow"},
		{"quoted path", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "'/x --mode enforce/py' -m opendaisugi.gate --root /r"}]}]}}`, ""},
		{"first hook wins", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.gate --mode shadow"}]}, {"hooks": [{"command": "python3 -m opendaisugi.gate --mode enforce"}]}]}}`, "shadow"},
		{"no mode then mode", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.gate"}, {"command": "python3 -m opendaisugi.gate --mode=enforce"}]}]}}`, "enforce"},
		{"hooks null", `{"hooks": null}`, ""},
		{"pre empty dict", `{"hooks": {"PreToolUse": {}}}`, ""},
		{"bad json", `{nope`, ""},
		{"bom", "\xef\xbb\xbf{}", ""},
		{"list top", `[1]`, "?"},
		{"hooks list", `{"hooks": [1]}`, "?"},
		{"pre dict", `{"hooks": {"PreToolUse": {"a": 1}}}`, "?"},
		{"entry str", `{"hooks": {"PreToolUse": ["x"]}}`, "?"},
		{"command int", `{"hooks": {"PreToolUse": [{"hooks": [{"command": 5}]}]}}`, ""},
		{"command list", `{"hooks": {"PreToolUse": [{"hooks": [{"command": ["python3 -m opendaisugi.gate --mode enforce"]}]}]}}`, ""},
		{"unclosed quote", `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.gate --mode enforce 'x"}]}]}}`, "unknown"},
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
		body := `{"hooks": {"PreToolUse": [{"hooks": [{"command": "python3 -m opendaisugi.gate --mode ` + mode + `"}]}]}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(home, "shadow")
	write(cwd, "enforce")
	eff, err := EffectiveHookMode(home, cwd)
	if err != nil || eff.Mode != "enforce" || SourceLabel(eff) != "project+global" {
		t.Fatalf("%+v %v", eff, err)
	}
	eff, _ = EffectiveHookMode(home, home)
	if eff.Mode != "shadow" || SourceLabel(eff) != "global" || eff.CwdMode != "" {
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
