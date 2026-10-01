package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The recorder runs arecord and returns the whole press on the first
// read. A fake arecord stands in for the real one: no microphone is used.
func TestRecorderReturnsTheWholePressFromAFakeArecord(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'abcd'\ntrap 'exit 0' INT\nwhile :; do /bin/sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(dir, "arecord"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	s, err := openRecorder()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the key is held for a moment
	b, more, err := s.Read(1600)
	if err != nil || more || string(b) != "abcd" {
		t.Fatalf("read %q more=%v err=%v", b, more, err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRecorderRefusesWithNoArecord(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := openRecorder(); err == nil {
		t.Fatal("want a refusal with no arecord on PATH")
	}
}

func TestArmMinutesFollowsTheOracleRules(t *testing.T) {
	for spec, want := range map[string]float64{"30m": 30, "2h": 120, " 90 ": 90, "1_0M": 10} {
		got, msg := armMinutes(spec)
		if msg != "" || got != want {
			t.Errorf("%q: got %v %q, want %v", spec, got, msg, want)
		}
	}
	for _, spec := range []string{"", "abc", "0", "-5m", "inf", "nan", "10081"} {
		if _, msg := armMinutes(spec); msg == "" {
			t.Errorf("%q: want a refusal", spec)
		}
	}
}
