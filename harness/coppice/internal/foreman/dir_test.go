package foreman

import (
	"path/filepath"
	"testing"
)

func TestDefaultDir(t *testing.T) {
	home := "/h"
	cases := []struct {
		name      string
		env       map[string]string
		dotExists bool
		want      string
	}{
		{"opendaisugi home wins", map[string]string{"OPENDAISUGI_HOME": "/o", "XDG_DATA_HOME": "/x"}, false, "/o/foreman"},
		{"xdg when no dot dir", map[string]string{"XDG_DATA_HOME": "/x"}, false, "/x/opendaisugi/foreman"},
		{"dot dir beats xdg", map[string]string{"XDG_DATA_HOME": "/x"}, true, "/h/.opendaisugi/foreman"},
		{"dot dir by default", map[string]string{}, false, "/h/.opendaisugi/foreman"},
	}
	for _, c := range cases {
		getenv := func(k string) string { return c.env[k] }
		exists := func(p string) bool {
			if p != filepath.Join(home, ".opendaisugi") {
				t.Fatalf("%s: asked about %q", c.name, p)
			}
			return c.dotExists
		}
		if got := defaultDir(getenv, home, exists); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
