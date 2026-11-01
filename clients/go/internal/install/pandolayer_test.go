package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scratchHome(t *testing.T) string {
	h := t.TempDir()
	for _, d := range []string{".claude", ".hermes", ".hermes/profiles/household"} {
		if err := os.MkdirAll(filepath.Join(h, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A profile counts when it has a config or a state.db.
	os.WriteFile(filepath.Join(h, ".hermes/profiles/household/config.yaml"), []byte("model: x\n"), 0o600)
	os.WriteFile(filepath.Join(h, ".claude/settings.json"), []byte(`{"permissions":{"allow":["Bash"]}}`), 0o600)
	return h
}

func runOnly(t *testing.T, rt Runtime, home string) {
	t.Helper()
	ch, err := PlanInstall(rt, home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{Only: map[string]bool{LayerPando: true}})
	if err != nil || ch.Failed {
		t.Fatalf("plan: %v %+v", err, ch)
	}
	if _, err := ApplyChange(ch); err != nil {
		t.Fatal(err)
	}
}

func TestPandoLayerClaudeWiresMCPAndFourHooksAndKeepsTheRest(t *testing.T) {
	home := scratchHome(t)
	claude := Runtimes[0]
	runOnly(t, claude, home)
	mcp, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if !strings.Contains(string(mcp), `"pando"`) || !strings.Contains(string(mcp), `"serve"`) || strings.Contains(string(mcp), "opendaisugi") {
		t.Fatalf(".claude.json: %s", mcp)
	}
	set, _ := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if !strings.Contains(string(set), ev) {
			t.Fatalf("no %s in %s", ev, set)
		}
	}
	if strings.Count(string(set), "/opt/bin/daisugi pando hook") != 4 || !strings.Contains(string(set), `"permissions"`) {
		t.Fatalf("settings: %s", set)
	}
	if _, err := os.Stat(PandoUnitPath(home)); err != nil {
		t.Fatalf("no unit: %v", err)
	}
	// Idempotent: a second run plans nothing.
	ch, _ := PlanInstall(claude, home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{Only: map[string]bool{LayerPando: true}})
	if len(ch.Edits) != 0 {
		t.Fatalf("second plan: %d edits", len(ch.Edits))
	}
	// Reverse takes out the pando entries and nothing else.
	rv, err := PlanReverseAll(claude, home, map[string]bool{}, map[string]bool{LayerPando: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyChange(rv); err != nil {
		t.Fatal(err)
	}
	set, _ = os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	mcp, _ = os.ReadFile(filepath.Join(home, ".claude.json"))
	if strings.Contains(string(set), "pando") || strings.Contains(string(mcp), "pando") || !strings.Contains(string(set), `"permissions"`) {
		t.Fatalf("after reverse: %s | %s", set, mcp)
	}
	if _, err := os.Stat(PandoUnitPath(home)); err == nil {
		t.Fatal("unit left")
	}
}

func TestPandoLayerHermesCoversEveryProfileWithPluginAndEnable(t *testing.T) {
	home := scratchHome(t)
	hermes := Runtimes[2]
	runOnly(t, hermes, home)
	for _, d := range []string{".hermes", ".hermes/profiles/household"} {
		cfg, _ := os.ReadFile(filepath.Join(home, d, "config.yaml"))
		if !strings.Contains(string(cfg), "pando") || !strings.Contains(string(cfg), "enabled") {
			t.Fatalf("%s config: %s", d, cfg)
		}
		py, err := os.ReadFile(filepath.Join(home, d, "plugins/pando/__init__.py"))
		if err != nil || !strings.Contains(string(py), `"/opt/bin/daisugi"`) || !strings.Contains(string(py), "pre_llm_call") {
			t.Fatalf("%s plugin: %v %s", d, err, py)
		}
		if _, err := os.Stat(filepath.Join(home, d, "plugins/pando/plugin.yaml")); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".hermes/profiles/household/config.yaml")); !strings.Contains(string(b), "model: x") {
		t.Fatalf("user config lost: %s", b)
	}
	rv, _ := PlanReverseAll(hermes, home, map[string]bool{}, map[string]bool{LayerPando: true})
	if _, err := ApplyChange(rv); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".hermes/plugins/pando")); err == nil {
		t.Fatal("plugin left")
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".hermes/profiles/household/config.yaml")); strings.Contains(string(b), "pando") {
		t.Fatalf("config after reverse: %s", b)
	}
}

func TestPandoLayerShowsInThePlanAndIsOffUnderOtherOnlyLists(t *testing.T) {
	home := scratchHome(t)
	var found bool
	for _, s := range PlanAllSteps(Runtimes[0], home, Opts{}) {
		found = found || s.Layer == LayerPando
	}
	if !found {
		t.Fatal("a default plan must show the pando layer")
	}
	for _, s := range PlanAllSteps(Runtimes[0], home, Opts{Only: map[string]bool{LayerSkill: true}}) {
		if s.Layer == LayerPando {
			t.Fatal("--only skill must not show pando")
		}
	}
}

func TestPandoLayerCodexBlockRoundTrips(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	p := filepath.Join(home, ".codex/config.toml")
	os.WriteFile(p, []byte("model = \"x\"\n"), 0o600)
	e, err := PlanPandoCodexMCP(p, "/opt/bin/daisugi", nil)
	if err != nil || e == nil || !strings.Contains(e.Content, "[mcp_servers.pando]") || !strings.Contains(e.Content, `model = "x"`) {
		t.Fatalf("%v %+v", err, e)
	}
	os.WriteFile(p, []byte(e.Content+"\n[other]\na = 1\n"), 0o600)
	r, err := PlanPandoCodexMCPRemove(p, nil)
	if err != nil || r == nil || strings.Contains(r.Content, "pando") || !strings.Contains(r.Content, "[other]") || !strings.Contains(r.Content, `model = "x"`) {
		t.Fatalf("%v %+v", err, r)
	}
}

// Round 7 fix: the logging hooks run in the background, SessionEnd catches
// the last reply, and an older hook is brought to the same shape.

func hookEntryOf(t *testing.T, settings, ev string) map[string]any {
	t.Helper()
	var s struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Hooks[ev] {
		for _, h := range e.Hooks {
			if c, _ := h["command"].(string); strings.HasSuffix(c, " pando hook") {
				return h
			}
		}
	}
	t.Fatalf("no pando hook for %s in %s", ev, settings)
	return nil
}

func TestPandoLoggingHooksAreAsyncAndTheViewHookIsNot(t *testing.T) {
	home := scratchHome(t)
	runOnly(t, Runtimes[0], home)
	set, _ := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	if h := hookEntryOf(t, string(set), "SessionStart"); h["async"] != nil {
		t.Fatalf("the context hook must be waited for: %v", h)
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop"} {
		if h := hookEntryOf(t, string(set), ev); h["async"] != true {
			t.Fatalf("%s must run async: %v", ev, h)
		}
	}
	// The end of the chat is waited for, so the last reply is not lost at exit.
	if h := hookEntryOf(t, string(set), "SessionEnd"); h["async"] != nil || h["timeout"] != float64(10) {
		t.Fatalf("SessionEnd must run in the foreground with a 10 s timeout: %v", h)
	}
}

func TestPandoSessionEndOfTheFirstBuildIsBroughtToTheForeground(t *testing.T) {
	home := scratchHome(t)
	old := `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"/opt/bin/daisugi pando hook","timeout":10,"async":true}]}]}}`
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte(old), 0o600)
	runOnly(t, Runtimes[0], home)
	set, _ := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	if h := hookEntryOf(t, string(set), "SessionEnd"); h["async"] != nil {
		t.Fatalf("SessionEnd stayed async: %v", h)
	}
	if strings.Count(string(set), " pando hook") != 4 {
		t.Fatalf("a hook was added twice: %s", set)
	}
}

func TestPandoHooksOfTheFirstBuildAreBroughtUp(t *testing.T) {
	home := scratchHome(t)
	old := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/opt/bin/daisugi pando hook","timeout":10}]}],` +
		`"SessionStart":[{"hooks":[{"type":"command","command":"/opt/bin/daisugi pando hook","timeout":10,"async":true}]}]}}`
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte(old), 0o600)
	runOnly(t, Runtimes[0], home)
	set, _ := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	if hookEntryOf(t, string(set), "Stop")["async"] != true || hookEntryOf(t, string(set), "SessionStart")["async"] != nil {
		t.Fatalf("hooks not brought up: %s", set)
	}
	if strings.Count(string(set), " pando hook") != 4 {
		t.Fatalf("a hook was added twice: %s", set)
	}
}

func TestPandoHermesPluginAsksEveryTurnAndIsIdempotent(t *testing.T) {
	home := scratchHome(t)
	runOnly(t, Runtimes[2], home)
	py, _ := os.ReadFile(filepath.Join(home, ".hermes/plugins/pando/__init__.py"))
	if strings.Contains(string(py), "is_first_turn:") || strings.Contains(string(py), "if not is_first_turn") {
		t.Fatalf("the plugin still skips later turns:\n%s", py)
	}
	// The same plan twice writes nothing the second time.
	ch, _ := PlanInstall(Runtimes[2], home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{Only: map[string]bool{LayerPando: true}})
	if len(ch.Edits) != 0 {
		t.Fatalf("second plan: %d edits", len(ch.Edits))
	}
}

func TestPandoCodexBlockIsFoundByLineNotBySubstring(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	p := filepath.Join(home, ".codex/config.toml")
	// A comment that names the table is not the table.
	os.WriteFile(p, []byte("# add [mcp_servers.pando] later\nmodel = \"x\"\n"), 0o600)
	e, err := PlanPandoCodexMCP(p, "/opt/bin/daisugi", nil)
	if err != nil || e == nil || strings.Count(e.Content, "\n[mcp_servers.pando]\n") != 1 {
		t.Fatalf("the comment hid the block: %v %+v", err, e)
	}
	// Remove: the block's own sub-table goes with it, a following array of
	// tables stays.
	os.WriteFile(p, []byte("model = \"x\"\n\n[mcp_servers.pando]\ncommand = \"d\"\n\n[mcp_servers.pando.env]\nA = \"1\"\n\n[[profiles]]\nname = \"p\"\n"), 0o600)
	r, err := PlanPandoCodexMCPRemove(p, nil)
	if err != nil || r == nil || strings.Contains(r.Content, "pando") || !strings.Contains(r.Content, "[[profiles]]") || !strings.Contains(r.Content, `model = "x"`) {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestPandoLayerFollowsHermesHome(t *testing.T) {
	home := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "hh")
	os.MkdirAll(elsewhere, 0o700)
	t.Setenv("HERMES_HOME", elsewhere)
	got := hermesHomes(home)
	if len(got) == 0 || got[0] != elsewhere {
		t.Fatalf("hermesHomes = %v", got)
	}
}

func TestPandoUnitOfTheOwnerIsBackedUpAndNeverRemoved(t *testing.T) {
	home := t.TempDir()
	p := PandoUnitPath(home)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("[Service]\nExecStart=/my/own\n"), 0o600)
	e, err := PlanPandoUnit(home, "/opt/bin/daisugi")
	if err != nil || e == nil {
		t.Fatalf("%v %+v", err, e)
	}
	if err := applyPandoEdit(e, nil); err != nil {
		t.Fatal(err)
	}
	baks, _ := filepath.Glob(p + ".bak*")
	if len(baks) != 1 {
		t.Fatalf("the owner's unit was not backed up: %v", baks)
	}
	if b, _ := os.ReadFile(baks[0]); !strings.Contains(string(b), "/my/own") {
		t.Fatalf("backup: %s", b)
	}
	// An owner's file that is not ours is left alone by uninstall.
	os.WriteFile(p, []byte("[Service]\nExecStart=/my/own\n"), 0o600)
	rv, _ := PlanReverseAll(Runtimes[0], home, map[string]bool{}, map[string]bool{LayerPando: true})
	for _, ed := range rv.Edits {
		if ed.Kind == "pando-unit-remove" {
			t.Fatal("uninstall would remove a unit that is not ours")
		}
	}
}

func TestPandoUnitHookEnablesOnInstallAndDisablesOnUninstall(t *testing.T) {
	home := scratchHome(t)
	var calls []string
	UnitHook = func(action, unit string) error { calls = append(calls, action+" "+unit); return nil }
	defer func() { UnitHook = nil }()
	runOnly(t, Runtimes[0], home)
	if strings.Join(calls, ",") != "enable "+PandoUnitName {
		t.Fatalf("install calls: %v", calls)
	}
	calls = nil
	rv, _ := PlanReverseAll(Runtimes[0], home, map[string]bool{}, map[string]bool{LayerPando: true})
	if _, err := ApplyChange(rv); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "disable "+PandoUnitName+",reload "+PandoUnitName {
		t.Fatalf("uninstall calls: %v", calls)
	}
}

func TestDedupeUnitEditsKeepsTheUnitOnce(t *testing.T) {
	home := scratchHome(t)
	var changes []*Change
	for _, rt := range []Runtime{Runtimes[0], Runtimes[2]} {
		ch, err := PlanInstall(rt, home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{Only: map[string]bool{LayerPando: true}})
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, ch)
	}
	DedupeUnitEdits(changes)
	n := 0
	for _, ch := range changes {
		for _, e := range ch.Edits {
			if e.Kind == "pando-unit" {
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("the unit is planned %d times", n)
	}
}

func TestPandoIsAComponentSoOnlyCanNameIt(t *testing.T) {
	got, err := ComponentLayers([]string{"pando"})
	if err != nil || !got[LayerPando] {
		t.Fatalf("%v %v", got, err)
	}
	if !strings.Contains(ListComponents(), "pando") {
		t.Fatal("install --list must show pando")
	}
}

// Two layers write the same file (config.yaml, config.toml): the second plans
// on what the first wrote, so a default install keeps both.
func TestADefaultInstallKeepsTheOpendaisugiAndThePandoEntriesInOneFile(t *testing.T) {
	home := scratchHome(t)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	for _, rt := range []Runtime{Runtimes[1], Runtimes[2]} { // codex, hermes
		ch, err := PlanInstall(rt, home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{})
		if err != nil || ch.Failed {
			t.Fatalf("%s: %v %+v", rt.Key, err, ch)
		}
		if _, err := ApplyChange(ch); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".hermes/config.yaml"))
	toml, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	for _, want := range []string{"opendaisugi", "pando"} {
		if !strings.Contains(string(cfg), want) || !strings.Contains(string(toml), want) {
			t.Fatalf("lost %s:\n%s\n---\n%s", want, cfg, toml)
		}
	}
	if !strings.Contains(string(cfg), "pre_tool_call") {
		t.Fatalf("lost the capture hook:\n%s", cfg)
	}
	// A second run changes nothing, and an uninstall of everything leaves neither.
	for _, rt := range []Runtime{Runtimes[1], Runtimes[2]} {
		// The two shared config files are not planned again. (The skill copy and
		// the instructions file are always planned, and no-ops when current.)
		ch2, _ := PlanInstall(rt, home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{})
		for _, e := range ch2.Edits {
			if strings.HasSuffix(e.Path, "config.yaml") || strings.HasSuffix(e.Path, "config.toml") || strings.HasPrefix(e.Kind, "pando") {
				t.Fatalf("%s second plan holds %s [%s]", rt.Key, e.Path, e.Kind)
			}
		}
		rv, err := PlanReverseAll(rt, home, map[string]bool{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyChange(rv); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ = os.ReadFile(filepath.Join(home, ".hermes/config.yaml"))
	toml, _ = os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if strings.Contains(string(cfg), "pando") || strings.Contains(string(cfg), "opendaisugi") ||
		strings.Contains(string(toml), "pando") || strings.Contains(string(toml), "opendaisugi") {
		t.Fatalf("an entry is left:\n%s\n---\n%s", cfg, toml)
	}
}
