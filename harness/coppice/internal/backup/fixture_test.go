package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// coppiceSpec has the shape `coppice backup` has: three areas and its
// allow-list.
func coppiceSpec(data, cfg string) Spec {
	return Spec{Tool: "coppice", Version: "9.9",
		Roots: map[string]string{"data": data, "foreman": filepath.Join(data, "foreman"), "config": cfg},
		Units: []Unit{{"data", "layout.json"}, {"data", "recent-dirs.json"}, {"foreman", "main"}, {"foreman", "tree"},
			{"foreman", "view.json"}, {"config", "coppice.toml"}, {"config", "pools.toml"}}}
}

func coppiceFiles(t *testing.T, root, who string) {
	write(t, root+"/d/layout.json", "{\"from\":\""+who+"\"}\n")
	write(t, root+"/d/foreman/main/2026-10-24.jsonl", "{}\n")
	write(t, root+"/d/foreman/tree/a.jsonl", "{}\n")
	write(t, root+"/d/foreman/view.json", "{}")
	write(t, root+"/c/coppice.toml", "fence = \"auto\"\n")
}

// A fixture archive made by the Rust coppice reads here. The Rust coppice
// reads testdata/go-made.tgz in the same way. MAKE_FIXTURE=<dir> writes a
// new go-made.tgz there.
func TestReadsTheArchiveTheRustCoppiceMade(t *testing.T) {
	dst := t.TempDir()
	r, err := Restore(coppiceSpec(dst+"/d", dst+"/c"), "testdata/rust-made.tgz", false, t0)
	if err != nil || r.Files != 5 {
		t.Fatalf("%v %+v", err, r)
	}
	for p, want := range map[string]string{"d/layout.json": "{\"from\":\"rust\"}\n", "d/foreman/tree/a.jsonl": "{}\n", "c/coppice.toml": "fence = \"auto\"\n"} {
		if b, _ := os.ReadFile(filepath.Join(dst, p)); string(b) != want {
			t.Errorf("%s = %q", p, b)
		}
	}
}

func TestMakeFixture(t *testing.T) {
	dir := os.Getenv("MAKE_FIXTURE")
	if dir == "" {
		t.Skip("MAKE_FIXTURE not set")
	}
	src := t.TempDir()
	coppiceFiles(t, src, "go")
	if _, err := Create(coppiceSpec(src+"/d", src+"/c"), filepath.Join(dir, "go-made.tgz"), t0); err != nil {
		t.Fatal(err)
	}
}
