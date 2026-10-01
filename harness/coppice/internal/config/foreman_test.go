package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheForemanTableDefaultsToPaneMode(t *testing.T) {
	s, err := (Config{}).ForemanSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := ForemanSettings{Mode: "pane", CompactorProfile: "default"}
	if s != want {
		t.Fatalf("settings = %+v, want %+v", s, want)
	}
	if got := s.CompactorModelFor("llama"); got != "haiku" {
		t.Fatalf("model on the default profile = %q", got)
	}
}

func TestTheForemanTableReadsFromTheFileAndStaysOutWhenEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "claude"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path())
	if strings.Contains(string(b), "foreman") {
		t.Fatalf("an empty foreman table was written: %s", b)
	}
	in := Config{Foreman: Foreman{Mode: "chat", CompactorProfile: "desk", CompactorModel: "m1", TurnProfile: "desk", TurnModel: "sonnet"}}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := got.ForemanSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := ForemanSettings{Mode: "chat", CompactorProfile: "desk", CompactorModel: "m1", TurnProfile: "desk", TurnModel: "sonnet"}
	if s != want {
		t.Fatalf("settings = %+v, want %+v", s, want)
	}
}

func TestACompactorModelOnANonDefaultProfileFollowsTheProfile(t *testing.T) {
	s := ForemanSettings{CompactorProfile: "desk"}
	if got := s.CompactorModelFor("llama3.3"); got != "llama3.3" {
		t.Fatalf("model = %q", got)
	}
	s.CompactorModel = "pinned"
	if got := s.CompactorModelFor("llama3.3"); got != "pinned" {
		t.Fatalf("model = %q", got)
	}
}

func TestForemanValuesThatAreNotAllowedNameTheKey(t *testing.T) {
	for _, tc := range []struct {
		f    Foreman
		want string
	}{
		{Foreman{Mode: "wide"}, "mode"},
	} {
		_, err := (Config{Foreman: tc.f}).ForemanSettings()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: err = %v", tc.f, err)
		}
	}
}

func TestAnOldForemanTableWithCutKeysStillLoads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	old := "[foreman]\nmode = \"chat\"\ncalls = 3\nturn_timeout_s = 60\n"
	if err := os.WriteFile(Path(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := got.ForemanSettings()
	if err != nil || s.Mode != "chat" {
		t.Fatalf("settings = %+v, err = %v", s, err)
	}
}
