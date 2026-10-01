package cli

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/gate"
)

func run(t *testing.T, home string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(&Env{
		Args:    args,
		Stdin:   strings.NewReader(stdin),
		Stdout:  &out,
		Stderr:  &errb,
		Environ: []string{"HOME=" + home, "PATH=/nonexistent"},
	})
	return code, out.String(), errb.String()
}

// --version prints the version the build set, and with none set, what the
// build recorded, never a fixed release number.
func TestVersionComesFromTheBuild(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "9.9.9-test"
	if code, out, _ := run(t, t.TempDir(), "", "--version"); code != 0 || out != "9.9.9-test\n" {
		t.Fatalf("set version: exit %d, %q", code, out)
	}
	Version = ""
	if code, out, _ := run(t, t.TempDir(), "", "--version"); code != 0 || out == "\n" || out == "0.43.0\n" {
		t.Fatalf("unset version: exit %d, %q", code, out)
	}
}

// A command or flag the binary does not carry says so in one line, exits
// 2 and writes nothing.
func TestNotYetChangesNothing(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"dashboard", "--tui"}, {"dashboard", "--serve"}, {"gate", "audit"},
	} {
		code, out, errs := run(t, home, "", args...)
		if code != 2 || out != "" || !strings.HasSuffix(errs, "is not in this binary yet.\n") || strings.Count(errs, "\n") != 1 {
			t.Errorf("%v: exit %d out %q err %q", args, code, out, errs)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".claude"))
	if len(entries) != 0 {
		t.Fatalf("wrote %v", entries)
	}
}

func TestHelpListsWhatIsNotInTheBinary(t *testing.T) {
	code, out, _ := run(t, t.TempDir(), "", "--help")
	if code != 0 || !strings.Contains(out, "Not yet in this binary") || !strings.Contains(out, "registry") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{
		{"gate", "status", "--bogus"}, {"gate", "arm", "extra"}, {"gate", "register"},
		{"gate", "status", "--root"}, {"gate", "status", "--json=1"}, {"nope"}, {"gate", "nope"},
	} {
		if code, _, errs := run(t, home, "", args...); code != 2 || !strings.Contains(errs, "Error: ") {
			t.Errorf("%v: exit %d err %q", args, code, errs)
		}
	}
}

// gate check is the hook entry: whatever the environment, it answers
// exactly what the gate package answers, never a CLI refusal (an exit 2
// that would deny even in audit mode).
func TestGateCheckIsTheGatePackage(t *testing.T) {
	root := t.TempDir()
	args := []string{"gate", "check", "--mode", "audit", "--root", root, "--format", "claude"}
	payload := `{"session_id": "s1", "tool_name": "TodoWrite", "tool_input": {}, "cwd": "/work"}`
	for _, environ := range [][]string{{"PATH=/nonexistent"}, {"HOME=relative", "PATH=/nonexistent"}} {
		var out, errb bytes.Buffer
		code := Main(&Env{Args: args, Stdin: strings.NewReader(payload), Stdout: &out, Stderr: &errb, Environ: environ})
		want := gate.Run(args[2:], []byte(payload), environ)
		if code != want.Exit || out.String() != want.Stdout {
			t.Errorf("%v: got %d %q %q, gate.Run %d %q %q", environ, code, out.String(), errb.String(),
				want.Exit, want.Stdout, want.Stderr)
		}
		if strings.Contains(errb.String(), "Nothing was changed") {
			t.Errorf("%v: the CLI refused the hook call: %s", environ, errb.String())
		}
	}
}

func TestConfirm(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
		code int
	}{{"y\n", true, 0}, {"YES\n", true, 0}, {"n\n", false, 0}, {"\n", false, 0}, {"maybe\ny\n", true, 0}, {"", false, 1}} {
		var out, errb bytes.Buffer
		e := &Env{Stdin: strings.NewReader(c.in), Stdout: &out, Stderr: &errb}
		e.in = bufio.NewReader(e.Stdin)
		got, err := e.confirm("Proceed?")
		code := 0
		if err != nil {
			code = err.(*exitError).code
		}
		if got != c.want || code != c.code {
			t.Errorf("%q: %v %d", c.in, got, code)
		}
	}
}

func TestPathwaysListOnAFreshHome(t *testing.T) {
	home := t.TempDir()
	var out, errb strings.Builder
	code := Main(&Env{Args: []string{"pathways", "list", "--json"}, Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errb, Environ: []string{"HOME=" + home}})
	if code != 0 || out.String() != "[]\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
	// Python's PathwayStore creates the database; so does this binary.
	if _, err := os.Stat(home + "/.opendaisugi/pathways.db"); err != nil {
		t.Fatal(err)
	}
}

// DAISUGI_PORT hands install, with its arguments, to the port it names
// (PK-R-15); with no sibling of that name it stops with one line.
func TestInstallHandsOverToDaisugiPort(t *testing.T) {
	dir := t.TempDir()
	rs := filepath.Join(dir, "daisugi-rs")
	if err := os.WriteFile(rs, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var gotArgv, gotEnv []string
	var out, errb bytes.Buffer
	e := &Env{
		Args:     []string{"-q", "install", "--gate", "--dry-run"},
		Stdin:    strings.NewReader(""),
		Stdout:   &out,
		Stderr:   &errb,
		Environ:  []string{"HOME=" + dir, "PATH=/nonexistent", "DAISUGI_PORT=rust"},
		selfPath: filepath.Join(dir, "daisugi"),
		execFn: func(argv0 string, argv, env []string) error {
			gotArgv, gotEnv = argv, env
			return nil
		},
	}
	if code := Main(e); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := []string{rs, "-q", "install", "--gate", "--dry-run"}
	if strings.Join(gotArgv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv %q, want %q", gotArgv, want)
	}
	if gotEnv[len(gotEnv)-1] != "DAISUGI_PORT_HOP=1" {
		t.Fatalf("env %q", gotEnv)
	}
	e = &Env{
		Args: []string{"install", "--dry-run"}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Environ:  []string{"HOME=" + dir, "PATH=/nonexistent", "DAISUGI_PORT=python"},
		selfPath: filepath.Join(dir, "daisugi"),
	}
	errb.Reset()
	if code := Main(e); code != 2 || !strings.Contains(errb.String(), "daisugi: DAISUGI_PORT is python, but there is no daisugi-py beside") {
		t.Fatalf("missing sibling: exit %d, %q", code, errb.String())
	}
}
