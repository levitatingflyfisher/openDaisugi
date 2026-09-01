package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

func TestOneHarnessAsksNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	c, err := FirstRun([]config.Found{{Name: "claude", Path: "/bin/claude", State: "hooks"}}, strings.NewReader(""), &out)
	if err != nil || c.Default != "claude" {
		t.Fatalf("%+v %v", c, err)
	}
	if strings.Contains(out.String(), "Which") {
		t.Fatal("asked a question with one harness")
	}
	if !strings.Contains(out.String(), "Enter opens claude here.") {
		t.Fatalf("%s", out.String())
	}
}

func TestTwoHarnessesAskOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	c, err := FirstRun([]config.Found{{Name: "claude"}, {Name: "pi"}}, strings.NewReader("2\n"), &out)
	if err != nil || c.Default != "pi" {
		t.Fatalf("%+v %v", c, err)
	}
	if strings.Count(out.String(), "?") != 1 {
		t.Fatalf("more than one question: %s", out.String())
	}
}

func TestNoHarnessIsHonest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	_, err := FirstRun(nil, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(out.String(), "apps") {
		t.Fatalf("%v %s", err, out.String())
	}
}

// With no harness found nothing is written.
func TestNoHarnessWritesNoFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	_, err := FirstRun(nil, strings.NewReader(""), &out)
	if err == nil || err.Error() != config.AppOnlyNote(nil) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config file was written with no harness found")
	}
}

// EOF on the answer takes the first harness and names the file to edit.
func TestEOFOnTheAnswerTakesTheFirstAndNamesTheFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	c, err := FirstRun([]config.Found{{Name: "claude"}, {Name: "pi"}}, strings.NewReader(""), &out)
	if err != nil || c.Default != "claude" {
		t.Fatalf("%+v %v", c, err)
	}
	want := "claude is your default. Edit " + config.Path() + " to change it."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("missing %q in %q", want, out.String())
	}
	if strings.Count(out.String(), "?") != 1 {
		t.Fatalf("more than one question: %s", out.String())
	}
}

// A number past the list takes the first harness.
func TestANumberPastTheListTakesTheFirst(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	c, err := FirstRun([]config.Found{{Name: "claude"}, {Name: "pi"}}, strings.NewReader("9\n"), &out)
	if err != nil || c.Default != "claude" {
		t.Fatalf("%+v %v", c, err)
	}
	if !strings.Contains(out.String(), "Edit "+config.Path()) {
		t.Fatalf("the fallback does not name the file: %q", out.String())
	}
}

// A pick writes the file with the default and one table per harness.
func TestAPickWritesTheConfigFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	_, err := FirstRun([]config.Found{{Name: "claude", Path: "/bin/claude", State: "hooks"},
		{Name: "pi", Path: "/bin/pi", State: "rpc"}}, strings.NewReader("2\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := config.Load()
	if err != nil || !ok {
		t.Fatalf("no config on disk: ok=%v err=%v", ok, err)
	}
	if got.Default != "pi" {
		t.Fatalf("default = %q", got.Default)
	}
	if got.Harness["claude"].Command != "claude" || got.Harness["pi"].Command != "pi" {
		t.Fatalf("tables = %+v", got.Harness)
	}
	if got.Harness["pi"].State != "rpc" {
		t.Fatalf("pi state = %q", got.Harness["pi"].State)
	}
}

// The list shows every harness with its number and path, in probe order,
// and the whole run ends with the three vocabulary lines.
func TestTheQuestionListsEveryHarnessAndEndsWithTheVocabulary(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	_, err := FirstRun([]config.Found{{Name: "claude", Path: "/bin/claude"},
		{Name: "pi", Path: "/usr/bin/pi"}}, strings.NewReader("1\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"Which harness should Enter open?\n",
		"  1. claude    /bin/claude\n",
		"  2. pi    /usr/bin/pi\n",
		"Pick a number [1]: ",
		"claude is your default. Enter opens it here.\n",
		"\nEnter opens claude here. n opens another.\nSpace peeks. ctrl-c quits.\nctrl-t, then words, to talk.\n",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %q", want, s)
		}
	}
	if !strings.HasSuffix(s, "ctrl-t, then words, to talk.\n") {
		t.Fatalf("does not end with the vocabulary: %q", s)
	}
}
