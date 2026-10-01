package server

import (
	"sync/atomic"
	"testing"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

func TestAResizeThatEndsAFitTellsPresenceOnce(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	// presenceGap runs each time the server works out who looks at a pane,
	// so it counts the presence events. Set before any pane exists.
	var told atomic.Int32
	s.presenceGap = func(string) { told.Add(1) }
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	reply := func(id string) {
		t.Helper()
		for {
			if m := next(); m["id"] == id {
				return
			}
		}
	}
	send(`{"id":"c","cmd":"pane.create","cwd":"` + t.TempDir() + `","cmd_argv":["sh","-c","exec sleep 600"],"kind":"pty","cols":40,"rows":10}`)
	reply("c")
	send(`{"id":"a","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`)
	reply("a")
	before := told.Load()
	send(`{"id":"r1","cmd":"pane.resize","pane":"w1:p1","cols":60,"rows":20}`)
	reply("r1")
	if got := told.Load() - before; got != 1 {
		t.Fatalf("a resize that ended a fit told presence %d times, want 1", got)
	}
	send(`{"id":"r2","cmd":"pane.resize","pane":"w1:p1","cols":61,"rows":21}`)
	reply("r2")
	if got := told.Load() - before; got != 1 {
		t.Fatalf("a resize with no fit to end told presence again")
	}
}
