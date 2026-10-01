package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

// F8: sprig is on PATH after every install but has no model. The picker
// labels it, and labels nothing else.
func TestThePickerLabelsSprigAsNeedingAModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	_, err := FirstRun([]config.Found{{Name: "claude", Path: "/bin/claude"},
		{Name: "sprig", Path: "/bin/sprig"}}, strings.NewReader("1\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "  2. sprig    /bin/sprig  (needs a model: see docs/how-to/foreman-any-model.md)\n") {
		t.Fatalf("sprig is not labeled: %q", s)
	}
	if !strings.Contains(s, "  1. claude    /bin/claude\n") {
		t.Fatalf("claude line changed: %q", s)
	}
}

func TestSprigAloneIsNamedWithItsNote(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	if _, err := FirstRun([]config.Found{{Name: "sprig", Path: "/bin/sprig"}}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sprig is your default. Enter opens it here.  (needs a model: see docs/how-to/foreman-any-model.md)\n") {
		t.Fatalf("no note: %q", out.String())
	}
}
