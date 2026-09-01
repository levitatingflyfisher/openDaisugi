package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "coppice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAMissingPluginsKeyYieldsTheDefaultList(t *testing.T) {
	writeConfig(t, "default = \"claude\"\n")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.EnabledPlugins(), DefaultPlugins()) {
		t.Fatalf("%v", c.EnabledPlugins())
	}
}

func TestNoConfigFileYieldsTheDefaultList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c, ok, err := Load()
	if err != nil || ok {
		t.Fatal(ok, err)
	}
	if !reflect.DeepEqual(c.EnabledPlugins(), DefaultPlugins()) {
		t.Fatalf("%v", c.EnabledPlugins())
	}
}

func TestAnEmptyPluginsListTurnsEverythingOff(t *testing.T) {
	writeConfig(t, "plugins = []\n")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.EnabledPlugins(); got == nil || len(got) != 0 {
		t.Fatalf("%#v", got)
	}
}

// A command that loads the file and saves it again must keep an empty
// list empty, not bring it back as the default list.
func TestSaveKeepsAnEmptyPluginsList(t *testing.T) {
	writeConfig(t, "plugins = []\n")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Default = "pi"
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	again, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := again.EnabledPlugins(); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestSaveLeavesAMissingPluginsKeyMissing(t *testing.T) {
	writeConfig(t, "default = \"claude\"\n")
	c, _, _ := Load()
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	again, _, _ := Load()
	if again.Plugins != nil {
		t.Fatalf("%v", *again.Plugins)
	}
}

func TestAnExplicitListIsUsedAsItIs(t *testing.T) {
	writeConfig(t, "plugins = [\"tree\", \"close-quiet\"]\n")
	c, _, _ := Load()
	if got := c.EnabledPlugins(); !reflect.DeepEqual(got, []string{"tree", "close-quiet"}) {
		t.Fatalf("%v", got)
	}
}

func TestAPluginTableKeepsItsSettings(t *testing.T) {
	writeConfig(t, "[plugin.turn-budget]\nbudget = 12\n\n[plugin.notify-ntfy]\nurl = \"https://ntfy.example\"\ntopic = \"t\"\n")
	c, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Plugin["turn-budget"]["budget"] != int64(12) || c.Plugin["notify-ntfy"]["topic"] != "t" {
		t.Fatalf("%#v", c.Plugin)
	}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	again, _, _ := Load()
	if again.Plugin["notify-ntfy"]["url"] != "https://ntfy.example" {
		t.Fatalf("%#v", again.Plugin)
	}
}
