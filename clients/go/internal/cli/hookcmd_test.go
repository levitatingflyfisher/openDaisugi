package cli

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tend trigger spawns through spawnAutoTend, which no test may let
// start a real daisugi.
func stubSpawn(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := spawnAutoTend
	spawnAutoTend = func(_ map[string]string, dataDir string) { got = append(got, dataDir) }
	t.Cleanup(func() { spawnAutoTend = old })
	return &got
}

func TestHookRecordWritesTheCaptureAndTheContract(t *testing.T) {
	spawned := stubSpawn(t)
	home := t.TempDir()
	code, out, errText := run(t, home, `{"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": "ls"}}`,
		"hook", "record")
	if code != 0 || out != "{\"continue\": true}\n" || errText != "" {
		t.Fatalf("exit %d, %q, %q", code, out, errText)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".opendaisugi/captures/s1.jsonl"))
	if err != nil || !strings.Contains(string(raw), `"command": "ls"`) {
		t.Fatalf("capture: %q, %v", raw, err)
	}
	if len(*spawned) != 0 {
		t.Fatalf("spawned with no consent: %v", *spawned)
	}
}

func TestHookRecordNeverFailsOnItsInput(t *testing.T) {
	stubSpawn(t)
	for _, in := range []string{"", "{", "[1]", `{"tool_name": 5}`, "\xff\xfe", strings.Repeat("[", 5000)} {
		for _, ev := range []string{"pre_tool_use", "stop", "notification", "subagent_start", "subagent_stop"} {
			code, out, _ := run(t, t.TempDir(), in, "hook", "record", "--event", ev, "--format", "hermes")
			if code != 0 || out != "{}\n" {
				t.Fatalf("%q %s: exit %d, %q", in, ev, code, out)
			}
		}
	}
}

func TestHookRecordTriggersTendOnceWithConsent(t *testing.T) {
	spawned := stubSpawn(t)
	home := t.TempDir()
	dd := filepath.Join(home, ".opendaisugi")
	if err := os.MkdirAll(dd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dd, "config.yaml"), []byte("auto_tend: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := `{"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": "ls"}}`
	run(t, home, in, "hook", "record")
	run(t, home, in, "hook", "record")
	if len(*spawned) != 1 || (*spawned)[0] != dd {
		t.Fatalf("spawned %v", *spawned)
	}
	if _, err := os.Stat(filepath.Join(dd, ".hook-record-tend-trigger")); err != nil {
		t.Fatal(err)
	}
}

func TestHookStopReportsToCoppice(t *testing.T) {
	stubSpawn(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lines := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			lines <- line
			_, _ = c.Write([]byte("{\"ok\": true}\n"))
			c.Close()
		}
	}()
	var out, errb strings.Builder
	code := Main(&Env{
		Args:    []string{"hook", "record", "--event", "stop"},
		Stdin:   strings.NewReader(`{"session_id": "s9"}`),
		Stdout:  &out,
		Stderr:  &errb,
		Environ: []string{"HOME=" + dir, "PATH=/nonexistent", "COPPICE_SOCK=" + sock, "COPPICE_PANE=w1:p2"},
	})
	if code != 0 {
		t.Fatalf("exit %d %q", code, errb.String())
	}
	got := <-lines
	for _, want := range []string{`"cmd": "pane.report_state"`, `"pane": "w1:p2"`, `"state": "idle"`, `"source": "headless"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("report %q lacks %s", got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".opendaisugi/sessions/s9.jsonl")); err != nil {
		t.Fatal(err)
	}
}
