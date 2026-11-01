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
		"../../../../src/opendaisugi/harness_pi/extension/index.ts":             PiExtension,
		"../../../../src/opendaisugi/harness_opencode/plugin/daisugi-warden.ts": OpenCodePlugin,
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
	e := HookEntry("/opt/my bin/daisugi", HookOptions{Mode: "enforce", Root: "/h/.opendaisugi/warden"})
	cmd := command(e)
	want := "DAISUGI_WARDEN_HOOK=opendaisugi.warden '/opt/my bin/daisugi' warden check --mode enforce " +
		"--root /h/.opendaisugi/warden --format claude --verify-timeout 10.0 || exit 2"
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
	audit := command(HookEntry("/b", HookOptions{Mode: "audit", Root: "/r"}))
	if strings.Contains(audit, "exit 2") {
		t.Fatalf("audit must never block the host: %s", audit)
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

func TestCodexWardenUsesARegexMatcher(t *testing.T) {
	dir := t.TempDir()
	e, err := PlanCodexWarden(filepath.Join(dir, "hooks.json"), HookEntry("/b", HookOptions{Mode: "enforce", Root: "/r"}))
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
		`{"type": "command", "command": "python3 -m opendaisugi.warden --mode enforce"}, {"type": "command", "command": "/opt/opendaisugi.router-watch"}]}]}, "env": {"A": "1"}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := PlanPopHook(p, config.IsWardenHook, []string{"PreToolUse"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"hooks\": {\n    \"PreToolUse\": [\n      {\n        \"matcher\": \"Bash\",\n        \"hooks\": [\n" +
		"          {\n            \"type\": \"command\",\n            \"command\": \"daisugi hook record\"\n          },\n" +
		"          {\n            \"type\": \"command\",\n            \"command\": \"/opt/opendaisugi.router-watch\"\n          }\n" +
		"        ]\n      }\n    ]\n  },\n  \"env\": {\n    \"A\": \"1\"\n  }\n}\n"
	if e.Content != want {
		t.Fatalf("got\n%s", e.Content)
	}
}

// A foreign hook that merely holds "opendaisugi.warden" is not a warden hook:
// install adds ours next to it rather than skipping.
func TestInstallIgnoresAForeignHook(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	body := `{"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "/opt/opendaisugi.router-watch --mode enforce"}]}]}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := PlanClaudeWarden(p, HookEntry("/b", HookOptions{Mode: "audit", Root: "/r"}))
	if err != nil || e == nil || e.Warning != "" || !strings.Contains(e.Content, "warden check") {
		t.Fatal(e, err)
	}
	e, err = PlanCodexWarden(p, HookEntry("/b", HookOptions{Mode: "audit", Root: "/r"}))
	if err != nil || e == nil || !strings.Contains(e.Content, "warden check") {
		t.Fatal(e, err)
	}
}

// A binary path with spaces, quotes, $ and ; is one shell word in the
// hook, and every reader finds the warden and its mode.
func TestHostilePathIsQuoted(t *testing.T) {
	for _, self := range []string{
		"/tmp/h1/my bin/daisugi",
		"/tmp/it's here/daisugi",
		"/tmp/$(touch x); rm -rf ~/`y`/daisugi",
		"/tmp/--mode enforce/daisugi",
	} {
		for _, mode := range []string{"audit", "enforce"} {
			cmd := command(HookEntry(self, HookOptions{Mode: mode, Root: "/r oot/'g'", Session: strp("s; x")}))
			words, err := verify.ShlexSplit(cmd)
			if err != nil {
				t.Fatalf("%s: %v", cmd, err)
			}
			if words[0] != MarkerEnv || words[1] != self || words[2] != "warden" || words[3] != "check" {
				t.Fatalf("words %q", words)
			}
			if got := config.WardenHookMode(cmd); got != mode {
				t.Fatalf("%s: mode %q", cmd, got)
			}
			args, _ := config.WardenHookArgs(cmd)
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
	// A binary run under a name that is not ours is not written as it is:
	// the hooks name what PATH calls daisugi, else the bare name.
	other := filepath.Join(dir, "daisugi-local")
	if err := os.Symlink(real, other); err != nil {
		t.Fatal(err)
	}
	if got, err := selfEntry(other, bin); err != nil || got != link {
		t.Fatalf("a wrapper name: %q %v", got, err)
	}
	if got, err := selfEntry(other, "/nonexistent"); err != nil || got != "daisugi" {
		t.Fatalf("a wrapper name, none on PATH: %q %v", got, err)
	}
}

// The embedded skill and OpenClaw plugin must be the files Python ships,
// every one of them.
func TestLayerAssetsMatchPython(t *testing.T) {
	for _, pair := range [][2]string{
		{"../../../../src/opendaisugi/skills/opendaisugi-checklist", "assets/skill/opendaisugi-checklist"},
		{"../../../../src/opendaisugi/install_assets/openclaw_plugin", "assets/openclaw_plugin"},
	} {
		var want []string
		err := filepath.Walk(pair[0], func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || strings.Contains(p, "__pycache__") {
				return err
			}
			rel, _ := filepath.Rel(pair[0], p)
			want = append(want, rel)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, f := range SkillFiles(pair[1]) {
			got[f.Rel] = f.Text
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d embedded files, %d shipped", pair[1], len(got), len(want))
		}
		for _, rel := range want {
			b, _ := os.ReadFile(filepath.Join(pair[0], rel))
			if got[rel] != string(b) {
				t.Errorf("%s/%s drifted from the embedded copy", pair[0], rel)
			}
		}
	}
}

func TestWardenModeRelation(t *testing.T) {
	audit := HookEntry("/b", HookOptions{Mode: "audit", Root: "/r"}).Value("hooks").([]any)[0].(*pyjson.Object).Value("command").(string)
	enforce := HookEntry("/b", HookOptions{Mode: "enforce", Root: "/r"}).Value("hooks").([]any)[0].(*pyjson.Object).Value("command").(string)
	ask := HookEntry("/b", HookOptions{Mode: "enforce", Root: "/r", Ask: true}).Value("hooks").([]any)[0].(*pyjson.Object).Value("command").(string)
	other := HookEntry("/b", HookOptions{Mode: "enforce", Root: "/other"}).Value("hooks").([]any)[0].(*pyjson.Object).Value("command").(string)
	for _, c := range []struct{ old, new, want string }{
		{audit, enforce, "up"},
		{audit, ask, "up"},
		{enforce, audit, "keep"},
		{ask, audit, "keep"},
		{audit, audit, ""},
		{enforce, enforce, ""},
		{audit, other, ""},
	} {
		if got := WardenModeRelation(c.old, c.new); got != c.want {
			t.Errorf("WardenModeRelation(%q, %q) = %q, want %q", c.old, c.new, got, c.want)
		}
	}
}
