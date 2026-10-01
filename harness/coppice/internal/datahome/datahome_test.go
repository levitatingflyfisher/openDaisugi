package datahome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F11: one rule that never asks whether ~/.opendaisugi exists. Old coppice
// data at ~/.opendaisugi/coppice is kept when XDG_DATA_HOME is set and its
// own coppice directory holds none.
func TestDirFollowsTheRule(t *testing.T) {
	home := "/h"
	cases := []struct {
		name       string
		env        map[string]string
		has        []string // paths that exist and hold data
		want       string
		wantLegacy bool
	}{
		{"opendaisugi home wins", map[string]string{"OPENDAISUGI_HOME": "/o", "XDG_DATA_HOME": "/x"}, []string{"/h/.opendaisugi/coppice"}, "/o", false},
		{"xdg when it is set", map[string]string{"XDG_DATA_HOME": "/x"}, nil, "/x/opendaisugi", false},
		{"the dot dir when xdg is unset", map[string]string{}, nil, "/h/.opendaisugi", false},
		{"a dot dir made by the gate does not move xdg data", map[string]string{"XDG_DATA_HOME": "/x"}, []string{"/h/.opendaisugi/gate"}, "/x/opendaisugi", false},
		{"empty values count as unset", map[string]string{"OPENDAISUGI_HOME": "", "XDG_DATA_HOME": ""}, nil, "/h/.opendaisugi", false},
		{"old coppice data is kept beside xdg", map[string]string{"XDG_DATA_HOME": "/x"}, []string{"/h/.opendaisugi/coppice"}, "/h/.opendaisugi", true},
		{"new data beats old data", map[string]string{"XDG_DATA_HOME": "/x"}, []string{"/h/.opendaisugi/coppice", "/x/opendaisugi/coppice"}, "/x/opendaisugi", false},
		{"with xdg unset the dot dir is plain, not legacy", map[string]string{}, []string{"/h/.opendaisugi/coppice"}, "/h/.opendaisugi", false},
	}
	for _, c := range cases {
		getenv := func(k string) string { return c.env[k] }
		has := func(p string) bool {
			for _, h := range c.has {
				if h == p {
					return true
				}
			}
			return false
		}
		got, legacy := Resolve(getenv, home, has)
		if got != c.want || legacy != c.wantLegacy {
			t.Errorf("%s: got %q legacy=%v, want %q legacy=%v", c.name, got, legacy, c.want, c.wantLegacy)
		}
		if d := Dir(getenv, home, has); d != c.want {
			t.Errorf("%s: Dir = %q", c.name, d)
		}
	}
}

// With no home directory the legacy name stays relative, as before.
func TestNoHomeKeepsTheRelativeLegacyName(t *testing.T) {
	no := func(string) bool { return false }
	if got := Dir(func(string) string { return "" }, "", no); got != ".opendaisugi" {
		t.Fatalf("Dir = %q", got)
	}
}

func TestDefaultReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("OPENDAISUGI_HOME", "/o")
	if got := Default(); got != "/o" {
		t.Fatalf("Default() = %q, want /o", got)
	}
}

// The same box gets the same place before and after ~/.opendaisugi
// appears (`daisugi install --gate` makes it), with no note.
func TestInstallingTheGateDoesNotMoveCoppice(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "share")
	t.Setenv("HOME", home)
	t.Setenv("OPENDAISUGI_HOME", "")
	t.Setenv("XDG_DATA_HOME", xdg)
	want := filepath.Join(xdg, "opendaisugi")
	if got := Default(); got != want {
		t.Fatalf("before: %q, want %q", got, want)
	}
	if err := os.MkdirAll(filepath.Join(home, ".opendaisugi", "gate"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Default(); got != want {
		t.Fatalf("after the gate is installed: %q, want %q", got, want)
	}
	if Legacy() != "" {
		t.Fatalf("a note for no reason: %q", Legacy())
	}
}

func TestAnEmptyOldCoppiceDirIsNotKept(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "share")
	t.Setenv("HOME", home)
	t.Setenv("OPENDAISUGI_HOME", "")
	t.Setenv("XDG_DATA_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(home, ".opendaisugi", "coppice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Default(); got != filepath.Join(xdg, "opendaisugi") {
		t.Fatalf("an empty old dir was kept: %q", got)
	}
}

func TestOldCoppiceDirWithDataIsKeptWithOneLineSayingWhere(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "share")
	t.Setenv("HOME", home)
	t.Setenv("OPENDAISUGI_HOME", "")
	t.Setenv("XDG_DATA_HOME", xdg)
	old := filepath.Join(home, ".opendaisugi", "coppice")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "layout.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Default(); got != filepath.Join(home, ".opendaisugi") {
		t.Fatalf("old data not kept: %q", got)
	}
	note := Legacy()
	if strings.Contains(note, "\n") || !strings.Contains(note, old) ||
		!strings.Contains(note, filepath.Join(xdg, "opendaisugi", "coppice")) {
		t.Fatalf("note: %q", note)
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
