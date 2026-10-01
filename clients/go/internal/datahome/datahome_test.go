package datahome

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDirFollowsTheRule(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name string
		env  map[string]string
		dot  bool
		want string
	}{
		{"neither set", nil, false, home + "/.opendaisugi"},
		{"OPENDAISUGI_HOME wins", map[string]string{"OPENDAISUGI_HOME": home + "/od", "XDG_DATA_HOME": home + "/x"}, true, home + "/od"},
		{"xdg when the dot dir is absent", map[string]string{"XDG_DATA_HOME": home + "/x"}, false, home + "/x/opendaisugi"},
		{"an existing dot dir keeps working", map[string]string{"XDG_DATA_HOME": home + "/x"}, true, home + "/.opendaisugi"},
		{"empty values count as unset", map[string]string{"OPENDAISUGI_HOME": "", "XDG_DATA_HOME": ""}, false, home + "/.opendaisugi"},
		{"paths print as pathlib prints them", map[string]string{"XDG_DATA_HOME": home + "//x/"}, false, home + "/x/opendaisugi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dot := filepath.Join(home, ".opendaisugi")
			_ = os.RemoveAll(dot)
			if c.dot {
				if err := os.Mkdir(dot, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got := Dir(envOf(c.env), home, Exists); got != c.want {
				t.Fatalf("Dir = %q, want %q", got, c.want)
			}
		})
	}
}

func TestGuardedHoldsEveryPlaceTheDataCanBe(t *testing.T) {
	env := map[string]string{"OPENDAISUGI_HOME": "/h/od", "XDG_DATA_HOME": "/h/x"}
	want := []string{"/h/.opendaisugi", "/h/od", "/h/x/opendaisugi"}
	if got := Guarded(envOf(env), "/h"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Guarded = %q, want %q", got, want)
	}
	if got := Guarded(envOf(nil), "/h"); !reflect.DeepEqual(got, []string{"/h/.opendaisugi"}) {
		t.Fatalf("Guarded with nothing set = %q", got)
	}
	same := map[string]string{"OPENDAISUGI_HOME": "/h/.opendaisugi"}
	if got := Guarded(envOf(same), "/h"); !reflect.DeepEqual(got, []string{"/h/.opendaisugi"}) {
		t.Fatalf("Guarded with a duplicate = %q", got)
	}
}

func TestATildeIsExpandedAndARelativeValueIgnored(t *testing.T) {
	home := t.TempDir()
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OPENDAISUGI_HOME": "~/od"}, home + "/od"},
		{map[string]string{"OPENDAISUGI_HOME": "~"}, home},
		{map[string]string{"XDG_DATA_HOME": "~/x"}, home + "/x/opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "od"}, home + "/.opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "./od"}, home + "/.opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "~user/od"}, home + "/.opendaisugi"},
		{map[string]string{"XDG_DATA_HOME": "rel"}, home + "/.opendaisugi"},
		{map[string]string{"OPENDAISUGI_HOME": "od", "XDG_DATA_HOME": home + "/x"}, home + "/x/opendaisugi"},
	} {
		if got := Dir(envOf(c.env), home, Exists); got != c.want {
			t.Errorf("%v: Dir = %q, want %q", c.env, got, c.want)
		}
	}
	env := map[string]string{"OPENDAISUGI_HOME": "~/od", "XDG_DATA_HOME": "rel"}
	if got := Guarded(envOf(env), "/h"); !reflect.DeepEqual(got, []string{"/h/.opendaisugi", "/h/od"}) {
		t.Errorf("Guarded = %q", got)
	}
}
