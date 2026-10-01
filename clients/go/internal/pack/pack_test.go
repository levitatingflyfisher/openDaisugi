package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repo = "../../../.."

// The embedded files are copies of the repository's (a test checks the
// copy, as internal/catalog does).
func TestAssetsAreCopies(t *testing.T) {
	pairs := map[string]string{
		"assets/catalog.json":  "packs/catalog.json",
		"assets/train.lock":    "packs/train.lock",
		"assets/vla-ref.lock":  "packs/vla-ref.lock",
		"assets/worker.py":     "src/opendaisugi/pack/worker.py",
		"assets/lora_train.py": "src/opendaisugi/lora/train.py",
		"assets/vla_oracle.py": "src/opendaisugi/pack/vla_oracle.py",
	}
	for mine, theirs := range pairs {
		a, err := os.ReadFile(mine)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(repo, theirs))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Errorf("%s is not a copy of %s", mine, theirs)
		}
	}
}

func TestDefaultCatalog(t *testing.T) {
	cat, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range cat.Packs {
		names = append(names, p.Name)
		if !p.GPU {
			if _, err := cat.LockText(p); err != nil {
				t.Errorf("%s: %v", p.Name, err)
			}
		}
	}
	if strings.Join(names, " ") != "train train-cuda vla-ref vla-ref-cuda" {
		t.Errorf("names %v", names)
	}
	if cat.Protocol != Protocol || len(cat.Python.Sha256) != 64 {
		t.Errorf("catalog %+v", cat.Python)
	}
}

func TestParseLock(t *testing.T) {
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	text := "# a comment\nFoo_Bar==1.0 \\\n    --hash=sha256:" + a + " \\\n    --hash=sha256:" + b +
		"\n    # via baz\ntorch==2.14.1+cpu \\\n    --hash=sha256:" + c + "\n"
	reqs, err := ParseLock(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 || reqs[0].Name != "foo-bar" || reqs[0].Version != "1.0" ||
		strings.Join(reqs[0].Hashes, ",") != a+","+b || reqs[1].Name != "torch" || reqs[1].Version != "2.14.1+cpu" {
		t.Errorf("reqs %+v", reqs)
	}
	for _, bad := range []string{
		"foo==1.0\n",
		"foo>=1.0 --hash=sha256:" + a + "\n",
		"foo==1.0 ; sys_platform == 'win32' --hash=sha256:" + a + "\n",
		"foo==1.0 --hash=md5:abc\n",
	} {
		if _, err := ParseLock(bad); err == nil {
			t.Errorf("no error for %q", bad)
		}
	}
}

func TestWheelKey(t *testing.T) {
	if Normalize("Foo_Bar.baz") != "foo-bar-baz" {
		t.Error("normalize")
	}
	n, v, ok := WheelKey("torch-2.14.1+cpu-cp312-cp312-manylinux_2_28_x86_64.whl")
	if !ok || n != "torch" || v != "2.14.1+cpu" {
		t.Error("torch")
	}
	n, v, ok = WheelKey("Foo_Bar-1.0-1-py3-none-any.whl")
	if !ok || n != "foo-bar" || v != "1.0" {
		t.Error("build tag")
	}
	if _, _, ok := WheelKey("notawheel.tar.gz"); ok {
		t.Error("not a wheel")
	}
}

// The same bytes as opendaisugi.pack.ustar.write.
func TestUstarWrite(t *testing.T) {
	files := []File{{"wheels/" + strings.Repeat("x", 95) + ".whl", []byte("one")}, {"b.txt", []byte("two")}}
	b, err := UstarWrite(files)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if len(b) != 3072 || hex.EncodeToString(sum[:]) != "cf85a0b218a0e9bb759e89ad547d576f370f2f0e4b73d4083eb0270a2b5ca3ab" {
		t.Errorf("ustar %d %x", len(b), sum)
	}
	got, err := UstarRead(b)
	if err != nil || len(got) != 2 || got[0].Name != "b.txt" || string(got[1].Data) != "one" {
		t.Errorf("read back %v %v", got, err)
	}
	// A name longer than the ustar fields goes in an extended header.
	long := "wheels/" + strings.Repeat("y", 120) + "-1.0-py3-none-any.whl"
	pax, err := UstarWrite([]File{{long, []byte("long")}, {"a.txt", []byte("a")}})
	if err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(pax)
	if hex.EncodeToString(sum[:]) != "4fd858c9e651dd835f78a85eff780cabdb930685c815c6a734f6c50551747c08" {
		t.Errorf("pax %x", sum)
	}
	if got, err := UstarRead(pax); err != nil || len(got) != 2 || got[1].Name != long {
		t.Errorf("pax read back %v %v", got, err)
	}
	evil, _ := UstarWrite([]File{{"a", nil}})
	copy(evil[0:], "../evil\x00")
	fixChecksum(evil[:512])
	if _, err := UstarRead(evil); err == nil {
		t.Error("an escaping name")
	}
}

func TestUnpackFakeCPython(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repo, "clients/fixtures/pack/assets/cpython-fake-x86_64-linux.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := unpackTarGz(data, dest, "x.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(filepath.Join(dest, "python/bin/python")); err != nil || l != "python3" {
		t.Errorf("link %q %v", l, err)
	}
	long := filepath.Join(dest, "python/share/fake", strings.Repeat("d", 60), strings.Repeat("f", 40)+".txt")
	if _, err := os.Stat(long); err != nil {
		t.Error(err)
	}
	if st, err := os.Stat(filepath.Join(dest, "python/bin/python3")); err != nil || st.Mode()&0o111 == 0 {
		t.Error("python3 is not executable")
	}
}

func fakePack(t *testing.T, worker string) string {
	d := filepath.Join(t.TempDir(), "packs", "t")
	if err := os.MkdirAll(filepath.Join(d, "venv/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/python3", filepath.Join(d, "venv/bin/python")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if worker == "" {
		worker = WorkerPy
	}
	if err := os.WriteFile(filepath.Join(d, "worker", WorkerFile), []byte(worker), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

var testEnv = []string{"PATH=/usr/bin:/bin", "DAISUGI_PACK_TEST_JOBS=1"}

func TestRunJob(t *testing.T) {
	d := fakePack(t, "")
	var seen []string
	o := RunJob(d, "t", "echo", []string{"a", "b"}, func(s string) { seen = append(seen, s) }, testEnv)
	if o.Code != 0 || o.Result == nil || strings.Join(seen, ",") != "a,b" {
		t.Errorf("echo %+v %v", o, seen)
	}
	o = RunJob(d, "t", "nope", nil, func(string) {}, testEnv)
	if o.Code != 1 || o.Error != "pack t: nope: no job named nope in this worker" {
		t.Errorf("nope %+v", o)
	}
	o = RunJob(d, "t", "die", []string{"5"}, func(string) {}, testEnv)
	if o.Code != 1 || o.Error != "pack t: the worker exited 5: die: asked to exit" {
		t.Errorf("die %+v", o)
	}
}

func TestRunJobBadWorkers(t *testing.T) {
	d := fakePack(t, "import json,sys\nprint(json.dumps({\"ready\": \"daisugi-pack-0\"}), flush=True)\nsys.stdin.read()\n")
	o := RunJob(d, "t", "echo", nil, func(string) {}, testEnv)
	if o.Error != "pack t: the worker speaks daisugi-pack-0, not daisugi-pack-1. Install it again: daisugi pack install t --force" {
		t.Errorf("protocol %+v", o)
	}
	d = fakePack(t, "import json,sys\nprint(json.dumps({\"ready\": \"daisugi-pack-1\"}), flush=True)\nsys.stdin.buffer.read(4)\nprint('hello', flush=True)\nsys.stdin.read()\n")
	o = RunJob(d, "t", "echo", nil, func(string) {}, testEnv)
	if o.Error != "pack t: the worker wrote a line that is not a reply: hello" {
		t.Errorf("not a reply %+v", o)
	}
}
