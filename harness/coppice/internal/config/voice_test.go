package config

import (
	"os"
	"strings"
	"testing"
)

func TestVoiceIsOnWhenTheFileSaysNothing(t *testing.T) {
	if !(Config{}).VoiceOn() {
		t.Fatal("a file with no [voice] table must start voice")
	}
	off := false
	if (Config{Voice: Voice{Enabled: &off}}).VoiceOn() {
		t.Fatal("[voice] enabled = false must turn voice off")
	}
}

func TestVoiceTableRoundTripsAndStaysOutWhenEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "claude"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path())
	if strings.Contains(string(b), "voice") {
		t.Fatalf("an empty voice table was written: %s", b)
	}
	off := false
	in := Config{Voice: Voice{Enabled: &off, URL: "http://127.0.0.1:7477", TokenFile: "/x/token",
		Args: []string{"--data-dir", "/x"}}}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Voice.Enabled == nil || *got.Voice.Enabled || got.Voice.URL != in.Voice.URL ||
		got.Voice.TokenFile != in.Voice.TokenFile || strings.Join(got.Voice.Args, " ") != "--data-dir /x" {
		t.Fatalf("voice = %+v", got.Voice)
	}
}

func TestTheDefaultTalkKeyIsCtrlBackslash(t *testing.T) {
	b, err := (Config{}).TalkKey(DefaultLeave)
	if err != nil || b != 0x1c {
		t.Fatalf("TalkKey() = %x %v, want ctrl-\\", b, err)
	}
	b, err = (Config{Keys: Keys{Talk: "ctrl-g"}}).TalkKey(DefaultLeave)
	if err != nil || b != 0x07 {
		t.Fatalf("TalkKey(ctrl-g) = %x %v", b, err)
	}
}

func TestTheTalkKeyCannotBeAKeyTheFloorAlreadyUses(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"ctrl-space", "ctrl-t", "ctrl-w", "ctrl-c", "ctrl-[", "q"} {
		_, err := (Config{Keys: Keys{Talk: name}}).TalkKey(DefaultLeave)
		if err == nil {
			t.Fatalf("talk = %q was accepted", name)
		}
		if !strings.Contains(err.Error(), "[keys] talk") || !strings.Contains(err.Error(), Path()) {
			t.Fatalf("talk = %q error %q does not name the key and the file", name, err)
		}
	}
	if _, err := (Config{Keys: Keys{Talk: "ctrl-]"}}).TalkKey(0x1d); err == nil {
		t.Fatal("a talk key equal to the leave key was accepted")
	}
}
