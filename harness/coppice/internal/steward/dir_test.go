package steward

import (
	"testing"
)

func TestDefaultDir(t *testing.T) {
	home := "/h"
	cases := []struct {
		name string
		env  map[string]string
		has  string // a path that exists and holds data
		want string
	}{
		{"opendaisugi home wins", map[string]string{"OPENDAISUGI_HOME": "/o", "XDG_DATA_HOME": "/x"}, "", "/o/" + DirName},
		{"xdg when it is set", map[string]string{"XDG_DATA_HOME": "/x"}, "", "/x/opendaisugi/" + DirName},
		{"a ~/.opendaisugi made by the warden does not move it", map[string]string{"XDG_DATA_HOME": "/x"}, "/h/.opendaisugi/warden", "/x/opendaisugi/" + DirName},
		{"old coppice data is kept", map[string]string{"XDG_DATA_HOME": "/x"}, "/h/.opendaisugi/coppice", "/h/.opendaisugi/" + DirName},
		{"the dot dir when xdg is unset", map[string]string{}, "", "/h/.opendaisugi/" + DirName},
	}
	for _, c := range cases {
		getenv := func(k string) string { return c.env[k] }
		has := func(p string) bool { return p == c.has }
		if got := defaultDir(getenv, home, has); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
