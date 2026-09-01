package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

func TestProjectAddResolvesToAnAbsolutePath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(here, dir)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", rel)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	cfg, ok, err := config.Load()
	if err != nil || !ok {
		t.Fatalf("config load: %v %v", ok, err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0] != dir {
		t.Fatalf("projects = %v, want [%s]", cfg.Projects, dir)
	}
}

func TestProjectAddRefusesAMissingDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "nope")
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", missing)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, missing) {
		t.Fatalf("stderr = %q, want it to name %q", errb, missing)
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config was written for a missing directory")
	}
}

// A path that exists but is a file, not a directory, is a different
// mistake from a missing path, and gets a message that says so.
func TestProjectAddRefusesAFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", file)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, file) || !strings.Contains(errb, "is not a directory") {
		t.Fatalf("stderr = %q, want it to name %q and say it is not a directory", errb, file)
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config was written for a file")
	}
}

// project add resolves symlinks, so pinning a directory by its symlink and
// again by its real path (or the reverse) never lists it twice.
func TestProjectAddResolvesASymlinkSoItIsNeverListedTwice(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link-to-real")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink on this filesystem: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", link); code != 0 {
		t.Fatalf("add via symlink: exit %d: %s", code, errb)
	}
	if code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", real); code != 0 {
		t.Fatalf("add via real path: exit %d: %s", code, errb)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0] != resolved {
		t.Fatalf("projects = %v, want exactly [%s]", cfg.Projects, resolved)
	}
}

func TestProjectAddTwiceDoesNotDuplicate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", dir)
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	cfg, _, _ := config.Load()
	if len(cfg.Projects) != 1 {
		t.Fatalf("projects = %v, want one entry", cfg.Projects)
	}
}

func TestProjectAddKeepsTheRestOfAHandEditedFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	empty := []string{}
	if err := config.Save(config.Config{
		Default: "claude",
		Harness: map[string]config.Harness{
			"claude": {Command: "claude", Args: []string{"--flag"}, State: "hooks", ResumeArgs: []string{"--resume", "{session}"}},
		},
		Plugins: &empty,
		Plugin:  map[string]map[string]any{"notify-ntfy": {"topic": "x"}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	cfg, ok, err := config.Load()
	if err != nil || !ok {
		t.Fatalf("config load: %v %v", ok, err)
	}
	if cfg.Default != "claude" || cfg.Harness["claude"].Command != "claude" ||
		len(cfg.Harness["claude"].ResumeArgs) != 2 ||
		cfg.Plugin["notify-ntfy"]["topic"] != "x" {
		t.Fatalf("cfg after add = %+v, lost a hand-edited field", cfg)
	}
	if cfg.Plugins == nil || len(*cfg.Plugins) != 0 {
		t.Fatalf("plugins = %v, want the empty list kept", cfg.Plugins)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0] != dir {
		t.Fatalf("projects = %v", cfg.Projects)
	}
}

// project add rewrites the whole file, so a hand-added comment and a key
// coppice itself does not know about are both gone afterward - neither
// ever reaches the Config struct on Load, so there is nothing left to
// write back out.
func TestProjectAddDropsACommentAndAnUnknownKeyFromAHandEditedFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "# a hand-written note about this file\n" +
		"default = \"claude\"\n" +
		"some_future_key = \"kept by hand, not by coppice\"\n"
	if err := os.WriteFile(config.Path(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "add", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	b, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hand-written note") || strings.Contains(string(b), "some_future_key") {
		t.Fatalf("the rewrite kept what it should have dropped: %q", b)
	}
	cfg, ok, err := config.Load()
	if err != nil || !ok || cfg.Default != "claude" {
		t.Fatalf("config after add: %v %v %+v", ok, err, cfg)
	}
}

func TestProjectRmRemovesAPinnedProject(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	if err := config.Save(config.Config{Projects: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "rm", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	cfg, _, _ := config.Load()
	if len(cfg.Projects) != 0 {
		t.Fatalf("projects = %v, want none", cfg.Projects)
	}
}

// project rm resolves symlinks too, the same way add does - a project
// pinned by its real path is still found when the operator names it by a
// symlink to that same directory.
func TestProjectRmResolvesASymlinkToMatchTheStoredPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link-to-real")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink on this filesystem: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(config.Config{Projects: []string{resolved}}); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "rm", link)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	cfg, _, _ := config.Load()
	if len(cfg.Projects) != 0 {
		t.Fatalf("projects = %v, want none", cfg.Projects)
	}
}

func TestProjectRmOnAnUnknownDirectoryFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "project", "rm", t.TempDir())
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "project list") {
		t.Fatalf("stderr = %q, want it to point at project list", errb)
	}
}

func TestProjectAddWithRemoteIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "--remote", "ssh://host", "project", "add", dir)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "--remote") {
		t.Fatalf("stderr = %q, want it to name --remote", errb)
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config was written for a --remote project add")
	}
}

func TestProjectRmWithRemoteIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(), "--remote", "ssh://host", "project", "rm", t.TempDir())
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb, "--remote") {
		t.Fatalf("stderr = %q, want it to name --remote", errb)
	}
}

func TestProjectListPrintsATableAndJSON(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinned := t.TempDir()
	if err := config.Save(config.Config{Projects: []string{pinned}}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runCLI(t, sock, dir, "project", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, filepath.Base(pinned)) || !strings.Contains(out, pinned) {
		t.Fatalf("table = %q", out)
	}
	code, out, errb = runCLI(t, sock, dir, "project", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out, `"pinned":true`) {
		t.Fatalf("json = %q", out)
	}
}
