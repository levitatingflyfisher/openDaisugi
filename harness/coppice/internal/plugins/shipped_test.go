package plugins

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
	shipped "github.com/opendaisugi/coppice/plugins"
)

// shippedIDs is every plugin directory the binary carries.
func shippedIDs(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(shipped.Files(), ".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestEveryShippedPluginLoads(t *testing.T) {
	ids := shippedIDs(t)
	ps, probs := LoadFrom([]Source{{FS: shipped.Files(), Shipped: true}}, ids)
	if len(probs) != 0 || len(ps) != len(ids) {
		t.Fatalf("%+v", probs)
	}
	for _, p := range ps {
		if p.About == "" || strings.Contains(p.About, "\n") {
			t.Errorf("plugin %s needs a one line about", p.ID)
		}
	}
}

func TestTheSharedLibraryShipsWithoutItsCache(t *testing.T) {
	lib := SharedLib()
	if lib == nil {
		t.Fatal("no shared library")
	}
	if _, err := fs.Stat(lib, "floor_client.py"); err != nil {
		t.Fatal(err)
	}
	_ = fs.WalkDir(shipped.Files(), ".", func(name string, d fs.DirEntry, err error) error {
		if strings.Contains(name, "__pycache__") || strings.HasSuffix(name, ".pyc") {
			t.Errorf("the binary carries %s", name)
		}
		return nil
	})
}

// The default list is every view the binary carries, plus notify-ntfy. A
// new shipped view joins the list with its directory. The policies ship
// off.
func TestTheDefaultListIsEveryShippedViewAndNotifyNtfy(t *testing.T) {
	ps, probs := LoadFrom([]Source{{FS: shipped.Files(), Shipped: true}}, shippedIDs(t))
	if len(probs) != 0 {
		t.Fatalf("%+v", probs)
	}
	want := map[string]bool{"notify-ntfy": true}
	for _, p := range ps {
		if p.Kind == KindView {
			want[p.ID] = true
		}
	}
	got := map[string]bool{}
	for _, id := range config.DefaultPlugins() {
		if got[id] {
			t.Errorf("%s is twice in the default list", id)
		}
		got[id] = true
	}
	if len(got) != len(want) {
		t.Fatalf("default list %v, want %v", config.DefaultPlugins(), want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("%s is missing from the default list", id)
		}
	}
	for _, p := range ps {
		if p.Kind == KindPolicy && got[p.ID] && p.ID != "notify-ntfy" {
			t.Errorf("policy %s is on by default", p.ID)
		}
	}
}
