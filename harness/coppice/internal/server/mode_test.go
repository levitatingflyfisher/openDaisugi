package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/layout"
)

// agent.set_mode writes the per-agent marker the gate reads (F11): only
// the operator sets it, only enforcing or watching, only for a live
// harness agent, and the marker goes when the agent ends.

func modeServer(t *testing.T) *Server {
	t.Helper()
	s := newAgentServer(t)
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	if !got[0].OK {
		t.Fatalf("pane.create: %+v", got[0].Error)
	}
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	return s
}

func markerPath(s *Server, id string) string {
	sum := sha256.Sum256([]byte(s.cfg.SocketPath))
	return filepath.Join(s.cfg.GateRoot, "agents", hex.EncodeToString(sum[:])[:16], id+".mode")
}

func TestSetModeWatchingWritesTheMarkerAndEnforcingRemovesIt(t *testing.T) {
	s := modeServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"agent.set_mode","pane":"w1:p1","mode":"watching"}`)
	if !got[0].OK {
		t.Fatalf("set_mode: %+v", got[0].Error)
	}
	b, err := os.ReadFile(markerPath(s, "w1:p1"))
	if err != nil || string(b) != "watching\n" {
		t.Fatalf("marker = %q, %v", b, err)
	}
	fi, _ := os.Stat(markerPath(s, "w1:p1"))
	di, _ := os.Stat(filepath.Dir(markerPath(s, "w1:p1")))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file %v dir %v", fi.Mode(), di.Mode())
	}
	p, _ := s.tree.Pane("w1:p1")
	st := s.stackOf(p, s.viewFacts(p))
	d, _ := st["daisugi"].(map[string]any)
	if d["marker"] != "watching" || d["mode"] != modeOff {
		t.Fatalf("stack.daisugi = %v, want marker watching, mode off until a report", d)
	}
	got = roundTrip(t, s, `{"id":"2","cmd":"agent.set_mode","pane":"w1:p1","mode":"enforcing"}`)
	if !got[0].OK {
		t.Fatalf("set_mode enforcing: %+v", got[0].Error)
	}
	if _, err := os.Lstat(markerPath(s, "w1:p1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("enforcing left the marker: %v", err)
	}
}

func TestSetModeRefusesOffAndOtherWords(t *testing.T) {
	s := modeServer(t)
	for _, m := range []string{"off", "audit", "enforce", "", "WATCHING"} {
		got := roundTrip(t, s, `{"id":"1","cmd":"agent.set_mode","pane":"w1:p1","mode":"`+m+`"}`)
		if got[0].OK || got[0].Error.Code != "bad_request" || !strings.Contains(got[0].Error.Message, "daisugi gate disarm") {
			t.Fatalf("mode %q: %+v", m, got[0])
		}
	}
	if _, err := os.Lstat(markerPath(s, "w1:p1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused mode wrote a marker: %v", err)
	}
}

func TestSetModeRefusesAPaneAPluginAndAnUnplacedPeer(t *testing.T) {
	s := modeServer(t)
	for _, f := range []*peerFacts{
		{checked: true, pane: true, paneID: "w1:p1"},
		{checked: true, plugin: "p"},
		{checked: true, unknown: true},
	} {
		send, next, stop := streamFacts(t, s, f)
		send(`{"id":"1","cmd":"agent.set_mode","pane":"w1:p1","mode":"watching"}`)
		r := next()
		stop()
		e, _ := r["error"].(map[string]any)
		if r["ok"] != false || e["code"] != "unauthorized" || e["message"] != ModeRefusal {
			t.Fatalf("facts %+v: %v", f, r)
		}
	}
	if _, err := os.Lstat(markerPath(s, "w1:p1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused set_mode wrote a marker: %v", err)
	}
}

func TestSetModeRefusesAViewOnlyAttach(t *testing.T) {
	s := modeServer(t)
	fc := newFlowClient(t, s)
	fc.send(`{"id":"a","cmd":"pane.attach","pane":"w1:p1","view_only":true,"cols":5,"rows":2}`)
	fc.expectOK("a")
	fc.send(`{"id":"m","cmd":"agent.set_mode","pane":"w1:p1","mode":"watching"}`)
	m, _ := fc.replyFor("m")
	expectNotAttached(t, m, "w1:p1")
	// The view-only refusal comes before any other check (F11-R-10).
	fc.send(`{"id":"o","cmd":"agent.set_mode","pane":"w1:p1","mode":"off"}`)
	m, _ = fc.replyFor("o")
	expectNotAttached(t, m, "w1:p1")
	if _, err := os.Lstat(markerPath(s, "w1:p1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a view-only set_mode wrote a marker: %v", err)
	}
}

func TestSetModeRefusesNoPaneAndNoHarness(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"agent.set_mode","pane":"w9:p9","mode":"watching"}`,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"3","cmd":"agent.set_mode","pane":"w1:p1","mode":"watching"}`,
		`{"id":"4","cmd":"agent.set_mode","pane":"../w1:p1","mode":"watching"}`)
	if got[0].OK || got[0].Error.Code != "no_such_pane" {
		t.Fatalf("no pane: %+v", got[0])
	}
	if got[2].OK || got[2].Error.Code != "bad_request" {
		t.Fatalf("no harness: %+v", got[2])
	}
	if got[3].OK || got[3].Error.Code != "no_such_pane" {
		t.Fatalf("traversal id: %+v", got[3])
	}
}

func TestTheMarkerGoesWhenTheAgentEnds(t *testing.T) {
	s := modeServer(t)
	roundTrip(t, s, `{"id":"1","cmd":"agent.set_mode","pane":"w1:p1","mode":"watching"}`)
	if _, err := os.Lstat(markerPath(s, "w1:p1")); err != nil {
		t.Fatal(err)
	}
	if err := s.closePane("w1:p1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(markerPath(s, "w1:p1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an ended agent kept its marker: %v", err)
	}
}

func TestAStartClearsThisFloorsMarkersOnly(t *testing.T) {
	s := newTestServer(t)
	dir := filepath.Dir(markerPath(s, "w1:p1"))
	other := filepath.Join(s.cfg.GateRoot, "agents", "0123456789abcdef")
	for _, d := range []string{dir, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"w1:p1.mode", "w3:p7.mode"} {
			if err := os.WriteFile(filepath.Join(d, n), []byte("watching\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "README" {
		t.Fatalf("after start this floor's dir holds %v", left)
	}
	kept, _ := os.ReadDir(other)
	if len(kept) != 2 {
		t.Fatalf("a start cleared another floor's markers: %v", kept)
	}
}

// F11-R-8 (B3, the server): coppice writes and sweeps markers only in
// real directories. A symlink or a file where agents/ or the floor's
// directory should be is refused, and nothing is made or removed through
// it.
func TestSetModeRefusesASymlinkedMarkerDirectory(t *testing.T) {
	for _, which := range []string{"agents", "floor"} {
		t.Run(which, func(t *testing.T) {
			s := modeServer(t)
			floor := filepath.Dir(markerPath(s, "w1:p1"))
			link := floor
			if which == "agents" {
				link = filepath.Dir(floor)
			}
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"watching", "enforcing"} {
				got := roundTrip(t, s, `{"id":"1","cmd":"agent.set_mode","pane":"w1:p1","mode":"`+mode+`"}`)
				if got[0].OK || got[0].Error.Code != "internal" || !strings.Contains(got[0].Error.Message, "not a directory") {
					t.Fatalf("%s through a symlinked %s dir: %+v", mode, which, got[0])
				}
			}
			left, _ := os.ReadDir(target)
			if len(left) != 0 {
				t.Fatalf("set_mode wrote through the symlink: %v", left)
			}
		})
	}
}

func TestAStartDeletesNothingThroughASymlinkedFloorDir(t *testing.T) {
	s := newTestServer(t)
	floor := filepath.Dir(markerPath(s, "w1:p1"))
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "w1:p1.mode"), []byte("watching\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(floor), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, floor); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "w1:p1.mode")); err != nil {
		t.Fatalf("a start deleted through a symlinked floor dir: %v", err)
	}
}

// F11-R-9 (M2): the stack shows a marker only when it reads as the gate
// reads it, by fd, through no link, owned by this user with one link and
// writable by no one else.
func TestTheStackShowsOnlyAMarkerTheGateWouldRead(t *testing.T) {
	marker := func(s *Server) string {
		p, _ := s.tree.Pane("w1:p1")
		d, _ := s.stackOf(p, s.viewFacts(p))["daisugi"].(map[string]any)
		m, _ := d["marker"].(string)
		return m
	}
	for name, spoil := range map[string]func(t *testing.T, p string){
		"symlink": func(t *testing.T, p string) {
			real := filepath.Join(t.TempDir(), "real")
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		},
		"hard link": func(t *testing.T, p string) {
			if err := os.Link(p, p+".second"); err != nil {
				t.Fatal(err)
			}
		},
		"group writable": func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o620); err != nil {
				t.Fatal(err)
			}
		},
		"floor dir symlink": func(t *testing.T, p string) {
			dir := filepath.Dir(p)
			real := filepath.Join(t.TempDir(), "floor")
			if err := os.Rename(dir, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, dir); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := modeServer(t)
			roundTrip(t, s, `{"id":"1","cmd":"agent.set_mode","pane":"w1:p1","mode":"watching"}`)
			if marker(s) != "watching" {
				t.Fatal("no marker before it was spoiled")
			}
			spoil(t, markerPath(s, "w1:p1"))
			if got := marker(s); got != "" {
				t.Fatalf("%s: the stack shows marker %q", name, got)
			}
		})
	}
}

// F11-R-15 (M4): floor.facts names the hook's gate root when an installed
// hook names another one with --root, since a marker here then does
// nothing.
func TestFloorFactsNamesAHookGateRootThatDiffers(t *testing.T) {
	s := newTestServer(t)
	hookRoot := func() any {
		got := roundTrip(t, s, `{"id":"1","cmd":"floor.facts"}`)
		res, _ := got[0].Result.(map[string]any)
		d, _ := res["daisugi"].(map[string]any)
		return d["hook_root"]
	}
	if err := os.MkdirAll(filepath.Join(s.cfg.HookHome, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(cmd string) {
		body := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"` + cmd + `"}]}]}}`
		if err := os.WriteFile(filepath.Join(s.cfg.HookHome, ".claude", "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := hookRoot(); got != nil {
		t.Fatalf("no hook, hook_root %v", got)
	}
	write("python -m opendaisugi.gate --mode enforce --root " + s.cfg.GateRoot)
	if got := hookRoot(); got != nil {
		t.Fatalf("the same root, hook_root %v", got)
	}
	write("python -m opendaisugi.gate --mode enforce --root /elsewhere/gate")
	if got := hookRoot(); got != "/elsewhere/gate" {
		t.Fatalf("another root, hook_root %v", got)
	}
	write("python -m opendaisugi.gate --root=/elsewhere/gate --mode enforce")
	if got := hookRoot(); got != "/elsewhere/gate" {
		t.Fatalf("--root=, hook_root %v", got)
	}
}
