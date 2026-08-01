package sprig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
