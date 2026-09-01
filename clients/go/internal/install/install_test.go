package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// The embedded extensions must be the files Python ships.
func TestAssetsMatchPython(t *testing.T) {
	for path, got := range map[string]string{
		"../../../../src/opendaisugi/harness_pi/extension/index.ts":           PiExtension,
		"../../../../src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts": OpenCodePlugin,
	} {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != got {
			t.Errorf("%s drifted from the embedded copy: copy it into internal/install/assets", path)
		}
	}
}

func command(e *pyjson.Object) string {
	c, _ := e.Value("hooks").([]any)[0].(*pyjson.Object).Get("command")
	return c.(string)
}

// An enforce hook must say enforce, carry the fail-closed suffix, match
// every tool, and be found by Python's reader of the installed mode.
func TestEnforceHookIsNeverWeaker(t *testing.T) {
	e := HookEntry("/opt/my bin/daisugi", HookOptions{Mode: "enforce", Root: "/h/.opendaisugi/gate"})
	cmd := command(e)
	want := "DAISUGI_GATE_HOOK=opendaisugi.gate '/opt/my bin/daisugi' gate check --mode enforce " +
		"--root /h/.opendaisugi/gate --format claude --verify-timeout 10.0 || exit 2"
	if cmd != want {
		t.Fatalf("got  %s\nwant %s", cmd, want)
	}
	if m, _ := e.Get("matcher"); m != "*" {
		t.Fatalf("matcher %v", m)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	doc := pyjson.NewObject().Set("hooks", pyjson.NewObject().Set("PreToolUse", []any{e}))
	if err := os.WriteFile(p, []byte(pyjson.DumpsIndent(doc, 2, true)), 0o644); err != nil {
		t.Fatal(err)
	}
	if mode, err := config.InstalledHookMode(p); err != nil || mode != "enforce" {
		t.Fatalf("installed mode %q %v", mode, err)
	}
	shadow := command(HookEntry("/b", HookOptions{Mode: "shadow", Root: "/r"}))
	if strings.Contains(shadow, "exit 2") {
		t.Fatalf("shadow must never block the host: %s", shadow)
	}
}

func TestAskWidensTheHostTimeout(t *testing.T) {
	e := HookEntry("/b", HookOptions{Mode: "enforce", Root: "/r", Ask: true})
	h := e.Value("hooks").([]any)[0].(*pyjson.Object)
	if to, _ := h.Get("timeout"); to != 105 {
		t.Fatalf("timeout %v", to)
	}
	if !strings.Contains(command(e), " --ask --ask-timeout 90 || exit 2") {
		t.Fatal(command(e))
	}
}

func TestCodexGateUsesARegexMatcher(t *testing.T) {
	dir := t.TempDir()
	e, err := PlanCodexGate(filepath.Join(dir, "hooks.json"), HookEntry("/b", HookOptions{Mode: "enforce", Root: "/r"}))
	if err != nil || e == nil {
		t.Fatal(e, err)
	}
	if !strings.Contains(e.Content, `"matcher": ".*"`) {
		t.Fatal(e.Content)
	}
}

func TestPopKeepsOtherHooks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [` +
		`{"type": "command", "command": "daisugi hook record"},` +
		`{"type": "command", "command": "python3 -m opendaisugi.gate --mode enforce"}, {"type": "command", "command": "/opt/opendaisugi.gateway-watch"}]}]}, "env": {"A": "1"}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := PlanPopHook(p, config.IsGateHook, []string{"PreToolUse"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"hooks\": {\n    \"PreToolUse\": [\n      {\n        \"matcher\": \"Bash\",\n        \"hooks\": [\n" +
		"          {\n            \"type\": \"command\",\n            \"command\": \"daisugi hook record\"\n          },\n" +
		"          {\n            \"type\": \"command\",\n            \"command\": \"/opt/opendaisugi.gateway-watch\"\n          }\n" +
		"        ]\n      }\n    ]\n  },\n  \"env\": {\n    \"A\": \"1\"\n  }\n}\n"
	if e.Content != want {
		t.Fatalf("got\n%s", e.Content)
	}
}

// A foreign hook that merely holds "opendaisugi.gate" is not a gate hook:
// install adds ours next to it rather than skipping.
func TestInstallIgnoresAForeignHook(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := `{"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "/opt/opendaisugi.gateway-watch --mode enforce"}]}]}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := PlanClaudeGate(p, HookEntry("/b", HookOptions{Mode: "shadow", Root: "/r"}))
	if err != nil || e == nil || e.Warning != "" || !strings.Contains(e.Content, "gate check") {
		t.Fatal(e, err)
	}
	e, err = PlanCodexGate(p, HookEntry("/b", HookOptions{Mode: "shadow", Root: "/r"}))
	if err != nil || e == nil || !strings.Contains(e.Content, "gate check") {
		t.Fatal(e, err)
	}
}

// A binary path with spaces, quotes, $ and ; is one shell word in the
// hook, and every reader finds the gate and its mode.
func TestHostilePathIsQuoted(t *testing.T) {
	for _, self := range []string{
		"/tmp/h1/my bin/daisugi",
		"/tmp/it's here/daisugi",
		"/tmp/$(touch x); rm -rf ~/`y`/daisugi",
		"/tmp/--mode enforce/daisugi",
	} {
		for _, mode := range []string{"shadow", "enforce"} {
			cmd := command(HookEntry(self, HookOptions{Mode: mode, Root: "/r oot/'g'", Session: strp("s; x")}))
			words, err := verify.ShlexSplit(cmd)
			if err != nil {
				t.Fatalf("%s: %v", cmd, err)
			}
			if words[0] != MarkerEnv || words[1] != self || words[2] != "gate" || words[3] != "check" {
				t.Fatalf("words %q", words)
			}
			if got := config.GateHookMode(cmd); got != mode {
				t.Fatalf("%s: mode %q", cmd, got)
			}
			args, _ := config.GateHookArgs(cmd)
			if args[3] != "/r oot/'g'" || args[len(args)-1] != "s; x" {
				t.Fatalf("args %q", args)
			}
		}
	}
}

func strp(s string) *string { return &s }

// The hook names the binary by the path it was run by, symlink kept, so
// an upgrade that swaps the file behind the link leaves the hook working.
func TestSelfKeepsTheInvokedPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "v1", "daisugi")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "daisugi")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got, err := self("daisugi", "/nonexistent:"+bin); err != nil || got != link {
		t.Fatalf("on PATH: %q %v", got, err)
	}
	if got, err := self(link, ""); err != nil || got != link {
		t.Fatalf("by path: %q %v", got, err)
	}
}
