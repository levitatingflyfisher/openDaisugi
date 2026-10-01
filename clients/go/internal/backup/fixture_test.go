package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// A fixture archive made by the Rust port reads here. The Rust port reads
// testdata/go-made.tgz in the same way. MAKE_FIXTURE=<dir> writes a new
// go-made.tgz there.
func TestReadsTheArchiveTheRustPortMade(t *testing.T) {
	dst := t.TempDir()
	r, err := Restore(spec(dst), "testdata/rust-made.tgz", false, t0)
	if err != nil || r.Files != 3 {
		t.Fatalf("%v %+v", err, r)
	}
	for p, want := range map[string]string{"config.yaml": "from: rust\n", "journal/traces/a.json": "{}\n", "gate/envelopes/s.json": "{\"e\":1}\n"} {
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
	write(t, src+"/config.yaml", "from: go\n")
	write(t, src+"/journal/traces/a.json", "{}\n")
	write(t, src+"/gate/envelopes/s.json", "{\"e\":1}\n")
	if _, err := Create(spec(src), filepath.Join(dir, "go-made.tgz"), t0); err != nil {
		t.Fatal(err)
	}
}
