package foreman

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, coppiceDir string, argv ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Main(argv, coppiceDir, &out, &errw)
	return code, out.String(), errw.String()
}

func TestCLIReads(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "first\nline")
	mustAppend(t, c, KindForeman, "second")
	before := readAll(t, dir)

	code, out, _ := runCLI(t, "", "--data-dir", dir, "log")
	if code != 0 || out != "0 2026-10-08T23:58:00Z user|first line\n1 2026-10-08T23:58:00Z foreman|second\n" {
		t.Fatalf("log = %d %q", code, out)
	}
	code, out, _ = runCLI(t, "", "log", "--json", "--day", "2026-10-08", "--data-dir", dir)
	if code != 0 || strings.Count(out, "\n") != 2 || !strings.Contains(out, `"text":"first\nline"`) {
		t.Fatalf("log --json = %d %q", code, out)
	}
	code, out, _ = runCLI(t, "", "--data-dir", dir, "log", "--day", "2026-10-09")
	if code != 0 || out != "" {
		t.Fatalf("log of an empty day = %d %q", code, out)
	}
	code, out, _ = runCLI(t, "", "--data-dir", dir, "view")
	if code != 0 || out != c.Render() {
		t.Fatalf("view = %d %q", code, out)
	}
	code, out, _ = runCLI(t, "", "--data-dir", dir, "zoom", "0+2")
	if code != 0 || out != "0+1|first line\n1+1|second\n" {
		t.Fatalf("zoom 0+2 = %d %q", code, out)
	}
	code, out, _ = runCLI(t, "", "--data-dir", dir, "zoom", "0")
	if code != 0 || out != "first\nline\n" {
		t.Fatalf("zoom 0 = %d %q", code, out)
	}
	code, out, _ = runCLI(t, "", "--data-dir", dir, "date", "1")
	if code != 0 || out != "2026-10-08T23:58:00Z\n" {
		t.Fatalf("date 1 = %d %q", code, out)
	}
	if readAll(t, dir) != before {
		t.Fatal("the CLI changed the chat")
	}
}

func TestCLIErrors(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "x")
	for _, argv := range [][]string{
		{}, {"--data-dir", dir}, {"--data-dir", dir, "nope"}, {"--data-dir", dir, "zoom"},
		{"--data-dir", dir, "zoom", "0+3"}, {"--data-dir", dir, "zoom", "4+1"},
		{"--data-dir", dir, "date", "x"}, {"--data-dir", dir, "date", "9"},
		{"--data-dir", dir, "view", "extra"}, {"--data-dir", dir, "--bogus", "view"},
		{"--data-dir"}, {"--data-dir", filepath.Join(dir, "missing"), "view"},
		{"--data-dir", dir, "log", "--day", "bad"},
	} {
		if code, _, _ := runCLI(t, "", argv...); code != 1 {
			t.Errorf("%q exits %d, want 1", argv, code)
		}
	}
	if code, out, _ := runCLI(t, "", "--help"); code != 0 || !strings.Contains(out, "coppice foreman") {
		t.Fatalf("--help = %d %q", code, out)
	}
}

// coppice --data-dir X foreman reads X/foreman, never the owner's dir.
func TestCLIUsesCoppiceDataDir(t *testing.T) {
	top := t.TempDir()
	c := openT(t, filepath.Join(top, "foreman"), &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "here")
	code, out, _ := runCLI(t, top, "log")
	if code != 0 || !strings.Contains(out, "user|here") {
		t.Fatalf("log = %d %q", code, out)
	}
}

// With no dir given, the default dir is under the isolated test home.
func TestCLIDefaultDirIsUnderTheTestHome(t *testing.T) {
	t.Setenv("OPENDAISUGI_HOME", t.TempDir())
	d, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	c := openT(t, d, &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "home")
	if code, out, _ := runCLI(t, "", "view"); code != 0 || !strings.Contains(out, "0+1|home") {
		t.Fatalf("view = %d %q", code, out)
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatal(err)
	}
}
