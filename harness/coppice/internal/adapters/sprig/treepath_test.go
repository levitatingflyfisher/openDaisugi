package sprig

import "testing"

func TestTreePathJoinsTheSessionDirAndID(t *testing.T) {
	p, ok := TreePath([]string{"sprig", "--session-dir", "/fake/dir", "--", "task"}, "abc123")
	if !ok || p != "/fake/dir/abc123.jsonl" {
		t.Fatalf("TreePath = %q %v", p, ok)
	}
	p, ok = TreePath([]string{"sprig", "--session-dir=/fake/dir"}, "abc123")
	if !ok || p != "/fake/dir/abc123.jsonl" {
		t.Fatalf("TreePath = %q %v", p, ok)
	}
}

func TestTreePathRefusesWhatIsNotItsOwnFile(t *testing.T) {
	for _, c := range []struct {
		argv []string
		id   string
	}{
		{[]string{"sprig"}, "abc"},
		{[]string{"sprig", "--session-dir", "rel/dir"}, "abc"},
		{[]string{"sprig", "--session-dir", "/fake"}, ""},
		{[]string{"sprig", "--session-dir", "/fake"}, "../abc"},
		{[]string{"sprig", "--session-dir", "/fake"}, "a/b"},
		{[]string{"sprig", "--session-dir", "/fake"}, ".."},
		{[]string{"sprig", "--", "--session-dir", "/fake"}, "abc"},
	} {
		if p, ok := TreePath(c.argv, c.id); ok {
			t.Errorf("TreePath(%v, %q) = %q, want a refusal", c.argv, c.id, p)
		}
	}
}
