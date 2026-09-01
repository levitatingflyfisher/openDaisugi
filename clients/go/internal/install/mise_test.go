package install

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// miseTree makes a mise data directory with daisugi installed at one
// version, and returns the data dir and the installed binary.
func miseTree(t *testing.T) (data, bin string) {
	t.Helper()
	data = filepath.Join(t.TempDir(), "mise")
	bin = filepath.Join(data, "installs", "github-openDaisugi", "0.44.0", "daisugi")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return data, bin
}

func TestHookPathOutsideMiseKeepsSelf(t *testing.T) {
	data, _ := miseTree(t)
	other := filepath.Join(t.TempDir(), "daisugi")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, versioned := HookPath(other, map[string]string{"MISE_DATA_DIR": data}, nil)
	if p != other || versioned {
		t.Fatalf("got %q %v", p, versioned)
	}
}

func TestHookPathUsesTheShimWhenItRunsThisBinary(t *testing.T) {
	data, bin := miseTree(t)
	shim := filepath.Join(data, "shims", "daisugi")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MISE_DATA_DIR": data}
	p, versioned := HookPath(bin, env, func(string) (string, error) { return bin + "\n", nil })
	if p != shim || versioned {
		t.Fatalf("got %q %v, want the shim", p, versioned)
	}
	// A shim that runs another version, or a mise that fails, keeps the
	// versioned path and says so.
	other := filepath.Join(data, "installs", "github-openDaisugi", "0.45.0", "daisugi")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if p, versioned := HookPath(bin, env, func(string) (string, error) { return other, nil }); p != bin || !versioned {
		t.Fatalf("other version: %q %v", p, versioned)
	}
	if p, versioned := HookPath(bin, env, func(string) (string, error) { return "", errors.New("no mise") }); p != bin || !versioned {
		t.Fatalf("mise failed: %q %v", p, versioned)
	}
}

func TestHookPathWithNoShimKeepsTheVersionedPath(t *testing.T) {
	data, bin := miseTree(t)
	p, versioned := HookPath(bin, map[string]string{"MISE_DATA_DIR": data}, func(string) (string, error) {
		return "", errors.New("no mise")
	})
	if p != bin || !versioned {
		t.Fatalf("got %q %v", p, versioned)
	}
}

func TestMiseDirsFollowTheDocumentedOrder(t *testing.T) {
	cases := []struct {
		env         map[string]string
		data, shims string
	}{
		{map[string]string{"HOME": "/h"}, "/h/.local/share/mise", "/h/.local/share/mise/shims"},
		{map[string]string{"HOME": "/h", "XDG_DATA_HOME": "/x"}, "/x/mise", "/x/mise/shims"},
		{map[string]string{"HOME": "/h", "MISE_DATA_DIR": "/m"}, "/m", "/m/shims"},
		{map[string]string{"HOME": "/h", "MISE_SHIMS_DIR": "/s"}, "/h/.local/share/mise", "/s"},
	}
	for _, c := range cases {
		if d, s := MiseDirs(c.env); d != c.data || s != c.shims {
			t.Fatalf("%v: %q %q", c.env, d, s)
		}
	}
}

// writeStub writes an Omarchy-style mise stub named daisugi in dir.
func writeStub(t *testing.T, dir, tool string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "daisugi")
	body := "#!/bin/bash\nmise use -g --quiet " + tool + " >/dev/null\nexec mise x " + tool + " -- daisugi \"$@\"\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// With no shim, an Omarchy stub on PATH in ~/.local/bin whose mise which
// names this binary is the hook's program; the shim still comes first.
func TestHookPathUsesAnOmarchyStubAfterTheShim(t *testing.T) {
	data, bin := miseTree(t)
	home := t.TempDir()
	const tool = "github:levitatingflyfisher/openDaisugi"
	local := filepath.Join(home, ".local", "bin")
	stub := writeStub(t, local, tool)
	env := map[string]string{"MISE_DATA_DIR": data, "HOME": home, "PATH": local + ":/usr/bin"}
	var asked []string
	which := func(tl string) (string, error) {
		asked = append(asked, tl)
		if tl == tool {
			return bin, nil
		}
		return "", errors.New("daisugi is not a global tool")
	}
	if p, versioned := HookPath(bin, env, which); p != stub || versioned {
		t.Fatalf("got %q %v, want the stub", p, versioned)
	}
	if len(asked) == 0 || asked[len(asked)-1] != tool {
		t.Fatalf("the stub's tool was not asked: %v", asked)
	}
	// Off PATH, the stub is not used.
	env["PATH"] = "/usr/bin"
	if p, versioned := HookPath(bin, env, which); p != bin || !versioned {
		t.Fatalf("stub off PATH: %q %v", p, versioned)
	}
	// $XDG_BIN_HOME comes first.
	xdg := filepath.Join(home, "xbin")
	xstub := writeStub(t, xdg, tool)
	env["XDG_BIN_HOME"], env["PATH"] = xdg, xdg+":"+local
	if p, _ := HookPath(bin, env, which); p != xstub {
		t.Fatalf("got %q, want the XDG_BIN_HOME stub", p)
	}
	// A shim that runs this binary comes before any stub.
	shim := filepath.Join(data, "shims", "daisugi")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	always := func(string) (string, error) { return bin, nil }
	if p, _ := HookPath(bin, env, always); p != shim {
		t.Fatalf("got %q, want the shim first", p)
	}
	// A stub whose tool runs another binary is not used.
	if err := os.Remove(shim); err != nil {
		t.Fatal(err)
	}
	other := func(string) (string, error) { return "/nowhere/daisugi", nil }
	if p, versioned := HookPath(bin, env, other); p != bin || !versioned {
		t.Fatalf("stub for another binary: %q %v", p, versioned)
	}
}

func TestMiseStubReadsOnlyAMiseExecLine(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "PATH": local}
	for body, want := range map[string]string{
		"#!/bin/sh\nexec mise x github:a/b -- daisugi \"$@\"\n":    "github:a/b",
		"#!/bin/sh\nexec mise exec 'aqua:c/d' -- daisugi \"$@\"\n": "aqua:c/d",
		"#!/bin/sh\nexec /opt/daisugi \"$@\"\n":                    "",
		"\x7fELF binary":                                           "",
	} {
		if err := os.WriteFile(filepath.Join(local, "daisugi"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, tool := MiseStub(env); tool != want {
			t.Fatalf("%q: tool %q, want %q", body, tool, want)
		}
	}
}
