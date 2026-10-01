package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The per-agent mode marker (F11-R-1, F11-R-2, F11-R-6): only the bytes
// "watching", in a regular file named for the calling pane in the calling
// floor's directory, lower an enforce hook. The gate cases pin the same
// rule against Python. Each test here needs the marker to lower a call,
// so it fails when the marker code is taken out.

const (
	sockA = "/run/coppice/floor-a.sock"
	sockB = "/run/coppice/floor-b.sock"
)

func floorDir(t *testing.T, root, sock string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(sock))
	d := filepath.Join(root, "agents", hex.EncodeToString(sum[:])[:16])
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func markerRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gate")
}

func writeMarker(t *testing.T, root, sock, pane, body string) string {
	t.Helper()
	p := filepath.Join(floorDir(t, root, sock), pane+".mode")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func floorRunner(root, sock, pane string) *runner {
	return &runner{env: map[string]string{"COPPICE_PANE": pane, "COPPICE_SOCK": sock}, root: root}
}

func TestAgentModeFloorIsTheSocketsSHA256(t *testing.T) {
	got, ok := agentModeFloor(sockA)
	sum := sha256.Sum256([]byte(sockA))
	if !ok || got != hex.EncodeToString(sum[:])[:16] {
		t.Fatalf("floor %q %v", got, ok)
	}
}

func TestAgentModeLowersOnlyOnWatching(t *testing.T) {
	for body, want := range map[string]bool{
		"watching": true, "watching\n": true,
		"enforcing\n": false, "off\n": false, "WATCHING\n": false, " watching\n": false,
		"watching\n\n": false, "watching\r\n": false, "": false, "\xffwatching": false,
		"watching\n" + string(make([]byte, 60)): false,
	} {
		root := markerRoot(t)
		writeMarker(t, root, sockA, "w1:p1", body)
		if got := floorRunner(root, sockA, "w1:p1").effectiveMode("enforce"); (got == "audit") != want {
			t.Errorf("marker %q: mode %q, want lowered=%v", body, got, want)
		}
	}
}

func TestAgentModeLowersThisPaneOfThisFloorOnly(t *testing.T) {
	root := markerRoot(t)
	writeMarker(t, root, sockB, "w1:p1", "watching\n")
	if got := floorRunner(root, sockA, "w1:p1").effectiveMode("enforce"); got != "enforce" {
		t.Fatalf("another floor's marker lowered this floor: %q", got)
	}
	if got := floorRunner(root, sockB, "w1:p1").effectiveMode("enforce"); got != "audit" {
		t.Fatalf("the floor's own marker did not lower it: %q", got)
	}
	if got := floorRunner(root, sockB, "w1:p2").effectiveMode("enforce"); got != "enforce" {
		t.Fatalf("another pane's marker lowered this pane: %q", got)
	}
	r := &runner{env: map[string]string{"COPPICE_PANE": "w1:p1"}, root: root}
	if got := r.effectiveMode("enforce"); got != "enforce" {
		t.Fatalf("no COPPICE_SOCK lowered it: %q", got)
	}
}

func TestAgentModeNeverRaisesAudit(t *testing.T) {
	root := markerRoot(t)
	writeMarker(t, root, sockA, "w1:p1", "enforcing\n")
	if got := floorRunner(root, sockA, "w1:p1").effectiveMode("audit"); got != "audit" {
		t.Fatalf("mode %q, want audit", got)
	}
}

func TestAgentModeRefusesOddFilesAndIDs(t *testing.T) {
	root := markerRoot(t)
	dir := floorDir(t, root, sockA)
	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, []byte("watching\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "w1:p1.mode")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "w1:p2.mode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "w1:p3.mode"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, root, sockA, "w1:p4", "watching\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "w1:p4.mode"), []byte("watching\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := floorRunner(root, sockA, "w1:p4").effectiveMode("enforce"); got != "audit" {
		t.Fatalf("the good marker did not lower: %q", got)
	}
	for _, pane := range []string{"w1:p1", "w1:p2", "w1:p3", "../w1:p4", "w1:p4/../w1:p4", "w1:p4\n", "", "%3", "w1:p1234567890"} {
		if got := floorRunner(root, sockA, pane).effectiveMode("enforce"); got != "enforce" {
			t.Errorf("pane %q: mode %q, want enforce", pane, got)
		}
	}
	r := &runner{env: map[string]string{"TMUX_PANE": "w1:p4", "HERDR_PANE_ID": "w1:p4", "COPPICE_SOCK": sockA}, root: root}
	if got := r.effectiveMode("enforce"); got != "enforce" {
		t.Errorf("no COPPICE_PANE: mode %q, want enforce", got)
	}
}

// F11-R-7: no symlinked directory is followed, and the marker must be
// this user's, with one link, writable by no one else. Each case first
// shows the same marker lowering the call.
func TestAgentModeRefusesLinksAndLooseOwners(t *testing.T) {
	lowered := func(root string) bool {
		return floorRunner(root, sockA, "w1:p1").effectiveMode("enforce") == "audit"
	}
	for name, spoil := range map[string]func(t *testing.T, root, marker string){
		"agents dir symlink": func(t *testing.T, root, _ string) {
			agents := filepath.Join(root, "agents")
			real := filepath.Join(root, "real-agents")
			if err := os.Rename(agents, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, agents); err != nil {
				t.Fatal(err)
			}
		},
		"floor dir symlink": func(t *testing.T, root, marker string) {
			dir := filepath.Dir(marker)
			real := filepath.Join(root, "real-floor")
			if err := os.Rename(dir, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, dir); err != nil {
				t.Fatal(err)
			}
		},
		"hard link": func(t *testing.T, root, marker string) {
			if err := os.Link(marker, filepath.Join(root, "second-name")); err != nil {
				t.Fatal(err)
			}
		},
		"group writable": func(t *testing.T, _, marker string) {
			if err := os.Chmod(marker, 0o620); err != nil {
				t.Fatal(err)
			}
		},
		"other writable": func(t *testing.T, _, marker string) {
			if err := os.Chmod(marker, 0o602); err != nil {
				t.Fatal(err)
			}
		},
		"another owner": func(t *testing.T, _, _ string) {
			was := geteuid
			geteuid = func() int { return was() + 1 }
			t.Cleanup(func() { geteuid = was })
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := markerRoot(t)
			marker := writeMarker(t, root, sockA, "w1:p1", "watching\n")
			if !lowered(root) {
				t.Fatal("the marker did not lower the call before it was spoiled")
			}
			spoil(t, root, marker)
			if lowered(root) {
				t.Fatalf("%s: the marker still lowered the call", name)
			}
		})
	}
}

func TestAgentModeResidentReadsTheCallersPaneAndFloor(t *testing.T) {
	root := markerRoot(t)
	writeMarker(t, root, sockA, "w1:p1", "watching\n")
	writeMarker(t, root, sockA, "w1:p9", "watching\n")
	p1, bad, a, b := "w1:p1", "../agents/w1:p1", sockA, sockB
	env := map[string]string{"COPPICE_PANE": "w1:p9", "COPPICE_SOCK": sockA}
	for _, c := range []struct {
		pane, sock *string
		want       string
	}{{&p1, &a, "audit"}, {&p1, &b, "enforce"}, {&bad, &a, "enforce"}, {nil, nil, "enforce"}, {&p1, nil, "enforce"}} {
		r := &runner{env: env, root: root, resident: true, caller: Caller{Pane: c.pane, Sock: c.sock}}
		if got := r.effectiveMode("enforce"); got != c.want {
			t.Errorf("caller %v %v: mode %q, want %q", c.pane, c.sock, got, c.want)
		}
	}
}
