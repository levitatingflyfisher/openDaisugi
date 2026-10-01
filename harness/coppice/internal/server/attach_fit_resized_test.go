package server

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

// A resize from another client ends a phone's fit, and the presence it
// sends makes the phone attach with its fit again. That refit must not undo
// the resize: the server refuses to fit that client again until it detaches
// and attaches fresh.
func TestAResizeThatEndsAFitRefusesThatClientsRefitUntilAFreshAttach(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	phone, phoneNext, stopPhone := stream(t, s)
	defer stopPhone()
	desk, deskNext, stopDesk := stream(t, s)
	defer stopDesk()
	reply := func(next func() map[string]any, id string) map[string]any {
		t.Helper()
		for {
			if m := next(); m["id"] == id {
				return m
			}
		}
	}
	phoneCall := func(line, id string) map[string]any {
		t.Helper()
		phone(line)
		return reply(phoneNext, id)
	}
	fitted := func(m map[string]any) any {
		res, _ := m["result"].(map[string]any)
		return res["fitted"]
	}
	size := func() (int, int) {
		t.Helper()
		lp, ok := s.Live("w1:p1")
		if !ok {
			t.Fatal("the pane is gone")
		}
		return lp.Grid.Size()
	}
	phoneCall(`{"id":"c","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","exec sleep 600"],"kind":"pty","cols":40,"rows":10}`, "c")
	if m := phoneCall(`{"id":"a1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "a1"); fitted(m) != true {
		t.Fatalf("a lone fit was refused: %v", m)
	}
	desk(`{"id":"r1","cmd":"pane.resize","pane":"w1:p1","cols":60,"rows":20}`)
	if m := reply(deskNext, "r1"); m["ok"] != true {
		t.Fatalf("resize: %v", m)
	}
	if m := phoneCall(`{"id":"a2","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "a2"); fitted(m) != false {
		t.Fatalf("the refit after a resize said fitted %v, want false", fitted(m))
	}
	if c, r := size(); c != 60 || r != 20 {
		t.Fatalf("the refit undid the resize: %dx%d, want 60x20", c, r)
	}
	phoneCall(`{"id":"d1","cmd":"pane.detach","pane":"w1:p1"}`, "d1")
	if m := phoneCall(`{"id":"a3","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "a3"); fitted(m) != true {
		t.Fatalf("a fresh attach after a detach was refused its fit: %v", m)
	}
	phoneCall(`{"id":"d2","cmd":"pane.detach","pane":"w1:p1"}`, "d2")
	if c, r := size(); c != 60 || r != 20 {
		t.Fatalf("the detach put back %dx%d, want 60x20", c, r)
	}
}
