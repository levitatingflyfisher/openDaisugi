package config

import (
	"os"
	"strings"
	"testing"
)

func TestParseKeyNamesCtrlAndBrackets(t *testing.T) {
	b, err := ParseKey("ctrl-]")
	if err != nil || b != 0x1d {
		t.Fatalf("%x %v", b, err)
	}
	if _, err := ParseKey("shift-tab"); err == nil {
		t.Fatal("a multi-byte key cannot be the leave key")
	}
}

func TestParseKeyAcceptsEveryCtrlSpelling(t *testing.T) {
	cases := map[string]byte{
		"ctrl-a": 0x01, "ctrl-z": 0x1a, "ctrl+t": 0x14, "CTRL-W": 0x17, " ctrl-] ": 0x1d,
		"ctrl-c": 0x03, `ctrl-\`: 0x1c, "ctrl-^": 0x1e, "ctrl-_": 0x1f,
		"ctrl-space": 0x00, "CTRL+SPACE": 0x00, "ctrl-@": 0x00, " ctrl+@ ": 0x00,
	}
	for name, want := range cases {
		got, err := ParseKey(name)
		if err != nil || got != want {
			t.Fatalf("ParseKey(%q) = %x %v, want %x", name, got, err, want)
		}
	}
	for _, name := range []string{"", "a", "ctrl", "ctrl-", "ctrl-ab", "ctrl-1", "alt-x", "esc", "space", "ctrl-spac", "ctrl- "} {
		if _, err := ParseKey(name); err == nil {
			t.Fatalf("ParseKey(%q) accepted", name)
		}
	}
	// ESC, Tab, Enter as newline, and Enter as return are keys every
	// harness reads, so none of them can be the leave key.
	for _, name := range []string{"ctrl-[", "ctrl-i", "ctrl-j", "ctrl-m", "ctrl+[", "CTRL-M"} {
		_, err := ParseKey(name)
		if err == nil {
			t.Fatalf("ParseKey(%q) accepted", name)
		}
		if !strings.Contains(err.Error(), "the leave key is one ctrl key, like ctrl-]") {
			t.Fatalf("ParseKey(%q) error = %q, want the teaching line", name, err)
		}
	}
}

func TestKeyNameIsTheInverseOfParseKey(t *testing.T) {
	if got := KeyName(0x1d); got != "ctrl-]" {
		t.Fatalf("KeyName(0x1d) = %q", got)
	}
	if got := KeyName(0x00); got != "ctrl-space" {
		t.Fatalf("KeyName(0x00) = %q, want ctrl-space", got)
	}
	for b := byte(0x00); b <= 0x1f; b++ {
		name := KeyName(b)
		back, err := ParseKey(name)
		if b == 0x09 || b == 0x0a || b == 0x0d || b == 0x1b {
			if err == nil {
				t.Fatalf("KeyName(%x) = %q, ParseKey accepted a refused key", b, name)
			}
			continue
		}
		if err != nil || back != b {
			t.Fatalf("KeyName(%x) = %q, ParseKey gives %x %v", b, name, back, err)
		}
	}
	for _, b := range []byte{0x20, 'a', 0x7f} {
		if got := KeyName(b); got != "?" {
			t.Fatalf("KeyName(%x) = %q, want ?", b, got)
		}
	}
}

func TestTheDefaultLeaveKeyIsCtrlSpace(t *testing.T) {
	if DefaultLeave != 0x00 {
		t.Fatalf("DefaultLeave = %x, want 0x00, ctrl-space", DefaultLeave)
	}
	if b, err := ParseKey(KeyName(DefaultLeave)); err != nil || b != DefaultLeave {
		t.Fatalf("the default does not round trip: %x %v", b, err)
	}
}

func TestLeaveKeyDefaultsAndRefusesABadValue(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if b, err := (Config{}).LeaveKey(); err != nil || b != DefaultLeave {
		t.Fatalf("empty = %x %v, want the default", b, err)
	}
	if b, err := (Config{Keys: Keys{Leave: "ctrl-a"}}).LeaveKey(); err != nil || b != 0x01 {
		t.Fatalf("ctrl-a = %x %v", b, err)
	}
	_, err := (Config{Keys: Keys{Leave: "shift-tab"}}).LeaveKey()
	if err == nil {
		t.Fatal("a bad leave key was accepted")
	}
	for _, want := range []string{`bad [keys] leave "shift-tab" in `, Path(), "the leave key is one ctrl key, like ctrl-]"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestKeysRoundTripThroughTheFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Default: "pi", Keys: Keys{Leave: "ctrl-a"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "[keys]") || !strings.Contains(string(b), `leave = "ctrl-a"`) {
		t.Fatalf("file lacks the keys table: %q", b)
	}
	got, ok, err := Load()
	if err != nil || !ok || got.Keys.Leave != "ctrl-a" {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
}
