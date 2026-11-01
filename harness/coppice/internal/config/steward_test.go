package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTheStewardTableDefaultsToPaneMode(t *testing.T) {
	s, err := (Config{}).StewardSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := StewardSettings{Mode: "pane", CompactorProfile: "default", TurnProfiles: []string{""}, Store: "work"}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("settings = %+v, want %+v", s, want)
	}
	if got := s.CompactorModelFor("llama"); got != "haiku" {
		t.Fatalf("model on the default profile = %q", got)
	}
}

func TestTheStewardTableReadsFromTheFileAndStaysOutWhenEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "claude"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path())
	if strings.Contains(string(b), "steward") {
		t.Fatalf("an empty steward table was written: %s", b)
	}
	in := Config{Steward: Steward{Mode: "chat", CompactorProfile: "desk", CompactorModel: "m1", TurnProfile: "desk", TurnModel: "sonnet"}}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := got.StewardSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := StewardSettings{Mode: "chat", CompactorProfile: "desk", CompactorModel: "m1", TurnProfiles: []string{"desk"}, TurnModel: "sonnet", Store: "work"}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("settings = %+v, want %+v", s, want)
	}
}

func TestACompactorModelOnANonDefaultProfileFollowsTheProfile(t *testing.T) {
	s := StewardSettings{CompactorProfile: "desk"}
	if got := s.CompactorModelFor("llama3.3"); got != "llama3.3" {
		t.Fatalf("model = %q", got)
	}
	s.CompactorModel = "pinned"
	if got := s.CompactorModelFor("llama3.3"); got != "pinned" {
		t.Fatalf("model = %q", got)
	}
}

func TestStewardValuesThatAreNotAllowedNameTheKey(t *testing.T) {
	for _, tc := range []struct {
		f    Steward
		want string
	}{
		{Steward{Mode: "wide"}, "mode"},
	} {
		_, err := (Config{Steward: tc.f}).StewardSettings()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: err = %v", tc.f, err)
		}
	}
}

func TestAnOldStewardTableWithCutKeysStillLoads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	old := "[steward]\nmode = \"chat\"\ncalls = 3\nturn_timeout_s = 60\n"
	if err := os.WriteFile(Path(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := got.StewardSettings()
	if err != nil || s.Mode != "chat" {
		t.Fatalf("settings = %+v, err = %v", s, err)
	}
}

func TestTurnProfilesIsAnOrderedChainAndTurnProfileIsAOneEntryChain(t *testing.T) {
	s, err := (Config{Steward: Steward{TurnProfiles: []string{"", "desk", "grok"}, TurnModel: "opus"}}).StewardSettings()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"", "desk", "grok"}; !reflect.DeepEqual(s.TurnProfiles, want) {
		t.Fatalf("chain = %q, want %q", s.TurnProfiles, want)
	}
	s, err = (Config{Steward: Steward{TurnProfile: "desk"}}).StewardSettings()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"desk"}; !reflect.DeepEqual(s.TurnProfiles, want) {
		t.Fatalf("chain = %q, want %q", s.TurnProfiles, want)
	}
}

func TestSettingBothTurnKeysNamesBoth(t *testing.T) {
	_, err := (Config{Steward: Steward{TurnProfile: "a", TurnProfiles: []string{"b"}}}).StewardSettings()
	if err == nil || !strings.Contains(err.Error(), "turn_profile") || !strings.Contains(err.Error(), "turn_profiles") {
		t.Fatalf("err = %v", err)
	}
}

func TestTurnProfilesReadFromTheFileAndWriteBack(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte("[steward]\nmode = \"chat\"\nturn_profiles = [\"\", \"desk\"]\nturn_model = \"opus\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s, err := got.StewardSettings(); err != nil || !reflect.DeepEqual(s.TurnProfiles, []string{"", "desk"}) {
		t.Fatalf("settings = %+v, err = %v", s, err)
	}
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path())
	if !strings.Contains(string(b), "turn_profiles") {
		t.Fatalf("the chain was not written back: %s", b)
	}
}

func TestSeveralAccountsNeedTheOneModelNamed(t *testing.T) {
	_, err := (Config{Steward: Steward{TurnProfiles: []string{"", "desk"}}}).StewardSettings()
	if err == nil || !strings.Contains(err.Error(), "turn_model") || !strings.Contains(err.Error(), "turn_profiles") {
		t.Fatalf("err = %v", err)
	}
	if _, err := (Config{Steward: Steward{TurnProfiles: []string{"desk"}}}).StewardSettings(); err != nil {
		t.Fatalf("one account needs no model: %v", err)
	}
}

func TestTurnProfilesRefuseTheSameAccountTwice(t *testing.T) {
	for _, list := range [][]string{{"", "default"}, {"", ""}, {"a", "b", "a"}} {
		c := Config{Steward: Steward{TurnProfiles: list, TurnModel: "m"}}
		if _, err := c.StewardSettings(); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("%q: err = %v", list, err)
		}
	}
	c := Config{Steward: Steward{TurnProfiles: []string{"", "a"}, TurnModel: "m"}}
	if _, err := c.StewardSettings(); err != nil {
		t.Fatal(err)
	}
}

func TestOldForemanTableRefused(t *testing.T) { // names-ok: the old table
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "coppice"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		if err := os.WriteFile(filepath.Join(dir, "coppice", "coppice.toml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[foreman]\nmode = \"chat\"\n")                                                             // names-ok: the old table
	if _, _, err := Load(); err == nil || err.Error() != "coppice.toml: [foreman] is now [steward]" { // names-ok: the old table
		t.Fatalf("got %v", err)
	}
	write("gateway = \"http://127.0.0.1:8787\"\n")                                               // names-ok: the old key
	if _, _, err := Load(); err == nil || err.Error() != "coppice.toml: gateway is now router" { // names-ok: the old key
		t.Fatalf("got %v", err)
	}
	// A key of that name inside a table is that table's own, not the old top-level key.
	write("[plugin.x]\ngateway = \"y\"\n") // names-ok: the old key
	if _, _, err := Load(); err != nil {
		t.Fatalf("a key in a table is not the old key: %v", err)
	}
}

func TestTheStewardStoreNamesAPandoStoreAndRefusesAll(t *testing.T) {
	s, err := (Config{Steward: Steward{Store: "home"}}).StewardSettings()
	if err != nil || s.Store != "home" {
		t.Fatalf("store = %q, %v", s.Store, err)
	}
	for _, bad := range []string{"all", "Work", "a/b"} {
		if _, err := (Config{Steward: Steward{Store: bad}}).StewardSettings(); err == nil {
			t.Fatalf("store %q must be refused", bad)
		}
	}
	_, err = (Config{Steward: Steward{Store: "all"}}).StewardSettings()
	if err == nil || !strings.Contains(err.Error(), "one store at a time") {
		t.Fatalf("all: %v", err)
	}
}

func TestTheStewardDirIsAbsoluteOrHomeRelative(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{"/srv/st/": "/srv/st", "~/work/st": filepath.Join(home, "work/st")} {
		s, err := (Config{Steward: Steward{Dir: in}}).StewardSettings()
		if err != nil || s.Dir != want {
			t.Errorf("dir %q: %q, %v (want %q)", in, s.Dir, err, want)
		}
	}
	if _, err := (Config{Steward: Steward{Dir: "rel/dir"}}).StewardSettings(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("a relative dir was not refused: %v", err)
	}
}

func TestTrustedRootsDefaultToCodeAndThePinnedProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := Config{Projects: []string{"/srv/p"}}.TrustedRoots()
	if err != nil || strings.Join(got, ",") != filepath.Join(home, "code")+",/srv/p" {
		t.Fatalf("roots = %v, %v", got, err)
	}
	got, err = Config{Steward: Steward{TrustedRoots: []string{"~/work", "/data/src/"}}}.TrustedRoots()
	if err != nil || strings.Join(got, ",") != filepath.Join(home, "work")+",/data/src" {
		t.Fatalf("roots = %v, %v", got, err)
	}
	for _, bad := range []string{"/", "~", "~/", "rel/dir"} {
		if _, err := (Config{Steward: Steward{TrustedRoots: []string{bad}}}).TrustedRoots(); err == nil || !strings.Contains(err.Error(), "trusted_roots") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}
