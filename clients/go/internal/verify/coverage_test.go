package verify

import "testing"

func TestCoversTree(t *testing.T) {
	for _, c := range []struct {
		dir   string
		globs []string
		ok    bool
	}{
		{"/work/out", []string{"/work/**"}, true},
		{"/work", []string{"/work/**"}, true},
		{"/work/a/out", []string{"/work/*/out/**"}, true},
		{"/work/out", []string{"/work/*.py"}, false},
		{"/work/out", []string{"/work/out"}, false},
		{"/work/out", []string{"/work/out/*"}, false},
		{"/", []string{"/**"}, true},
		{"/", []string{"**"}, true},
		{"/etc", []string{"/work/**", "/home/**"}, false},
		{"out", []string{"out/**"}, true},
	} {
		if CoversTree(c.dir, c.globs) != c.ok {
			t.Errorf("CoversTree(%q, %q) != %v", c.dir, c.globs, c.ok)
		}
	}
}

func TestCoversEverythingIsExact(t *testing.T) {
	if !CoversEverything([]string{"/**"}) || !CoversEverything([]string{"x", "**"}) {
		t.Fatal("** and /** cover everything")
	}
	if CoversEverything([]string{"/**/*"}) || CoversEverything([]string{"/home/**"}) {
		t.Fatal("a pattern glob is not everything")
	}
}
