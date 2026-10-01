package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReportsMissingWithoutError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, ok, err := Load()
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := Config{Default: "claude", Harness: map[string]Harness{"claude": {Command: "claude", State: "hooks"}}}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load()
	if err != nil || !ok || got.Default != "claude" || got.Harness["claude"].Command != "claude" {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
	st, _ := os.Stat(Path())
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestPathHonoursXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got, want := Path(), filepath.Join(dir, "coppice", "coppice.toml"); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := Path(); !strings.HasSuffix(got, filepath.Join(".config", "coppice", "coppice.toml")) {
		t.Fatalf("Path() = %q, want the ~/.config fallback", got)
	}
}

func TestSaveWritesTheCommentLineFirst(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "pi"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(string(b), "\n", 2)[0]
	if first != "# coppice config. Edit it and save. The next pane reads it." {
		t.Fatalf("first line = %q", first)
	}
	if !strings.Contains(string(b), `default = "pi"`) {
		t.Fatalf("body lacks the default: %q", b)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "foreman") || strings.HasPrefix(line, "plugins") {
			t.Fatalf("an empty key was written: %q", line)
		}
	}
}

func TestSaveKeepsTheDirectoryPrivate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "pi"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Dir(Path()))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", st.Mode().Perm())
	}
}

func TestLoadReturnsAParseErrorWithOkFalse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte("default = [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := Load()
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v, want ok=false and a parse error", ok, err)
	}
}

func TestProjectsRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := Config{Default: "claude", Projects: []string{"/work/a", "/work/b"}}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load()
	if err != nil || !ok || len(got.Projects) != 2 || got.Projects[0] != "/work/a" || got.Projects[1] != "/work/b" {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
}

func TestSaveWritesNoProjectsKeyWhenNoneIsSet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "pi"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "projects") {
		t.Fatalf("an empty projects key was written: %q", b)
	}
}

func TestSaveWritesNoKeysTableWhenNoneIsSet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "pi"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "keys") {
		t.Fatalf("an empty keys table was written: %q", b)
	}
}

func TestGatewayURLDefaultsToDaisugisOwnAddress(t *testing.T) {
	if got := (Config{}).GatewayURL(); got != "http://127.0.0.1:8787" {
		t.Fatalf("GatewayURL() = %q", got)
	}
	none := ""
	if got := (Config{Gateway: &none}).GatewayURL(); got != "" {
		t.Fatalf("gateway = \"\" gave %q, want no gateway", got)
	}
	mine := "http://127.0.0.1:9999"
	if got := (Config{Gateway: &mine}).GatewayURL(); got != mine {
		t.Fatalf("GatewayURL() = %q, want %q", got, mine)
	}
}

// FN-R-33: the fence is opt-in. With no fence key a harness starts with no
// fence; "on" and "auto" still turn it on.
func TestTheFenceIsOffUnlessTheConfigTurnsItOn(t *testing.T) {
	if got := (Config{}).FenceMode(); got != "off" {
		t.Errorf("no fence key: %q, want off", got)
	}
	for _, v := range []string{"on", "auto", "off"} {
		v := v
		if got := (Config{Fence: &v}).FenceMode(); got != v {
			t.Errorf("fence = %q: %q", v, got)
		}
	}
}

// An unknown fence value is a config error naming the value and the ones
// allowed, never a silent "off" (FN-R-33).
func TestAnUnknownFenceValueIsAConfigError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Dir(Path()), 0o700)
	os.WriteFile(Path(), []byte("fence = \"yes\"\n"), 0o600)
	_, _, err := Load()
	if err == nil || err.Error() != `fence must be "on", "off" or "auto", not "yes".` {
		t.Fatalf("err %v", err)
	}
}

// FN-R-37: the fence key takes only the strings "on", "off" and "auto".
// Any other shape, and a file that does not parse while it holds a fence
// key, is the same config error: never a silent no-fence.
func TestEveryOtherFenceShapeIsAConfigError(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"fence = on\n", `fence must be "on", "off" or "auto", not on.`},
		{"fence = true\n", `fence must be "on", "off" or "auto", not true.`},
		{"fence = 1\n", `fence must be "on", "off" or "auto", not 1.`},
		{"fence = \"on\"\nbroken = \n", "the file does not parse, and it sets fence"},
	} {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		os.MkdirAll(filepath.Dir(Path()), 0o700)
		os.WriteFile(Path(), []byte(c.text), 0o600)
		_, _, err := Load()
		if err == nil || !errors.Is(err, ErrBadFence) || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%q: err %v", c.text, err)
		}
	}
	// A file that does not parse and sets no fence is the old error.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Dir(Path()), 0o700)
	os.WriteFile(Path(), []byte("broken = \n"), 0o600)
	if _, _, err := Load(); err == nil || errors.Is(err, ErrBadFence) {
		t.Errorf("no fence key: err %v", err)
	}
	// "fence" in a table is not the top-level key.
	os.WriteFile(Path(), []byte("[harness.x]\ncommand = \"x\"\nfence = 1\n"), 0o600)
	if _, _, err := Load(); errors.Is(err, ErrBadFence) {
		t.Errorf("table key: err %v", err)
	}
}
