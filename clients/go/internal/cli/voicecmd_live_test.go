package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The recorder gathers arecord's audio as it comes, so push to talk can
// ask for the text so far before the owner stops.
func TestRecorderGivesTheAudioSoFarBeforeItStops(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
trap 'printf "BBBB"; exit 0' INT
printf "AAAA"
while :; do sleep 0.05; done
`
	if err := os.WriteFile(filepath.Join(dir, "arecord"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	s, err := openRecorder()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	r := s.(*recorder)
	var snap []byte
	for i := 0; i < 100 && len(snap) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		snap = r.Snapshot()
	}
	if string(snap) != "AAAA" {
		t.Fatalf("snapshot %q", snap)
	}
	all, _, err := s.Read(1600)
	if err != nil || string(all) != "AAAABBBB" {
		t.Fatalf("read %q %v", all, err)
	}
	s.Stop()
}

// The live line redraws one row in place: it clears the row, draws the
// end of the text dim, and never ends the line.
func TestPTTLiveRedrawsOneRowInPlace(t *testing.T) {
	var out bytes.Buffer
	e := &Env{Stdout: &out, env: map[string]string{}}
	if e.pttLive(false).Show != nil {
		t.Fatal("off a terminal the live line must show nothing")
	}
	live := e.pttLive(true)
	live.Show("run the tests")
	if got := out.String(); got != "\r\x1b[K\x1b[2mrun the tests\x1b[0m" {
		t.Fatalf("%q", got)
	}
	out.Reset()
	live.Show("")
	if out.String() != "\r\x1b[K" {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	live.Show(strings.Repeat("word ", 40))
	if strings.Contains(out.String(), "\n") || len([]rune(out.String())) > 80+12 {
		t.Fatalf("the line must fit one row: %q", out.String())
	}
	e = &Env{Stdout: &out, env: map[string]string{"NO_COLOR": "1"}}
	out.Reset()
	e.pttLive(true).Show("plain")
	if out.String() != "\r\x1b[Kplain" {
		t.Fatalf("NO_COLOR: %q", out.String())
	}
}

// --no-color is the same as NO_COLOR=1, and NO_COLOR set but empty is
// unset, as no-color.org says.
func TestPTTLiveHonoursNoColorFlagAndEmptyNoColor(t *testing.T) {
	var out bytes.Buffer
	e := &Env{Stdout: &out, env: map[string]string{}}
	e.run([]string{"--no-color"})
	out.Reset()
	e.pttLive(true).Show("plain")
	if out.String() != "\r\x1b[Kplain" {
		t.Fatalf("--no-color: %q", out.String())
	}
	e = &Env{Stdout: &out, env: map[string]string{"NO_COLOR": ""}}
	out.Reset()
	e.pttLive(true).Show("dim")
	if out.String() != "\r\x1b[K\x1b[2mdim\x1b[0m" {
		t.Fatalf("empty NO_COLOR: %q", out.String())
	}
}
