package fence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reaches says whether a rule grants path, or a parent of it.
func reaches(out Output, path string) bool {
	for _, r := range out.Rules {
		if r.Path == path || strings.HasPrefix(path, r.Path+"/") {
			return true
		}
	}
	return false
}

// linkedHome builds a fake HOME under a temp dir with an envelope that names
// the data dir and the stores, as the spelling the user wrote and as the
// resolved one. link sets up the links; it returns the real data dir.
func linkedHome(t *testing.T, link func(home, base string) string) (Input, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	ws := filepath.Join(home, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	realData := link(home, base)
	envFile := filepath.Join(base, "env.json")
	body := `{"permissions":{"file_read":["~/.opendaisugi/pando/**","~/.opendaisugi/**","` + base + `/r/**","` + base + `/elsewhere/**"],"file_write":[]}}`
	if err := os.WriteFile(envFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	in, _, err := Gather(Options{Harness: "claude", Workspace: ws, Envelope: envFile, Cwd: ws,
		Environ: []string{"HOME=" + home, "PATH=/usr/bin"}, Pid: 4242, Tmpdir: filepath.Join(base, "tmp")})
	if err != nil {
		t.Fatal(err)
	}
	return in, realData
}

// Review F3: the stores stay shut when the data dir is a link.
func TestMemoryStaysShutUnderALinkedDataDir(t *testing.T) {
	in, realData := linkedHome(t, func(home, base string) string {
		rd := filepath.Join(base, "r", "ods")
		for _, m := range Memory {
			if err := os.MkdirAll(filepath.Join(rd, m, "work"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(rd, filepath.Join(home, ".opendaisugi")); err != nil {
			t.Fatal(err)
		}
		return rd
	})
	out := Plan(in)
	for _, m := range Memory {
		if reaches(out, realData+"/"+m+"/work/x") {
			t.Errorf("%s reachable through the resolved data dir: %v", m, out.Rules)
		}
	}
	if len(out.Refusals) != 0 {
		t.Errorf("refusals: %v", out.Refusals)
	}
	g := NewGuard(in)
	for _, m := range Memory {
		if why := g.Check(realData+"/"+m, "ro", "x"); why == "" {
			t.Errorf("guard lets %s through", m)
		}
	}
}

// Review F3: the stores stay shut when a store is itself a link.
func TestMemoryStaysShutWhenTheStoreIsALink(t *testing.T) {
	in, target := linkedHome(t, func(home, base string) string {
		target := filepath.Join(base, "elsewhere", "p")
		if err := os.MkdirAll(filepath.Join(target, "work"), 0o755); err != nil {
			t.Fatal(err)
		}
		data := filepath.Join(home, ".opendaisugi")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(data, "pando")); err != nil {
			t.Fatal(err)
		}
		return target
	})
	out := Plan(in)
	if reaches(out, target+"/work/x") {
		t.Errorf("the linked store is reachable: %v", out.Rules)
	}
	if why := NewGuard(in).Check(target, "ro", "x"); why == "" {
		t.Error("guard lets the linked store through")
	}
}
