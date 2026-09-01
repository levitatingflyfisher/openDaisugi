package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/plugins"
)

func TestLoadPluginsPrintsEachProblemAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "coppice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coppice", "coppice.toml"), []byte(`plugins = ["tree", "ghost"]`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	c := &CLI{Err: &errOut}
	ps := c.loadPlugins()
	if len(ps) != 1 || ps[0].ID != "tree" {
		t.Fatalf("%+v", ps)
	}
	if !strings.Contains(errOut.String(), "plugin ghost") {
		t.Fatalf("%q", errOut.String())
	}
}

func TestLoadPluginsRunsNoneWhenTheConfigDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	_ = os.MkdirAll(filepath.Join(dir, "coppice"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "coppice", "coppice.toml"), []byte("plugins = [\n"), 0o600)
	var errOut bytes.Buffer
	c := &CLI{Err: &errOut}
	if ps := c.loadPlugins(); len(ps) != 0 {
		t.Fatalf("%+v", ps)
	}
	if !strings.Contains(errOut.String(), "No plugin runs.") {
		t.Fatalf("%q", errOut.String())
	}
}

func TestPolicySpecsHoldOnlyPolicies(t *testing.T) {
	ps := []plugins.Plugin{
		{Manifest: plugins.Manifest{ID: "tree", Kind: plugins.KindView}},
		{Manifest: plugins.Manifest{ID: "turns", Kind: plugins.KindPolicy, Needs: []string{"floor.note"}, Listens: []string{"state"}}},
	}
	got := policySpecs(ps)
	if len(got) != 1 || got["turns"].Needs[0] != "floor.note" || got["turns"].Listens[0] != "state" {
		t.Fatalf("%+v", got)
	}
}
