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

// Install writes the absolute path of the daisugi entry point, replaces an
// old bare entry in place, and checks that the command can run.

func runtimeOf(key string) Runtime {
	for _, r := range Runtimes {
		if r.Key == key {
			return r
		}
	}
	panic(key)
}

// apply plans and writes one runtime's install under home.
func applyInstall(t *testing.T, key, home, self string, o Opts) {
	t.Helper()
	ch, err := PlanInstall(runtimeOf(key), home, self, filepath.Join(home, ".opendaisugi", "warden"), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyChange(ch); err != nil {
		t.Fatal(err)
	}
}

func putFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func getFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func recordCommands(t *testing.T, settings string) []string {
	t.Helper()
	v, err := pyjson.Loads(settings)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	hooks, _ := v.(*pyjson.Object).Value("hooks").(*pyjson.Object)
	for _, ev := range hooks.Keys() {
		for _, e := range hooks.Value(ev).([]any) {
			for _, h := range e.(*pyjson.Object).Value("hooks").([]any) {
				c := h.(*pyjson.Object).Value("command")
				if config.IsRecordHook(c, "") {
					out = append(out, c.(string))
				}
			}
		}
	}
	return out
}

func TestClaudeEntriesUseTheAbsolutePathAndQuoteIt(t *testing.T) {
	home := t.TempDir()
	self := "/opt/my tools/daisugi"
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	applyInstall(t, "claude", home, self, Opts{Report: "herdr"})
	cmds := recordCommands(t, getFile(t, filepath.Join(home, ".claude", "settings.json")))
	if len(cmds) != 5 {
		t.Fatalf("want 5 record hooks, got %v", cmds)
	}
	for _, c := range cmds {
		toks, err := verify.ShlexSplit(c)
		if err != nil || toks[0] != self {
			t.Fatalf("%q: %v %v", c, toks, err)
		}
	}
	mcp := getFile(t, filepath.Join(home, ".claude.json"))
	if !strings.Contains(mcp, `"command": "/opt/my tools/daisugi"`) {
		t.Fatalf("mcp: %s", mcp)
	}
}

func TestARerunReplacesABareEntryInPlace(t *testing.T) {
	home := t.TempDir()
	self := "/x/bin/daisugi"
	putFile(t, filepath.Join(home, ".claude", "settings.json"), `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"daisugi hook record --format claude"}]}]}}`)
	putFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"opendaisugi":{"type":"stdio","command":"daisugi","args":["mcp","serve"],"env":{"K":"v"}}}}`)
	applyInstall(t, "claude", home, self, Opts{})
	cmds := recordCommands(t, getFile(t, filepath.Join(home, ".claude", "settings.json")))
	if len(cmds) != 1 || cmds[0] != "/x/bin/daisugi hook record --format claude" {
		t.Fatalf("got %v", cmds)
	}
	mcp := getFile(t, filepath.Join(home, ".claude.json"))
	if !strings.Contains(mcp, `"command": "/x/bin/daisugi"`) || !strings.Contains(mcp, `"K": "v"`) {
		t.Fatalf("mcp: %s", mcp)
	}
	// A second run changes the hook files no more.
	before := getFile(t, filepath.Join(home, ".claude", "settings.json"))
	ch, err := PlanInstall(runtimeOf("claude"), home, self, filepath.Join(home, ".opendaisugi", "warden"), Opts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ch.Edits {
		if e.Kind == "" && (e.Path == filepath.Join(home, ".claude.json")) {
			t.Fatalf("a second run planned an edit of %s", e.Path)
		}
	}
	if getFile(t, filepath.Join(home, ".claude", "settings.json")) != before {
		t.Fatal("the plan wrote")
	}
}

// legacyLayers are the layers install wrote before the pando layer: the
// tests of their rebinding rules leave pando out.
var legacyLayers = map[string]bool{LayerSkill: true, LayerMCP: true, LayerCapture: true, LayerInstructions: true, LayerWarden: true}

func TestACustomMCPCommandIsLeftAlone(t *testing.T) {
	home := t.TempDir()
	own := `{"mcpServers":{"opendaisugi":{"command":"/my/wrapper","args":["mcp","serve"]}}}`
	putFile(t, filepath.Join(home, ".claude.json"), own)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	applyInstall(t, "claude", home, "/x/daisugi", Opts{Only: legacyLayers})
	if got := getFile(t, filepath.Join(home, ".claude.json")); got != own {
		t.Fatalf("changed: %s", got)
	}
}

func TestCodexBareBlockIsReplacedAndBothFormsUninstall(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".codex", "config.toml")
	putFile(t, p, "model = \"x\"\n\n[mcp_servers.opendaisugi]\ncommand = \"daisugi\"\nargs = [\"mcp\", \"serve\"]\n\n[other]\nk = 1\n")
	applyInstall(t, "codex", home, "/x/bin/daisugi", Opts{Only: map[string]bool{LayerMCP: true}})
	got := getFile(t, p)
	if strings.Count(got, "[mcp_servers.opendaisugi]") != 1 || !strings.Contains(got, `command = "/x/bin/daisugi"`) ||
		!strings.Contains(got, "[other]\nk = 1\n") {
		t.Fatalf("got %q", got)
	}
	e, err := PlanCodexMCPRemove(p)
	if err != nil || e == nil || strings.Contains(e.Content, "opendaisugi") || !strings.Contains(e.Content, "[other]") {
		t.Fatalf("remove: %v %+v", err, e)
	}
	putFile(t, p, "[mcp_servers.opendaisugi]\ncommand = \"daisugi\"\nargs = [\"mcp\", \"serve\"]\n")
	putFile(t, p, "x = 1\n\n[mcp_servers.opendaisugi]\ncommand = \"daisugi\"\nargs = [\"mcp\", \"serve\"]\n")
	if e, err := PlanCodexMCPRemove(p); err != nil || e == nil || e.Content != "x = 1\n" {
		t.Fatalf("remove bare: %v %+v", err, e)
	}
}

func TestHermesBareEntriesAreRebound(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".hermes", "config.yaml")
	putFile(t, p, "mcp_servers:\n  opendaisugi:\n    command: daisugi\n    args:\n    - mcp\n    - serve\nhooks:\n  pre_tool_call:\n  - matcher: .*\n    command: daisugi hook record --format hermes\n    timeout: 10\n")
	applyInstall(t, "hermes", home, "/x/bin/daisugi", Opts{Only: legacyLayers})
	got := getFile(t, p)
	if strings.Count(got, "hook record") != 1 || !strings.Contains(got, "command: /x/bin/daisugi hook record --format hermes") ||
		!strings.Contains(got, "command: /x/bin/daisugi\n") {
		t.Fatalf("got %s", got)
	}
}

func TestOldBareRecordHooksAndAbsoluteOnesAreBothRecordHooks(t *testing.T) {
	for _, c := range []string{
		"daisugi hook record --format claude",
		"/opt/x/daisugi hook record --format claude",
		"/opt/x/daisugi-rs hook record --format claude --event stop",
	} {
		if !config.IsRecordHook(c, "") {
			t.Errorf("%q must be a record hook", c)
		}
	}
	if config.IsRecordHook("/opt/x/other hook record", "") {
		t.Error("another program is not a record hook")
	}
}

func TestAPlainCommandSwapsOnlyItsProgramWord(t *testing.T) {
	exe := "/x/bin/daisugi"
	for _, c := range [][2]string{
		{"daisugi hook record --format claude", " hook record --format claude"},
		{"/old/bin/daisugi-rs hook record --format claude", " hook record --format claude"},
		{"'/old path/daisugi' hook record --format claude", " hook record --format claude"},
		{"daisugi  hook record  --format   claude  ", "  hook record  --format   claude  "},
	} {
		got, left := rebindRecordCommand(c[0], exe)
		if got != exe+c[1] || left {
			t.Errorf("%q: got %q left=%v", c[0], got, left)
		}
	}
	got, left := rebindRecordCommand("daisugi hook record --format claude", "/opt/my tools/daisugi")
	if got != "'/opt/my tools/daisugi' hook record --format claude" || left {
		t.Errorf("quoted: %q %v", got, left)
	}
}

func TestAnythingMoreIsLeftByteForByteAndFlagged(t *testing.T) {
	for _, old := range []string{
		"daisugi hook record --format claude || true",
		"daisugi hook record --format claude 2>/dev/null",
		"daisugi hook record --format claude >/dev/null",
		"daisugi hook record --format claude; exit 0",
		"daisugi hook record --format claude && true",
		"daisugi hook record --format claude | cat",
		"daisugi hook record --format claude &",
		"daisugi hook record --format claude $(true)",
		"daisugi hook record --format 'claude'",
		"env X=1 daisugi hook record --format claude",
		"X=1 daisugi hook record --format claude",
		"daisugi hook record --format claude\ntrue",
	} {
		if got, left := rebindRecordCommand(old, "/x/bin/daisugi"); got != old || !left {
			t.Errorf("%q: got %q left=%v", old, got, left)
		}
	}
	for _, c := range []string{
		"/x/bin/daisugi hook record --format claude --event stop",
		"/x/bin/daisugi hook record --format claude || true",
		"env X=1 /x/bin/daisugi hook record --format claude",
		"other hook record",
		"daisugi-local hook record --format claude",
		"true || daisugi x",
	} {
		if got, left := rebindRecordCommand(c, "/x/bin/daisugi"); got != c || left {
			t.Errorf("%q needs nothing: got %q left=%v", c, got, left)
		}
	}
}

func TestAPathIsNeverRewrittenToABareWord(t *testing.T) {
	for _, old := range []string{
		"/old/bin/daisugi hook record --format claude",
		"'/old path/daisugi-go' hook record --format claude",
	} {
		if got, left := rebindRecordCommand(old, "daisugi"); got != old || !left {
			t.Errorf("%q: got %q left=%v", old, got, left)
		}
	}
	if got, left := rebindRecordCommand("daisugi-go hook record --format claude", "daisugi"); got != "daisugi hook record --format claude" || left {
		t.Errorf("a bare word may rewrite a bare word: %q %v", got, left)
	}
	if got, left := rebindRecordCommand("daisugi hook record --format claude", "daisugi"); got != "daisugi hook record --format claude" || left {
		t.Errorf("the same word needs nothing: %q %v", got, left)
	}
	note := leftHookNote("Claude Code", "/old/bin/daisugi hook record --format claude", "daisugi")
	want := "[Claude Code] The hook `/old/bin/daisugi hook record --format claude` was left as it is: install found no daisugi on PATH, so the only word it could write is `daisugi`, and the hook already names a path."
	if note != want {
		t.Errorf("note: %q", note)
	}
	if n := leftHookNote("Claude Code", "daisugi hook record || true", "daisugi"); !strings.Contains(n, "not one plain command") {
		t.Errorf("a compound hook keeps its own note: %q", n)
	}
}

func TestOurNamesAreAnExactSet(t *testing.T) {
	for w, ours := range map[string]bool{
		"daisugi": true, "daisugi-py": true, "daisugi-go": true, "daisugi-rs": true, "/x/y/daisugi-rs": true,
		"daisugi-local": false, "/home/u/bin/daisugi-local": false, "daisugi-": false, "daisugix": false, "mydaisugi": false,
	} {
		if config.IsEntryWord(w) != ours {
			t.Errorf("%q: want %v", w, ours)
		}
	}
	if config.IsRecordHook("daisugi-local hook record --format claude", "") {
		t.Error("a user wrapper is not a record hook")
	}
}

func TestACompoundHookIsLeftAndNotedWithItsRuntime(t *testing.T) {
	home := t.TempDir()
	old := "daisugi hook record --format claude || true"
	p := filepath.Join(home, ".claude", "settings.json")
	putFile(t, p, `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"`+old+`"}]}]}}`)
	e, err := PlanClaudeCapture(p, nil, "/x/bin/daisugi")
	if err != nil || e == nil || !e.NoteOnly || e.Content != "" {
		t.Fatalf("want a note-only edit: %v %+v", err, e)
	}
	if !strings.HasPrefix(e.Note, "[Claude Code] ") || !strings.Contains(e.Note, old) || !LeftAsIs(e.Note) {
		t.Fatalf("note: %q", e.Note)
	}
}

func TestHermesWritesNothingWhenNothingWasRebound(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".hermes", "config.yaml")
	putFile(t, p, "mcp_servers:\n  opendaisugi:\n    command: /x/bin/daisugi\n    args:\n    - mcp\n    - serve\nhooks:\n  pre_tool_call:\n  - matcher: .*\n    command: daisugi hook record --format hermes || true\n    timeout: 10\n")
	e, err := PlanHermesConfig(p, true, true, "/x/bin/daisugi")
	if err != nil || e == nil || !e.NoteOnly || e.Content != "" {
		t.Fatalf("want a note-only edit, so no rewrite and no new backup: %v %+v", err, e)
	}
	if !strings.HasPrefix(e.Note, "[Hermes] ") || !LeftAsIs(e.Note) {
		t.Fatalf("note: %q", e.Note)
	}
}

func TestAUserWrapperIsNotReplaced(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".codex", "config.toml")
	putFile(t, p, "model = \"x\"\n\n[mcp_servers.opendaisugi]\ncommand = \"/h/daisugi-local\"\nargs = [\"mcp\", \"serve\"]\n")
	if e, err := PlanCodexMCP(p, "/x/bin/daisugi"); err != nil || e != nil {
		t.Fatalf("a wrapper block is left: %v %+v", err, e)
	}
}

func TestTheCheckReadsTheHookThatIsOnDisk(t *testing.T) {
	home := t.TempDir()
	gone := filepath.Join(home, "gone", "daisugi")
	putFile(t, filepath.Join(home, ".claude", "settings.json"),
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"`+gone+` hook record --format claude"}]}]}}`)
	putFile(t, filepath.Join(home, ".hermes", "config.yaml"), "mcp_servers:\n  opendaisugi:\n    command: "+gone+"\n    args:\n    - mcp\n    - serve\n")
	lines := UnrunnableCommands([]Runtime{runtimeOf("claude"), runtimeOf("hermes")}, home)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "[Claude Code] The command "+gone+" is not there, ") || !strings.HasPrefix(lines[1], "[Hermes] ") {
		t.Fatalf("got %q", lines)
	}
	plain := filepath.Join(home, "bin", "daisugi")
	putFile(t, plain, "x")
	putFile(t, filepath.Join(home, ".claude", "settings.json"),
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"`+plain+` hook record --format claude"}]}]}}`)
	if lines := UnrunnableCommands([]Runtime{runtimeOf("claude")}, home); len(lines) != 1 || !strings.Contains(lines[0], "is not executable") {
		t.Fatalf("got %q", lines)
	}
	if err := os.Chmod(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if lines := UnrunnableCommands([]Runtime{runtimeOf("claude")}, home); lines != nil {
		t.Fatalf("a runnable command must be quiet: %q", lines)
	}
	if lines := UnrunnableCommands([]Runtime{runtimeOf("codex")}, home); lines != nil {
		t.Fatalf("nothing written, nothing to say: %q", lines)
	}
}

func TestAUvCachePathIsCalledTemporary(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, ".cache", "uv", "archive-v0", "abc", "bin", "daisugi")
	putFile(t, exe, "x")
	if note := TemporaryExeNote(exe); !strings.Contains(note, "temporary") || !strings.Contains(note, "uv tool install") {
		t.Fatalf("note: %q", note)
	}
	link := filepath.Join(home, "bin", "daisugi")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	if TemporaryExeNote(link) == "" {
		t.Error("a link into the uv cache is temporary")
	}
	if TemporaryExeNote("/usr/local/bin/daisugi") != "" {
		t.Error("an installed path is not temporary")
	}
}

func TestThePathNoteNeedsAProgramWordThatNamesAPath(t *testing.T) {
	n := leftHookNote("Claude Code", "daisugi-go hook record --format claude", "daisugi")
	if strings.Contains(n, "already names a path") || !strings.Contains(n, "not one plain command") {
		t.Errorf("a bare program word has the other note: %q", n)
	}
}

func TestAnMCPCommandThatNamesAPathIsNeverRewrittenToABareWord(t *testing.T) {
	dir := t.TempDir()
	note := func(p, old string) string {
		return "The MCP command `" + old + "` in " + p + " was left as it is: install found no daisugi on PATH, so the only word it could write is `daisugi`, and the entry already names a path."
	}
	// Claude and OpenClaw JSON.
	j := filepath.Join(dir, "claude.json")
	putFile(t, j, `{"mcpServers": {"opendaisugi": {"command": "/old/bin/daisugi", "args": ["mcp", "serve"]}}}`)
	e, err := PlanMCP(j, false, []string{"mcpServers"}, mcpEntry(true, "daisugi"), "MCP registration", nil)
	if err != nil || e == nil || !e.NoteOnly || e.Content != "" || e.Note != note(j, "/old/bin/daisugi") {
		t.Fatalf("json: %v %+v", err, e)
	}
	// A bare word still takes the bare word.
	putFile(t, j, `{"mcpServers": {"opendaisugi": {"command": "daisugi-go", "args": ["mcp", "serve"]}}}`)
	if e, err := PlanMCP(j, false, []string{"mcpServers"}, mcpEntry(true, "daisugi"), "MCP registration", nil); err != nil || e == nil || e.Note != "" || !strings.Contains(e.Content, `"command": "daisugi"`) {
		t.Fatalf("bare: %v %+v", err, e)
	}
	// Codex.
	c := filepath.Join(dir, "config.toml")
	putFile(t, c, "\n[mcp_servers.opendaisugi]\ncommand = \"/old/bin/daisugi\"\nargs = [\"mcp\", \"serve\"]\n")
	if e, err := PlanCodexMCP(c, "daisugi"); err != nil || e == nil || !e.NoteOnly || e.Note != note(c, "/old/bin/daisugi") {
		t.Fatalf("codex: %v %+v", err, e)
	}
	// Hermes.
	h := filepath.Join(dir, "config.yaml")
	putFile(t, h, "mcp_servers:\n  opendaisugi:\n    command: /old/bin/daisugi\n")
	if e, err := PlanHermesConfig(h, true, false, "daisugi"); err != nil || e == nil || !e.NoteOnly || e.Note != note(h, "/old/bin/daisugi") {
		t.Fatalf("hermes: %v %+v", err, e)
	}
}
