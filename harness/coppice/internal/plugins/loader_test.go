package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// writePlugin writes one plugin directory named id under a fresh root and
// returns the root. The directory holds the manifest and every file the
// manifest names as page or run.
func writePlugin(t *testing.T, id, manifest string) string {
	t.Helper()
	root := t.TempDir()
	addPlugin(t, root, id, manifest)
	return root
}

func addPlugin(t *testing.T, root, id, manifest string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"i.html", "index.html", "m.py", "run.sh"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// writePlugins writes one view per id under one root.
func writePlugins(t *testing.T, ids ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, id := range ids {
		addPlugin(t, root, id, `{"id":"`+id+`","kind":"view","page":"index.html","title":"`+id+`"}`)
	}
	return root
}

func TestLoadRefusesTheReservedPrefixFromUserDirs(t *testing.T) {
	dir := writePlugin(t, "coppice.evil", `{"id":"coppice.evil","kind":"view","page":"i.html"}`)
	_, probs := Load([]string{dir}, []string{"coppice.evil"})
	if len(probs) != 1 || !strings.Contains(probs[0].Message, "reserved") {
		t.Fatalf("%+v", probs)
	}
}

func TestAPolicyCannotAskForAllow(t *testing.T) {
	dir := writePlugin(t, "merge", `{"id":"merge","kind":"policy","run":"m.py","needs":["pane.read","agent.allow"]}`)
	_, probs := Load([]string{dir}, []string{"merge"})
	if len(probs) != 1 || !strings.Contains(probs[0].Message, "cannot allow") {
		t.Fatalf("%+v", probs)
	}
}

func TestAPolicyCannotAskForDeny(t *testing.T) {
	dir := writePlugin(t, "nope", `{"id":"nope","kind":"policy","run":"m.py","needs":["agent.deny"]}`)
	_, probs := Load([]string{dir}, []string{"nope"})
	if len(probs) != 1 || probs[0].Message != "a plugin can propose. It cannot allow." {
		t.Fatalf("%+v", probs)
	}
}

func TestOnlyEnabledIdsLoad(t *testing.T) {
	dir := writePlugins(t, "a", "b")
	ps, _ := Load([]string{dir}, []string{"b"})
	if len(ps) != 1 || ps[0].ID != "b" {
		t.Fatalf("%+v", ps)
	}
}

func TestAnEnabledIdWithNoDirectoryIsAProblem(t *testing.T) {
	dir := writePlugins(t, "a")
	ps, probs := Load([]string{dir}, []string{"a", "ghost"})
	if len(ps) != 1 || len(probs) != 1 || probs[0].ID != "ghost" || !strings.Contains(probs[0].Message, "no plugin directory") {
		t.Fatalf("%+v %+v", ps, probs)
	}
}

func TestLoadRefusesBadManifests(t *testing.T) {
	cases := []struct {
		name, id, manifest, want string
	}{
		{"bad id", "Bad_Id", `{"id":"Bad_Id","kind":"view","page":"i.html"}`, "id must"},
		{"id differs from dir", "one", `{"id":"two","kind":"view","page":"i.html"}`, "directory"},
		{"view with no page", "v", `{"id":"v","kind":"view"}`, "a view needs page"},
		{"policy with no run", "p", `{"id":"p","kind":"policy"}`, "a policy needs run"},
		{"unknown kind", "k", `{"id":"k","kind":"daemon","run":"m.py"}`, "kind must be view or policy"},
		{"page escapes", "e", `{"id":"e","kind":"view","page":"../i.html"}`, "inside the plugin directory"},
		{"absolute run", "r", `{"id":"r","kind":"policy","run":"/bin/sh"}`, "inside the plugin directory"},
		{"missing file", "f", `{"id":"f","kind":"policy","run":"gone.py"}`, "no file gone.py"},
		{"frame listen", "l", `{"id":"l","kind":"policy","run":"m.py","listens":["frame"]}`, "listens"},
		{"view with needs", "vn", `{"id":"vn","kind":"view","page":"i.html","needs":["pane.read"]}`, "a view holds no verbs"},
		{"unknown field", "u", `{"id":"u","kind":"view","page":"i.html","neds":[]}`, "manifest.json"},
		{"not json", "j", `{"id":`, "manifest.json"},
		{"report state", "rs", `{"id":"rs","kind":"policy","run":"m.py","needs":["pane.report_state"]}`, "reports no state"},
		{"odd verb", "o", `{"id":"o","kind":"policy","run":"m.py","needs":["Pane Close"]}`, "not a verb name"},
		{"policy with ring", "pr", `{"id":"pr","kind":"policy","run":"m.py","ring":true}`, "only a view reads the ring"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writePlugin(t, c.id, c.manifest)
			ps, probs := Load([]string{dir}, []string{c.id})
			if len(ps) != 0 || len(probs) != 1 || !strings.Contains(probs[0].Message, c.want) {
				t.Fatalf("want one problem with %q, got %+v %+v", c.want, ps, probs)
			}
		})
	}
}

func TestShippedSourceMayUseTheReservedPrefix(t *testing.T) {
	fsys := fstest.MapFS{
		"coppice.core/manifest.json": {Data: []byte(`{"id":"coppice.core","kind":"view","page":"index.html"}`)},
		"coppice.core/index.html":    {Data: []byte("x")},
	}
	ps, probs := LoadFrom([]Source{{FS: fsys, Shipped: true}}, []string{"coppice.core"})
	if len(probs) != 0 || len(ps) != 1 || !ps[0].Shipped {
		t.Fatalf("%+v %+v", ps, probs)
	}
}

// A directory of the operator's may not take a shipped id. One write there
// would otherwise swap the code of a plugin that runs by default. The
// shipped plugin loads, and the directory is a problem.
func TestAUserDirectoryMayNotUseAShippedId(t *testing.T) {
	fsys := fstest.MapFS{
		"tree/manifest.json": {Data: []byte(`{"id":"tree","kind":"view","page":"index.html","title":"shipped"}`)},
		"tree/index.html":    {Data: []byte("x")},
	}
	root := t.TempDir()
	addPlugin(t, root, "tree", `{"id":"tree","kind":"view","page":"index.html","title":"mine"}`)
	ps, probs := LoadFrom([]Source{{FS: fsys, Shipped: true}, {Dir: root}}, []string{"tree"})
	if len(ps) != 1 || ps[0].Title != "shipped" || !ps[0].Shipped || len(probs) != 1 ||
		probs[0].Dir != filepath.Join(root, "tree") || !strings.Contains(probs[0].Message, "the shipped plugin tree") {
		t.Fatalf("%+v %+v", ps, probs)
	}
}

func TestPluginsComeInTheEnabledOrderOnce(t *testing.T) {
	dir := writePlugins(t, "a", "b", "c")
	ps, probs := Load([]string{dir}, []string{"c", "a", "c"})
	if len(probs) != 0 || len(ps) != 2 || ps[0].ID != "c" || ps[1].ID != "a" {
		t.Fatalf("%+v %+v", ps, probs)
	}
}

func TestAPolicyKeepsItsListensNeedsAndConfig(t *testing.T) {
	dir := writePlugin(t, "turns", `{"id":"turns","kind":"policy","run":"m.py","listens":["state"],`+
		`"needs":["pane.send_keys","floor.note"],"config":{"budget":40},"about":"pauses a long run"}`)
	ps, probs := Load([]string{dir}, []string{"turns"})
	if len(probs) != 0 || len(ps) != 1 {
		t.Fatalf("%+v %+v", ps, probs)
	}
	p := ps[0]
	if strings.Join(p.Listens, ",") != "state" || strings.Join(p.Needs, ",") != "pane.send_keys,floor.note" ||
		p.Config["budget"] != float64(40) || p.About != "pauses a long run" {
		t.Fatalf("%+v", p)
	}
}

func TestAPageLinkedOutOfTheDirectoryIsRefused(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.html")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	addPlugin(t, root, "leak", `{"id":"leak","kind":"view","page":"secret.html"}`)
	if err := os.Symlink(outside, filepath.Join(root, "leak", "secret.html")); err != nil {
		t.Fatal(err)
	}
	ps, probs := Load([]string{root}, []string{"leak"})
	if len(ps) != 0 || len(probs) != 1 {
		t.Fatalf("%+v %+v", ps, probs)
	}
}

// A view that asks for the event ring is served with that ask, and one
// that does not ask is served without it.
func TestAViewCarriesItsAskForTheRing(t *testing.T) {
	root := t.TempDir()
	addPlugin(t, root, "hist", `{"id":"hist","kind":"view","page":"index.html","ring":true}`)
	addPlugin(t, root, "plain", `{"id":"plain","kind":"view","page":"index.html"}`)
	ps, probs := Load([]string{root}, []string{"hist", "plain"})
	if len(probs) != 0 {
		t.Fatalf("%+v", probs)
	}
	vs := ViewsOf(ps)
	if len(vs) != 2 || !vs[0].Ring || vs[1].Ring {
		t.Fatalf("%+v", vs)
	}
}
