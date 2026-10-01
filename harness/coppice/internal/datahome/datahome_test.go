package datahome

import (
	"path/filepath"
	"testing"
)

// The rule of opendaisugi.datahome, as clients/go/internal/datahome holds it.
func TestDirFollowsTheRule(t *testing.T) {
	home := "/h"
	cases := []struct {
		name      string
		env       map[string]string
		dotExists bool
		want      string
	}{
		{"opendaisugi home wins", map[string]string{"OPENDAISUGI_HOME": "/o", "XDG_DATA_HOME": "/x"}, true, "/o"},
		{"xdg when no dot dir", map[string]string{"XDG_DATA_HOME": "/x"}, false, "/x/opendaisugi"},
		{"dot dir beats xdg", map[string]string{"XDG_DATA_HOME": "/x"}, true, "/h/.opendaisugi"},
		{"dot dir by default", map[string]string{}, false, "/h/.opendaisugi"},
		{"empty values count as unset", map[string]string{"OPENDAISUGI_HOME": "", "XDG_DATA_HOME": ""}, false, "/h/.opendaisugi"},
	}
	for _, c := range cases {
		getenv := func(k string) string { return c.env[k] }
		exists := func(p string) bool {
			if p != filepath.Join(home, ".opendaisugi") {
				t.Fatalf("%s: asked about %q", c.name, p)
			}
			return c.dotExists
		}
		if got := Dir(getenv, home, exists); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDefaultReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("OPENDAISUGI_HOME", "/o")
	if got := Default(); got != "/o" {
		t.Fatalf("Default() = %q, want /o", got)
	}
}

// A leading ~ is expanded; a value still not absolute is ignored.
func TestATildeIsExpandedAndARelativeValueIgnored(t *testing.T) {
	no := func(string) bool { return false }
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OPENDAISUGI_HOME": "~/od"}, "/h/od"},
		{map[string]string{"OPENDAISUGI_HOME": "~"}, "/h"},
		{map[string]string{"XDG_DATA_HOME": "~/x"}, "/h/x/opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "od"}, "/h/.opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "./od"}, "/h/.opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "~user/od"}, "/h/.opendaisugi"},
		{map[string]string{"XDG_DATA_HOME": "rel"}, "/h/.opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "od", "XDG_DATA_HOME": "/x"}, "/x/opendaisugi"},
	} {
		if got := Dir(func(k string) string { return c.env[k] }, "/h", no); got != c.want {
			t.Errorf("%v: Dir = %q, want %q", c.env, got, c.want)
		}
	}
}
