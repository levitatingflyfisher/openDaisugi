package install

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// treeOf is every file, link and empty directory under root, with a hash
// of each file: the manifest the modularity check on the box compares.
// Backups (*.bak<ns>) are listed apart.
func treeOf(t *testing.T, root string) (tree []string, backups []string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if strings.Contains(filepath.Base(p), ".bak") {
			backups = append(backups, rel)
			return nil
		}
		switch {
		case d.IsDir():
			if ents, _ := os.ReadDir(p); len(ents) == 0 {
				tree = append(tree, "emptydir "+rel)
			}
		case d.Type()&fs.ModeSymlink != 0:
			tree = append(tree, "link "+rel)
		default:
			b, _ := os.ReadFile(p)
			s := sha256.Sum256(b)
			tree = append(tree, hex.EncodeToString(s[:8])+" "+rel)
		}
		return nil
	})
	sort.Strings(tree)
	sort.Strings(backups)
	return tree, backups
}

// freshClaudeHome is a home as Claude Code 2.1 leaves it after its first
// run: ~/.claude with no settings.json, ~/.claude.json with no final
// newline, and an empty ~/.config.
func freshClaudeHome(t *testing.T) string {
	h := t.TempDir()
	for _, d := range []string{".claude/projects", ".config", ".local/share"} {
		if err := os.MkdirAll(filepath.Join(h, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(h, ".claude/projects/x.jsonl"), []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(h, ".claude.json"), []byte("{\n  \"numStartups\": 2,\n  \"userID\": \"u\"\n}"), 0o600)
	return h
}

func installAll(t *testing.T, rt Runtime, home string) *Change {
	t.Helper()
	ch, err := PlanInstall(rt, home, "/opt/bin/daisugi", filepath.Join(home, ".local/share/opendaisugi/warden"), Opts{})
	if err != nil || ch.Failed {
		t.Fatalf("plan: %v %+v", err, ch)
	}
	if _, err := ApplyChange(ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

func uninstallAll(t *testing.T, rt Runtime, home string) {
	t.Helper()
	rv, err := PlanReverseAll(rt, home, map[string]bool{}, nil)
	if err != nil || rv.Failed {
		t.Fatalf("reverse: %v %+v", err, rv)
	}
	if _, err := ApplyChange(rv); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSkillGoesWhereClaudeCodeReadsSkills(t *testing.T) {
	home := freshClaudeHome(t)
	installAll(t, Runtimes[0], home)
	if _, err := os.Stat(filepath.Join(home, ".claude/skills", SkillName, "SKILL.md")); err != nil {
		t.Fatalf("the skill is not in ~/.claude/skills: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".agents")); err == nil {
		t.Fatal("install wrote ~/.agents, which Claude Code does not read")
	}
}

func TestClaudeUninstallLeavesTheConfigAsInstallFoundIt(t *testing.T) {
	home := freshClaudeHome(t)
	before, _ := treeOf(t, home)
	installAll(t, Runtimes[0], home)
	uninstallAll(t, Runtimes[0], home)
	after, backups := treeOf(t, home)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("the tree changed:\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	// Backups are restore points and stay; each file gets one per run.
	seen := map[string]int{}
	for _, b := range backups {
		seen[b[:strings.Index(b, ".bak")]]++
	}
	for f, n := range seen {
		if n > 2 { // at most one per run; a file install made has none (TestNoBackupHoldsOnlyDaisugisOwnText)
			t.Errorf("%s has %d backups after one install and one uninstall: %v", f, n, backups)
		}
	}
}

func TestClaudeUninstallKeepsTheUsersOwnSettings(t *testing.T) {
	home := freshClaudeHome(t)
	own := "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"other-memory-tool stop\"\n          }\n        ]\n      }\n    ]\n  }\n}\n"
	settings := filepath.Join(home, ".claude/settings.json")
	os.WriteFile(settings, []byte(own), 0o600)
	os.MkdirAll(filepath.Join(home, ".claude/skills/theirs"), 0o700)
	installAll(t, Runtimes[0], home)
	uninstallAll(t, Runtimes[0], home)
	got, err := os.ReadFile(settings)
	if err != nil || string(got) != own {
		t.Fatalf("the user's settings.json changed: %v\n%s", err, got)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills/theirs")); err != nil {
		t.Fatal("uninstall removed a skills dir that holds the user's own skill")
	}
}

func TestOneInstallRunBacksUpEachFileOnce(t *testing.T) {
	home := freshClaudeHome(t)
	installAll(t, Runtimes[0], home)
	_, backups := treeOf(t, home)
	seen := map[string]int{}
	for _, b := range backups {
		seen[b[:strings.Index(b, ".bak")]]++
	}
	for f, n := range seen {
		if n != 1 {
			t.Errorf("%s: %d backups in one run: %v", f, n, backups)
		}
	}
	if seen[".claude.json"] != 1 {
		t.Errorf("~/.claude.json was not backed up: %v", backups)
	}
}

func TestHarnessUninstallRemovesTheDirItMadeAndNothingElse(t *testing.T) {
	for _, name := range []string{"pi", "opencode"} {
		home := t.TempDir()
		env := map[string]string{}
		os.MkdirAll(filepath.Join(home, ".pi/agent"), 0o700)
		os.MkdirAll(filepath.Join(home, ".config/opencode"), 0o700)
		before, _ := treeOf(t, home)
		if _, err := InstallHarness(name, home, env); err != nil {
			t.Fatal(err)
		}
		if _, err := UninstallHarness(name, home, env); err != nil {
			t.Fatal(err)
		}
		after, _ := treeOf(t, home)
		if strings.Join(before, "\n") != strings.Join(after, "\n") {
			t.Fatalf("%s: before:\n%s\nafter:\n%s", name, strings.Join(before, "\n"), strings.Join(after, "\n"))
		}
		// A dir that holds the user's own plugin stays.
		InstallHarness(name, home, env)
		mine := filepath.Join(wardenroot2(Target(name, home, env)), "mine.ts")
		if name == "pi" {
			mine = filepath.Join(home, ".pi/agent/extensions/mine/index.ts")
			os.MkdirAll(filepath.Dir(mine), 0o700)
		}
		os.WriteFile(mine, []byte("x"), 0o600)
		UninstallHarness(name, home, env)
		if _, err := os.Stat(mine); err != nil {
			t.Fatalf("%s: uninstall removed the user's own plugin", name)
		}
	}
}

func wardenroot2(p string) string { return filepath.Dir(p) }

func TestClaudeInstallWithNoClaudeDirWritesNothing(t *testing.T) {
	home := t.TempDir()
	ch, err := PlanInstall(Runtimes[0], home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Failed {
		t.Fatalf("no ~/.claude, yet the runtime did not fail: %+v", ch)
	}
	if _, err := ApplyChange(ch); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Fatalf("a failed runtime wrote %v", ents)
	}
}

// A symlinked skills dir (a dotfiles repo, say) is the user's: uninstall
// takes the daisugi skill out through the link and leaves the link and the
// user's other skills.
func TestUninstallNeverUnlinksASymlinkedDir(t *testing.T) {
	home := freshClaudeHome(t)
	dots := filepath.Join(t.TempDir(), "dotfiles-skills")
	os.MkdirAll(filepath.Join(dots, "theirs"), 0o700)
	os.WriteFile(filepath.Join(dots, "theirs", "SKILL.md"), []byte("x"), 0o600)
	link := filepath.Join(home, ".claude/skills")
	if err := os.Symlink(dots, link); err != nil {
		t.Fatal(err)
	}
	installAll(t, Runtimes[0], home)
	if _, err := os.Stat(filepath.Join(dots, SkillName, "SKILL.md")); err != nil {
		t.Fatalf("the skill did not go through the link: %v", err)
	}
	// Make the link's target empty but for the daisugi skill: the prune
	// must still stop at the link.
	os.RemoveAll(filepath.Join(dots, "theirs"))
	uninstallAll(t, Runtimes[0], home)
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("uninstall removed the symlinked skills dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dots, SkillName)); err == nil {
		t.Fatal("the daisugi skill is still in the link's target")
	}

	// pi: a symlinked extensions dir stays too.
	home2 := t.TempDir()
	ext := filepath.Join(t.TempDir(), "pi-ext")
	os.MkdirAll(ext, 0o700)
	os.MkdirAll(filepath.Join(home2, ".pi/agent"), 0o700)
	os.Symlink(ext, filepath.Join(home2, ".pi/agent/extensions"))
	if _, err := InstallHarness("pi", home2, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := UninstallHarness("pi", home2, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(filepath.Join(home2, ".pi/agent/extensions")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pi uninstall removed the symlinked extensions dir: %v", err)
	}
}

// Claude Code writes ~/.claude.json with JSON.stringify, which keeps
// UTF-8. A rewrite must not turn it into \u escapes.
func TestNonASCIIConfigRoundTripsByteForByte(t *testing.T) {
	home := freshClaudeHome(t)
	cj := "{\n  \"numStartups\": 2,\n  \"oauthAccount\": {\n    \"displayName\": \"José Ñandú\"\n  }\n}"
	st := "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"echo 🔥\"\n          }\n        ]\n      }\n    ]\n  }\n}\n"
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(cj), 0o600)
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte(st), 0o600)
	installAll(t, Runtimes[0], home)
	for f, want := range map[string]string{".claude.json": "José Ñandú", ".claude/settings.json": "echo 🔥"} {
		got, _ := os.ReadFile(filepath.Join(home, f))
		if !strings.Contains(string(got), want) {
			t.Fatalf("install escaped %s:\n%s", f, got)
		}
	}
	uninstallAll(t, Runtimes[0], home)
	for f, want := range map[string]string{".claude.json": cj, ".claude/settings.json": st} {
		got, _ := os.ReadFile(filepath.Join(home, f))
		if string(got) != want {
			t.Fatalf("%s changed:\nwant %q\ngot  %q", f, want, got)
		}
	}
}

// A file install created has no backup from the install run: its only
// content is daisugi's own. Nor does the uninstall that removes it back
// it up. A file the user had keeps one backup per run.
func TestNoBackupHoldsOnlyDaisugisOwnText(t *testing.T) {
	home := freshClaudeHome(t)
	installAll(t, Runtimes[0], home)
	uninstallAll(t, Runtimes[0], home)
	_, backups := treeOf(t, home)
	seen := map[string]int{}
	for _, b := range backups {
		seen[b[:strings.Index(b, ".bak")]]++
	}
	for _, f := range []string{".claude/settings.json", ".claude/CLAUDE.md"} {
		if seen[f] != 0 {
			t.Errorf("%s did not exist before install, yet has %d backups: %v", f, seen[f], backups)
		}
	}
	if seen[".claude.json"] != 2 {
		t.Errorf("the user's ~/.claude.json should have one backup per run (2): %v", backups)
	}
}

func TestNoClaudeDirSaysClaudeCodeHasNotRun(t *testing.T) {
	home := t.TempDir()
	ch, _ := PlanInstall(Runtimes[0], home, "/opt/bin/daisugi", filepath.Join(home, "w"), Opts{})
	if !ch.Failed || !strings.Contains(ch.Why, "Claude Code has not run yet") || !strings.Contains(ch.Why, ".claude is missing") {
		t.Fatalf("why: %q", ch.Why)
	}
}

// Codex reads ~/.agents/skills. A Claude uninstall leaves Codex's copy.
func TestClaudeUninstallLeavesCodexsSkill(t *testing.T) {
	home := freshClaudeHome(t)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	installAll(t, Runtimes[1], home) // codex
	codexSkill := filepath.Join(home, ".agents/skills", SkillName, "SKILL.md")
	if _, err := os.Stat(codexSkill); err != nil {
		t.Fatalf("codex skill not written: %v", err)
	}
	installAll(t, Runtimes[0], home)
	uninstallAll(t, Runtimes[0], home)
	if _, err := os.Stat(codexSkill); err != nil {
		t.Fatalf("the Claude uninstall removed Codex's skill: %v", err)
	}
}

// An older Claude install left the skill in ~/.agents/skills. With no
// Codex there, install takes that orphan out.
func TestClaudeInstallRemovesItsOldSkillWhenNoCodex(t *testing.T) {
	home := freshClaudeHome(t)
	old := filepath.Join(home, ".agents/skills", SkillName)
	os.MkdirAll(old, 0o700)
	os.WriteFile(filepath.Join(old, "SKILL.md"), []byte("old"), 0o600)
	installAll(t, Runtimes[0], home)
	if _, err := os.Stat(old); err == nil {
		t.Fatal("the old skill in ~/.agents/skills is still there")
	}
	// With Codex installed it is Codex's: left alone.
	home2 := freshClaudeHome(t)
	os.MkdirAll(filepath.Join(home2, ".codex"), 0o700)
	old2 := filepath.Join(home2, ".agents/skills", SkillName)
	os.MkdirAll(old2, 0o700)
	installAll(t, Runtimes[0], home2)
	if _, err := os.Stat(old2); err != nil {
		t.Fatal("install removed Codex's skill")
	}
}
