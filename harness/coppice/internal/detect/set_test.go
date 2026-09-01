package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LoadSet's override path had no test at all beyond
// LoadSet(""). This exercises both halves at once: a valid override that
// must win over the bundled manifest of the same id, and a syntactically
// broken override that must not -- the bundled manifest stays in place, with
// a warning naming the broken file.
func TestLoadSetOverridesReplaceByIDAndWarnOnBrokenFiles(t *testing.T) {
	dir := t.TempDir()

	// A valid claude override: a rule only this file declares, so a match on
	// it proves the override is what's actually active, not just present.
	validOverride := "id = \"claude\"\n" +
		"aliases = [\"claude-code\"]\n" +
		"[[rules]]\n" +
		"id = \"override_marker\"\n" +
		"state = \"working\"\n" +
		"priority = 5000\n" +
		"contains = [\"OVERRIDE_MARKER_TEXT\"]\n"
	overridePath := filepath.Join(dir, "claude.toml")
	if err := os.WriteFile(overridePath, []byte(validOverride), 0o644); err != nil {
		t.Fatal(err)
	}

	// A syntactically broken codex override: an unknown top-level key.
	brokenPath := filepath.Join(dir, "codex.toml")
	broken := "id = \"codex\"\n" +
		"this_is_not_a_real_key = true\n" +
		"[[rules]]\n" +
		"id = \"r\"\n" +
		"contains = [\"a\"]\n"
	if err := os.WriteFile(brokenPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSet(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The override wins for claude.
	claude, ok := s.For("claude")
	if !ok {
		t.Fatal("claude not found after override load")
	}
	if r := claude.Evaluate(Input{Screen: "OVERRIDE_MARKER_TEXT"}); !r.Matched ||
		r.RuleID != "override_marker" {
		t.Fatalf("the override did not win for claude: %+v", r)
	}
	if got := s.Source("claude"); got != overridePath {
		t.Fatalf("Source(claude) = %q, want the override path %q", got, overridePath)
	}

	// The bundled manifest's own alias still resolves through the override,
	// since the override redeclares it (a realistic edited-copy override).
	viaAlias, ok := s.For("claude-code")
	if !ok {
		t.Fatal("For(claude-code) did not resolve")
	}
	if viaAlias != claude {
		t.Fatal("For(claude-code) resolved to a different *Compiled than For(claude)")
	}

	// codex's broken override is skipped; the bundled manifest is untouched.
	if _, ok := s.For("codex"); !ok {
		t.Fatal("codex is missing entirely: a broken override must not take the bundled manifest down with it")
	}
	if got := s.Source("codex"); got != "bundled" {
		t.Fatalf("Source(codex) = %q, want %q (the broken override must not win)", got, "bundled")
	}

	warnings := s.Warnings()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, brokenPath) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings() = %v, want one naming the broken file's full path %q", warnings, brokenPath)
	}
}

// An override directory that exists but can't be read
// (permission denied, not "doesn't exist") must not be silently ignored --
// LoadSet must still succeed (the bundled set is unaffected) but warn,
// naming the directory, exactly like a broken individual override file does.
func TestLoadSetWarnsOnAnUnreadableOverrideDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't block root's own reads")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755) // let t.TempDir's own cleanup remove it

	s, err := LoadSet(locked)
	if err != nil {
		t.Fatalf("LoadSet must fail closed with a warning, not a hard error: %v", err)
	}
	if len(s.Agents()) != 21 {
		t.Fatalf("%d agents loaded, want the full bundled 21 despite the unreadable override dir",
			len(s.Agents()))
	}
	found := false
	for _, w := range s.Warnings() {
		if strings.Contains(w, locked) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings() = %v, want one naming the unreadable directory %q", s.Warnings(), locked)
	}
}

// LoadSet("") -- no override directory configured at all -- must not warn.
// This is the common case (most installs have no override file) and the
// existing os.IsNotExist(err) path must stay silent for it.
func TestLoadSetWithNoOverrideDirConfiguredWarnsOfNothing(t *testing.T) {
	s, err := LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	if w := s.Warnings(); len(w) != 0 {
		t.Fatalf("Warnings() = %v, want none", w)
	}
}

// A configured but nonexistent override directory (the common real-world
// shape: the operator never created ~/.config/coppice/agent-detection/) must
// also stay silent -- os.IsNotExist is not itself a problem.
func TestLoadSetWithANonexistentOverrideDirWarnsOfNothing(t *testing.T) {
	s, err := LoadSet(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if w := s.Warnings(); len(w) != 0 {
		t.Fatalf("Warnings() = %v, want none", w)
	}
}
