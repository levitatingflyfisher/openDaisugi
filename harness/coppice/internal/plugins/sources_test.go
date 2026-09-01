package plugins

import (
	"path/filepath"
	"testing"
)

func TestTheShippedTreeViewLoads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ps, probs := LoadEnabled([]string{"tree"})
	if len(probs) != 0 || len(ps) != 1 || !ps[0].Shipped || ps[0].Kind != KindView {
		t.Fatalf("%+v %+v", ps, probs)
	}
}

func TestUserDirIsBesideTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got := UserDir(); got != filepath.Join(dir, "coppice", "plugins") {
		t.Fatal(got)
	}
}

func TestAUserViewLoadsFromTheUserDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	addPlugin(t, UserDir(), "mine", `{"id":"mine","kind":"view","page":"index.html","title":"Mine"}`)
	ps, probs := LoadEnabled([]string{"tree", "mine"})
	if len(probs) != 0 || len(ps) != 2 || ps[1].ID != "mine" || ps[1].Shipped {
		t.Fatalf("%+v %+v", ps, probs)
	}
	views := ViewsOf(ps)
	if len(views) != 2 || views[1].ID != "mine" || views[1].Title != "Mine" || views[1].Page != "index.html" {
		t.Fatalf("%+v", views)
	}
}
