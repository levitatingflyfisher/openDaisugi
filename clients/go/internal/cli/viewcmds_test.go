package cli

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestCloseMatchesAsDifflib(t *testing.T) {
	// Answers of difflib.get_close_matches, and click's sorted join.
	opts := []string{"--help", "--data-dir", "--once", "--json", "--interval", "--tui", "--serve", "--host", "--port"}
	for word, want := range map[string]string{
		"--bogus": "--host",
		"--jsn":   "--json",
		"--port":  "--host,--port",
		"--zzzz":  "",
		"--sever": "--serve",
	} {
		got := closeMatches(word, opts)
		sort.Strings(got)
		if strings.Join(got, ",") != want {
			t.Errorf("%s: %v, want %q", word, got, want)
		}
	}
	if r := seqRatio([]rune("abcd"), []rune("bcde")); r != 0.75 {
		t.Errorf("ratio %v", r)
	}
}

func TestURLNetlocAsURLParse(t *testing.T) {
	for in, want := range map[string]string{
		"http://gpu-box:11434/v1":           "gpu-box:11434",
		"gpu-box:11434":                     "",
		"gpu-box":                           "",
		"https://u:p@h.example:8080/v1?q=1": "u:p@h.example:8080",
		"//host/path":                       "host",
		"http://h?x#y":                      "h",
	} {
		if got := urlNetloc(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestPythonNumberFormats(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{groupedFixed(1234.5, 2), "1,234.50"},
		{groupedFixed(-1234567.125, 2), "-1,234,567.12"},
		{groupedFixed(0.006, 2), "0.01"},
		{groupedFixed(math.NaN(), 2), "nan"},
		{groupedFixed(math.Inf(-1), 2), "-inf"},
		{percent0(0.7299), "73%"},
		{percent0(math.NaN()), "nan%"},
		{pyReprFloat(1e17), "1e+17"},
		{pyReprFloat(math.Inf(1)), "inf"},
		{pyReprFloat(0.1), "0.1"},
	} {
		if c.got != c.want {
			t.Errorf("%q, want %q", c.got, c.want)
		}
	}
}

// The frame lights the stage the pulse names, as run_live does on a
// terminal.
func TestDashboardPulseLightsOneStage(t *testing.T) {
	stages := []wStage{{key: "a", title: "one", role: "r"}, {key: "b", title: "two", role: "r"}}
	frame := renderDashboard("/d", stages, nil, 3, boxGlyphs)
	if strings.Count(frame, "▼ ●") != 1 || !strings.Contains(frame, "▼ ●\n┌─ two") {
		t.Fatalf("%s", frame)
	}
	if plain := renderDashboard("/d", stages, nil, -1, asciiGlyphs); strings.Contains(plain, "v *") {
		t.Fatalf("%s", plain)
	}
}

// openPty opens a pseudo-terminal pair: the side a program writes to and
// the side a test reads.
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		m.Close()
		t.Skipf("no pty: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		m.Close()
		t.Skipf("no pty: %v", e)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("no pty: %v", err)
	}
	return m, s
}

// On a terminal the live view draws box glyphs, clears the screen before
// each frame, lights one stage per frame and ends on Ctrl-C with a
// newline and exit 0.
func TestLiveViewOnATerminal(t *testing.T) {
	master, slave := openPty(t)
	defer master.Close()
	defer slave.Close()
	home := t.TempDir()
	var errb bytes.Buffer
	e := &Env{
		Args:    []string{"dashboard", "--interval", "0.05"},
		Stdin:   strings.NewReader(""),
		Stdout:  slave,
		Stderr:  &errb,
		Environ: []string{"HOME=" + home, "PATH=/nonexistent"},
	}
	got := make(chan string, 1)
	go func() {
		var all []byte
		buf := make([]byte, 65536)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			n, err := master.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil || bytes.Count(all, []byte("\x1b[H\x1b[2J")) >= 3 {
				break
			}
		}
		// The loop is past its first frame, so Ctrl-C reaches its handler.
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		got <- string(all)
	}()
	code := Main(e)
	out := <-got
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out, "┌─ harness (agent host) ") || !strings.Contains(out, "▼ ●") {
		t.Fatalf("no box frame: %q", out[:min(len(out), 400)])
	}
	frames := strings.Split(out, "\x1b[H\x1b[2J")
	if len(frames) < 3 || strings.Index(frames[1], "▼ ●") == strings.Index(frames[2], "▼ ●") {
		t.Fatalf("the pulse did not move")
	}
}
