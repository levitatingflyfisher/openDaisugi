package install

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var moveNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func moveRun(t *testing.T, dir, home string, dry bool) ([]string, error) {
	t.Helper()
	var out bytes.Buffer
	lines, err := MoveOldState(dir, home, map[string]string{}, moveNow, &out, dry)
	if got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"); out.Len() > 0 && strings.Join(got, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("out %q vs lines %q", got, lines)
	}
	return lines, err
}

func mk(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, text string) {
	t.Helper()
	mk(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMoveOldStateMovesDirs(t *testing.T) {
	d := t.TempDir()
	mk(t, filepath.Join(d, "gate", "envelopes")) // names-ok
	mk(t, filepath.Join(d, "gateway"))           // names-ok
	lines, err := moveRun(t, d, filepath.Join(d, "h"), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"moved " + d + "/gate to " + d + "/warden", "moved " + d + "/gateway to " + d + "/router"} // names-ok
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", lines, want)
	}
	if _, err := os.Stat(filepath.Join(d, "warden", "envelopes")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(d, "gate")); err == nil { // names-ok
		t.Fatal("the old directory is still there")
	}
}

func TestMoveOldStateGatewayFilesJoinAnExistingRouterDir(t *testing.T) { // names-ok
	d := t.TempDir()
	write(t, filepath.Join(d, "gateway", "turns.jsonl"), "{}\n") // names-ok
	write(t, filepath.Join(d, "router", "delegations.jsonl"), "{}\n")
	lines, err := moveRun(t, d, d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "moved "+d+"/gateway into "+d+"/router" { // names-ok
		t.Fatalf("got %q", lines)
	}
	for _, f := range []string{"turns.jsonl", "delegations.jsonl"} {
		if _, err := os.Stat(filepath.Join(d, "router", f)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMoveOldStateRefusesBoth(t *testing.T) {
	d := t.TempDir()
	mk(t, filepath.Join(d, "gate")) // names-ok
	mk(t, filepath.Join(d, "warden"))
	_, err := moveRun(t, d, d, false)
	var r *MoveRefused
	if !errors.As(err, &r) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(d, "gate")); err != nil { // names-ok
		t.Fatal("the old directory moved")
	}
}

func TestMoveOldStateRefusesLiveSocket(t *testing.T) {
	d, err := os.MkdirTemp("", "mv")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	mk(t, filepath.Join(d, "gate"))                                     // names-ok
	l, err := net.Listen("unix", filepath.Join(d, "gate", "gate.sock")) // names-ok
	if err != nil {
		t.Skip("socket path too long here: " + err.Error())
	}
	defer l.Close()
	lines, err := moveRun(t, d, d, false)
	var r *MoveRefused
	if !errors.As(err, &r) || !strings.Contains(lines[len(lines)-1], "still listens") {
		t.Fatalf("got %v %q", err, lines)
	}
	if _, err := os.Stat(filepath.Join(d, "warden")); err == nil {
		t.Fatal("the directory moved")
	}
}

func TestMoveOldStateRewritesConfigKeys(t *testing.T) {
	d := t.TempDir()
	cfg := filepath.Join(d, "config.yaml")
	write(t, cfg, "gate_mode: enforce\ngateway_router: knn\ngateway_router_model: m\n") // names-ok
	lines, err := moveRun(t, d, d, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	if string(b) != "warden_mode: enforce\nrouting_mode: knn\nrouting_model: m\n" {
		t.Fatalf("got %q", b)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "config: gate_mode is now warden_mode") { // names-ok
		t.Fatalf("got %q", lines)
	}
}

func TestInstallReplacesOldMarkerHook(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, ".claude", "settings.json")
	old := "DAISUGI_GATE_HOOK=opendaisugi.gate python -I -m opendaisugi.gate_client check --mode enforce --root /x/gate" // names-ok
	write(t, f, `{"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "`+old+`"}]}]}}`)
	lines, err := moveRun(t, filepath.Join(d, "data"), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "replaced the old warden hook in "+f {
		t.Fatalf("got %q", lines)
	}
	b, _ := os.ReadFile(f)
	s := string(b)
	if !strings.Contains(s, "DAISUGI_WARDEN_HOOK=opendaisugi.warden") || !strings.Contains(s, "--mode enforce") || strings.Contains(s, "DAISUGI_GATE") { // names-ok
		t.Fatalf("got %s", s)
	}
}

func TestInstallMovesOldExtensionsAside(t *testing.T) {
	d := t.TempDir()
	ext := filepath.Join(d, ".pi", "agent", "extensions", "daisugi-gate") // names-ok
	write(t, filepath.Join(ext, "index.ts"), "// old")
	lines, err := moveRun(t, filepath.Join(d, "data"), d, false)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(d, "data", "moved-aside", "20261030T120000Z", "pi-daisugi-gate") // names-ok
	if len(lines) != 1 || lines[0] != "moved aside "+ext+" to "+moved {
		t.Fatalf("got %q", lines)
	}
	if b, _ := os.ReadFile(filepath.Join(moved, "index.ts")); string(b) != "// old" {
		t.Fatalf("got %q", b)
	}
	if _, err := os.Lstat(ext); err == nil {
		t.Fatal("the extension is still in place")
	}
}

func TestMoveOldStateNamesAStaleDelegateAllow(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "warden", "envelopes", "e1.json"), `{"permissions": {"mcp_allowlist": ["opendaisugi/delegate"]}}`) // names-ok
	lines, err := moveRun(t, d, d, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "envelope e1 allows mcp opendaisugi/delegate; add opendaisugi/spawn (the tool was renamed)" // names-ok
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("got %q", lines)
	}
}

func TestMoveOldStateDryRunChangesNothing(t *testing.T) {
	d := t.TempDir()
	mk(t, filepath.Join(d, "gate")) // names-ok
	lines, err := moveRun(t, d, d, true)
	if err != nil || len(lines) != 1 || lines[0] != "would move "+d+"/gate to "+d+"/warden" { // names-ok
		t.Fatalf("got %v %q", err, lines)
	}
	if _, err := os.Stat(filepath.Join(d, "gate")); err != nil { // names-ok
		t.Fatal("moved on a dry run")
	}
}
