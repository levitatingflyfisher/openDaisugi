package server

import (
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// A spawn that fails must leave nothing behind: no row in pane.list, no
// record in layout.json, no live entry, no stored state.
func TestAFailedSpawnRegistersNoPane(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"/","cmd_argv":["/nonexistent/binary"],"label":"ghost","kind":"pty"}`)
	resp := got[0]
	if resp.OK || resp.Error.Code != proto.ErrSpawnFailed {
		t.Fatalf("expected spawn_failed, got %+v", resp)
	}
	for _, r := range listPanes(t, s) {
		if r["label"] == "ghost" {
			t.Fatalf("ghost pane registered: %v", r)
		}
	}
	if _, err := os.Stat(layoutPath(s.cfg.DataDir)); err == nil {
		tree, err := layout.Load(layoutPath(s.cfg.DataDir))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range tree.Panes() {
			if p.Label == "ghost" {
				t.Fatal("ghost pane persisted in layout.json")
			}
		}
	}
	if _, ok := s.Live("w1:p1"); ok {
		t.Fatal("ghost pane has a live entry")
	}
	if _, ok := s.States().Current("w1:p1"); ok {
		t.Fatal("ghost pane has a stored state")
	}
}

// The failed id is spent. The next pane in the workspace gets a fresh
// number, so a client that saw the failure can never be handed the same id
// for a different process.
func TestAFailedSpawnDoesNotFreeItsPaneNumber(t *testing.T) {
	s := newPaneServer(t)
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"/","cmd_argv":["/nonexistent/binary"],"label":"ghost","kind":"pty"}`)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	if id != "w1:p2" {
		t.Fatalf("the pane after a failed spawn is %s, want w1:p2", id)
	}
}
