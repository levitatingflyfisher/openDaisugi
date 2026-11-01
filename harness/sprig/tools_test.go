package sprig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadTool(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("hello\nworld\n"), 0o644)

	out, err := ReadTool{}.Run(map[string]any{"path": p})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello\nworld\n" {
		t.Fatalf("got %q", out)
	}
}

func TestWriteTool(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "b.txt") // parent must be created

	_, err := WriteTool{}.Run(map[string]any{"path": p, "content": "written"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "written" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditToolReplacesUniqueString(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.txt")
	os.WriteFile(p, []byte("the quick brown fox"), 0o644)

	_, err := EditTool{}.Run(map[string]any{"path": p, "old": "brown", "new": "red"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "the quick red fox" {
		t.Fatalf("got %q", string(got))
	}
}

func TestEditToolRefusesAmbiguousOrMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.txt")
	os.WriteFile(p, []byte("a a a"), 0o644)

	if _, err := (EditTool{}).Run(map[string]any{"path": p, "old": "a", "new": "b"}); err == nil {
		t.Fatal("edit must refuse a non-unique match (safety, like the real editor)")
	}
	if _, err := (EditTool{}).Run(map[string]any{"path": p, "old": "zzz", "new": "b"}); err == nil {
		t.Fatal("edit must error when the old string is absent")
	}
}

func TestBashTool(t *testing.T) {
	out, err := BashTool{}.Run(map[string]any{"cmd": "echo sprig123"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sprig123") {
		t.Fatalf("got %q", out)
	}
}

func TestDefaultToolsAreTheFour(t *testing.T) {
	tools := DefaultTools()
	for _, name := range []string{"read", "write", "edit", "bash"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing default tool %q", name)
		}
	}
	if len(tools) != 4 {
		t.Fatalf("pi-minimal: want exactly 4 tools, got %d", len(tools))
	}
}

// The step cap is per call: three calls of 0.4 s each pass under a 1 s cap,
// though together they run past it (ledger 2026-11-06: the whole run died
// at the envelope's 30 s).
func TestThreeShortStepsUnderTheStepCapPass(t *testing.T) {
	b := BashTool{StepCap: time.Second}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if out, err := b.Run(map[string]any{"cmd": "sleep 0.4; echo ok"}); err != nil || !strings.Contains(out, "ok") {
			t.Fatalf("step %d: %q, %v", i, out, err)
		}
	}
	if time.Since(start) < time.Second {
		t.Fatal("the three steps ran in under the cap together; the test proves nothing")
	}
}

// One slow call is killed at the cap, with its group, and fails with a line
// that names the call.
func TestASlowStepIsKilledAtTheStepCapAndNamed(t *testing.T) {
	b := BashTool{StepCap: time.Second}
	start := time.Now()
	_, err := b.Run(map[string]any{"cmd": "sleep 30 & sleep 30; echo done"})
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the step ran %s", took)
	}
	want := `the step "sleep 30 & sleep 30; echo done" ran past the step cap of 1s and was killed`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
}

func TestTheStepCapComesFromTheEnvironment(t *testing.T) {
	for v, want := range map[string]int{"": 0, "45": 45, " 7 ": 7, "-3": 0, "x": 0} {
		t.Setenv(StepCapEnv, v)
		if got := stepCapFromEnv(); got != want {
			t.Errorf("%q: %d", v, got)
		}
	}
	if b, ok := ToolsWithStepCap([]string{"bash"}, 3*time.Second)["bash"].(BashTool); !ok || b.StepCap != 3*time.Second {
		t.Fatalf("bash = %#v", b)
	}
}
